// Package control assigns the two roles, primary and media, from each node's
// measured rate and RTT; the proxy asks for a role, never a node. Every
// choice it makes is logged at info, with what it weighed, so a deployment's
// logs show the strategy at work.
package control

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
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
	primaryMargin = 100.0 // ms a known node's burst must be quicker by to take the primary, see burstMs
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

	mu      sync.Mutex
	primary *nodes.Node
	plays   map[string]*Playback
	refused time.Time // exploration paused until
	probing bool      // a test is running
	peak    float64   // highest bitrate played within peakMemory
	peakAt  time.Time
}

func New(cfg *config.Store, pool *nodes.Pool, stats *measure.Stats) *Controller {
	return &Controller{
		cfg:   cfg,
		pool:  pool,
		stats: stats,
		base:  cfg.Load().Upstream.Base(),
		plays: map[string]*Playback{},
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
		slog.Info("primary switched", "from", c.primary, "to", n.Name,
			"from_burst_ms", c.burstLog(c.primary), "to_burst_ms", c.burstLog(n))
	}
	c.primary = n
}

// reconsiderPrimary runs after each ping round. Another node takes over only
// when nothing is playing, its RTT is known, and its burst is primaryMargin
// quicker.
func (c *Controller) reconsiderPrimary() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.primaryGone() {
		c.setPrimary(c.quickest(c.usable(nil)))
		return
	}
	best := c.quickest(c.usable(func(n *nodes.Node) bool { return n != c.primary && c.stats.State(n).RTT.Known() }))
	if best != nil && !c.playing() && c.burst(best)+primaryMargin < c.burst(c.primary) {
		c.setPrimary(best)
	}
}

func (c *Controller) pingAll(ctx context.Context) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	start := time.Now()
	var ok atomic.Int32
	all := c.pool.All()
	for _, n := range all {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if s, err := measure.Ping(ctx, n, c.base); err == nil {
				c.stats.Record(n, s)
				if s.Err == "" {
					ok.Add(1)
				}
			}
		})
	}
	wg.Wait()
	slog.Info("pinged", "nodes", len(all), "ok", ok.Load(), "took_s", round1(time.Since(start).Seconds()))
	c.reconsiderPrimary()
}

// typicalMbps is the bitrate to judge at when none is known: the highest
// played lately. Caller holds c.mu.
func (c *Controller) typicalMbps() float64 {
	if c.peak == 0 {
		return firstRunMbps
	}
	return c.peak
}

// playing reports whether any session has a stream. Caller holds c.mu.
func (c *Controller) playing() bool {
	for _, p := range c.plays {
		if len(p.streams) > 0 {
			return true
		}
	}
	return false
}

func (c *Controller) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.plays {
		if len(p.streams) == 0 && time.Since(p.idle) > playLinger {
			delete(c.plays, key)
			slog.Info("playback ended", "session", key, "media", p.node.Name, "failovers", p.failovers,
				"tests", p.tests, "minutes", round1(p.idle.Sub(p.started).Minutes()))
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

func (c *Controller) open(n *nodes.Node) bool { return c.stats.Open(n, time.Now()) }

func (c *Controller) pinned(ref string) *nodes.Node {
	if n := c.pool.Find(ref); n != nil && !c.open(n) {
		return n
	}
	return nil
}

// quickest is the node with the shortest burst time.
func (c *Controller) quickest(ns []*nodes.Node) *nodes.Node {
	return minBy(ns, c.burst)
}

func (c *Controller) burst(n *nodes.Node) float64 { return burstMs(c.stats.State(n)) }

// burstLog is n's burst time for the logs: -1 for none.
func (c *Controller) burstLog(n *nodes.Node) float64 {
	if n == nil {
		return -1
	}
	if b := c.burst(n); b < 1e9 {
		return round1(b)
	}
	return -1
}

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
