// Package control is the adaptive controller: observe → update beliefs →
// predict stall risk → take the cheapest safe action. It assigns the three
// roles (primary, media, standby); the proxy asks for a role, never a node.
package control

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"math"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

const (
	primaryMargin = 100 * time.Millisecond // on a burst, see burstTime
	primaryConf   = 0.95
	primaryRounds = 2
	activeWindow  = 10 * time.Minute
	playLinger    = 2 * time.Minute // keeps a session's nodes across seeks
	firstRunMbps  = 40.0            // the bitrate to judge at before any session reports one
	peakMemory    = 7 * 24 * time.Hour
)

var ErrNoNode = errors.New("no usable node")

var failovers = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "embolt_failovers_total", Help: "Media node switches, by reason.",
}, []string{"reason"})

type Controller struct {
	cfg   *config.Store
	pool  *nodes.Pool
	stats *measure.Stats
	base  *url.URL

	lastActive atomic.Int64

	mu         sync.Mutex
	primary    *nodes.Node
	challenger *nodes.Node
	wins       int
	plays      map[string]*Playback
	refused    time.Time            // exploration paused until
	tested     map[string]time.Time // node ID → last explored, for the exit-IP cap
	peak       float64              // highest bitrate played within peakMemory
	peakAt     time.Time
}

func New(cfg *config.Store, pool *nodes.Pool, stats *measure.Stats) *Controller {
	return &Controller{
		cfg:    cfg,
		pool:   pool,
		stats:  stats,
		base:   cfg.Load().Upstream.Base(),
		plays:  map[string]*Playback{},
		tested: map[string]time.Time{},
	}
}

// Touch marks a client as active, which speeds up pinging.
func (c *Controller) Touch() { c.lastActive.Store(time.Now().UnixNano()) }

func (c *Controller) active() bool {
	return time.Since(time.Unix(0, c.lastActive.Load())) < activeWindow
}

// Run pings on schedule and expires idle playbacks. Rates are learned from
// playback alone: its own samples, and the stretches sessions explore.
func (c *Controller) Run(ctx context.Context) {
	ping := time.NewTimer(0)
	expire := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	defer expire.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.pool.Updated():
			c.pingAll(ctx)
			ping.Reset(c.pingInterval())
		case <-ping.C:
			c.pingAll(ctx)
			ping.Reset(c.pingInterval())
		case <-expire.C:
			c.expire()
		}
	}
}

func (c *Controller) pingInterval() time.Duration {
	switch {
	case len(c.usable(nil)) == 0:
		return 30 * time.Second
	case c.active():
		return 10 * time.Minute
	default:
		return time.Hour
	}
}

// Refused pauses exploration for 1 h: the server answered an explored
// stretch with 403 or 429.
func (c *Controller) Refused() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused = time.Now().Add(time.Hour)
	slog.Warn("server refused an explored node; pausing exploration for 1 h")
}

// Primary is the node for control traffic: the quickest to deliver a burst
// of browsing, which weighs speed as well as RTT; sticky.
func (c *Controller) Primary() (*nodes.Node, error) {
	if n := c.pinned(c.cfg.Load().Pins.Primary); n != nil {
		return n, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primaryGone() {
		c.setPrimary(c.quickest(c.usable(nil)))
	}
	if c.primary == nil {
		return nil, ErrNoNode
	}
	return c.primary, nil
}

// primaryGone reports whether there is no primary, or it left the pool or
// tripped its breaker.
func (c *Controller) primaryGone() bool {
	return c.primary == nil || c.pool.Get(c.primary.ID) == nil || c.open(c.primary)
}

func (c *Controller) setPrimary(n *nodes.Node) {
	if n != nil && n != c.primary {
		slog.Info("primary switched", "from", c.primary, "to", n.Name)
	}
	c.primary, c.challenger, c.wins = n, nil, 0
}

// reconsiderPrimary runs after each ping round. A challenger takes over only
// when P(its burst is 100 ms quicker) ≥ 95% for 2 rounds and nothing is
// playing.
func (c *Controller) reconsiderPrimary() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primaryGone() {
		c.setPrimary(c.quickest(c.usable(nil)))
		return
	}
	best := c.quickest(c.usable(func(n *nodes.Node) bool { return n != c.primary }))
	if best == nil || probFaster(c.burst(best), c.burst(c.primary), primaryMargin) < primaryConf {
		c.challenger, c.wins = nil, 0
		return
	}
	if best != c.challenger {
		c.challenger, c.wins = best, 0
	}
	if c.wins++; c.wins >= primaryRounds && len(c.busy()) == 0 {
		c.setPrimary(best)
	}
}

func (c *Controller) pingAll(ctx context.Context) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, n := range c.pool.All() {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if s, err := measure.Ping(ctx, n, c.base); err == nil {
				c.stats.Record(n, s)
			}
		})
	}
	wg.Wait()
	c.reconsiderPrimary()
}

// minProbeValue is the least expected gain, in seconds of stall, worth a test.
const minProbeValue = 0.01

// ranked orders the nodes worth a test, most valuable first: the knowledge
// gradient of a test, how far the best expected stall time over usable nodes
// would drop once its result is known (probeValue). A node far above the
// bitrate, or far worse than the best, is worth nearly nothing.
func (c *Controller) ranked(busy map[*nodes.Node]bool, bitrate float64, now time.Time) []*nodes.Node {
	worth := c.probeValues(c.usable(nil), busy, bitrate, now)
	ranked := slices.Collect(maps.Keys(worth))
	slices.SortFunc(ranked, func(a, b *nodes.Node) int { return cmp.Compare(worth[b], worth[a]) })
	return ranked
}

// probeValues scores a test of each usable node that no stream is
// measuring already, keeping those worth at least minProbeValue. Choices are
// judged at play start: B = 0 at the given bitrate.
func (c *Controller) probeValues(usable []*nodes.Node, busy map[*nodes.Node]bool, bitrate float64, now time.Time) map[*nodes.Node]float64 {
	cfg := c.cfg.Load().Control
	rates := make(map[*nodes.Node]measure.Belief, len(usable))
	stall := make(map[*nodes.Node]float64, len(usable))
	for _, n := range usable {
		rates[n] = c.stats.StateAt(n, now).Rate
		stall[n] = expectedStall(rates[n], 0, bitrate)
	}
	worth := map[*nodes.Node]float64{}
	for _, n := range usable {
		if busy[n] {
			continue
		}
		best := math.Inf(1) // over the other nodes
		for _, m := range usable {
			if m != n {
				best = min(best, stall[m])
			}
		}
		if v := probeValue(cfg, rates[n], best, bitrate, now); v >= minProbeValue {
			worth[n] = v
		}
	}
	return worth
}

// probeValue is the knowledge gradient of one test of a node with belief
// b, when the best other node stalls for best seconds: the expected drop in
// min(best, the node's stall), over 16 equally likely outcomes of the test.
func probeValue(cfg config.Control, b measure.Belief, best, bitrate float64, now time.Time) float64 {
	const outcomes = 16
	before := min(best, expectedStall(b, 0, bitrate))
	pred := b.Predictive()
	gain := 0.0
	for i := range outcomes {
		after := b
		after.Observe(pred.Quantile((float64(i)+0.5)/outcomes), now, cfg.HalfLife)
		gain += before - min(best, expectedStall(after, 0, bitrate))
	}
	return gain / outcomes
}

// firstWithinIPCap takes the first ranked node that keeps the distinct exit
// IPs the server sees on the player's token within max_new_ips_per_hour, and
// counts it. Caller holds c.mu.
func (c *Controller) firstWithinIPCap(ranked []*nodes.Node, now time.Time) *nodes.Node {
	recent := 0
	for id, t := range c.tested {
		if now.Sub(t) > time.Hour {
			delete(c.tested, id)
		} else {
			recent++
		}
	}
	for _, n := range ranked {
		if _, seen := c.tested[n.ID]; seen || recent < c.cfg.Load().Probes.MaxNewIPsPerHour {
			c.tested[n.ID] = now
			return n
		}
	}
	return nil
}

// typicalMbps is the bitrate to judge at when none is known: the highest
// played lately. Caller holds c.mu.
func (c *Controller) typicalMbps() float64 {
	if c.peak == 0 {
		return firstRunMbps
	}
	return c.peak
}

func (c *Controller) busy() map[*nodes.Node]bool {
	b := map[*nodes.Node]bool{}
	for _, p := range c.plays {
		if len(p.streams) > 0 {
			b[p.node] = true
		}
	}
	return b
}

func (c *Controller) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.plays {
		if len(p.streams) == 0 && time.Since(p.idle) > playLinger {
			delete(c.plays, key)
			slog.Info("playback ended", "session", key, "failovers", p.failovers)
		}
	}
}

// usable lists nodes whose breaker is closed and that pass keep (nil = all).
func (c *Controller) usable(keep func(*nodes.Node) bool) []*nodes.Node {
	var out []*nodes.Node
	for _, n := range c.pool.All() {
		if (keep == nil || keep(n)) && !c.open(n) {
			out = append(out, n)
		}
	}
	return out
}

func (c *Controller) open(n *nodes.Node) bool { return c.stats.State(n).Open(time.Now()) }

func (c *Controller) pinned(ref string) *nodes.Node {
	if n := c.pool.Find(ref); n != nil && !c.open(n) {
		return n
	}
	return nil
}

// lowestRTT ranks by the 90% upper bound of the typical RTT.
func (c *Controller) lowestRTT(ns []*nodes.Node) *nodes.Node {
	return minBy(c.rttMeasured(ns), c.rttKey)
}

// quickest ranks by the 90% upper bound of the burst time. A node with no
// speed data keeps a wide pooled prior, so it ranks behind a measured node of
// the same speed.
func (c *Controller) quickest(ns []*nodes.Node) *nodes.Node {
	return minBy(c.rttMeasured(ns), func(n *nodes.Node) float64 { return c.burst(n).upper() })
}

// rttMeasured keeps the nodes with a measured RTT, if there are any, so a
// guess never beats a measurement.
func (c *Controller) rttMeasured(ns []*nodes.Node) []*nodes.Node {
	measured := slices.DeleteFunc(slices.Clone(ns), func(n *nodes.Node) bool { return !c.stats.State(n).RTT.Measured() })
	if len(measured) > 0 {
		return measured
	}
	return ns
}

func (c *Controller) burst(n *nodes.Node) timing {
	st := c.stats.State(n)
	return burstTime(st.RTT, st.Rate)
}

// rttKey is the 90% upper bound of a node's typical RTT: low only when the
// node is both fast and well measured.
func (c *Controller) rttKey(n *nodes.Node) float64 { return c.stats.State(n).RTT.Mean().Quantile(0.9) }

func minBy[T any](xs []T, key func(T) float64) T {
	var best T
	bestKey := 0.0
	for i, x := range xs {
		if k := key(x); i == 0 || k < bestKey {
			best, bestKey = x, k
		}
	}
	return best
}
