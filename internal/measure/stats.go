package measure

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"math"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/nodes"
)

// Window is the length of a passive rate sample.
const Window = 2 * time.Second

// ProbeWeight is what a speed test counts for against a playback sample.
const ProbeWeight = 0.5

const (
	minRateDur  = 200 * time.Millisecond
	minRateMbps = 0.1 // a stalled window still counts, as very slow

	keepVanished = 7 * 24 * time.Hour
	recentLen    = 20
)

// prior is a log-scale centre and spread.
type prior struct{ mu, sigma float64 }

// Fallback priors when no node has data yet: 20 Mbps and 150 ms, wide.
var (
	defaultRate = prior{math.Log(20), 1.0}
	defaultRTT  = prior{math.Log(150), 0.6}
)

type Kind string

const (
	KindPing    Kind = "ping"
	KindSpeed   Kind = "speed"
	KindPassive Kind = "passive"
)

// Sample is one measurement of a node's route to the Emby server.
type Sample struct {
	Node  string        `json:"node"`
	Time  time.Time     `json:"time"`
	Kind  Kind          `json:"kind"`
	Bytes int64         `json:"bytes,omitempty"`
	Dur   time.Duration `json:"dur,omitempty"`  // transfer time after the first byte
	TTFB  time.Duration `json:"ttfb,omitempty"` // a ping's TTFB on a warm connection is its RTT
	Err   string        `json:"err,omitempty"`  // node faults only: connect errors and timeouts
}

// Mbps is the sample's transfer rate, or 0 if it has none: a failed sample,
// a ping, or a transfer too short to time.
func (s Sample) Mbps() float64 {
	if s.Err != "" || s.Kind == KindPing || s.Dur < minRateDur {
		return 0
	}
	return max(float64(s.Bytes)*8/s.Dur.Seconds()/1e6, minRateMbps)
}

// State is a copy of what is known about one node.
type State struct {
	RTT, Rate Belief
	OpenUntil time.Time
	Recent    []Sample
}

func (s State) Open(now time.Time) bool { return now.Before(s.OpenUntil) }

// Stats holds every node's beliefs and breaker, and logs every sample.
type Stats struct {
	cfg *config.Store
	log *sampleLog

	mu    sync.Mutex
	nodes map[string]*entry
}

type entry struct {
	Provider string    `json:"provider"`
	RTT      Belief    `json:"rtt"`
	Rate     Belief    `json:"rate"`
	Seen     time.Time `json:"seen"`
	breaker  breaker
	recent   []Sample
}

// NewStats keeps beliefs for every node and appends samples to sampleDir,
// unless it is empty.
func NewStats(cfg *config.Store, sampleDir string) *Stats {
	s := &Stats{cfg: cfg, nodes: map[string]*entry{}}
	if sampleDir != "" {
		s.log = &sampleLog{dir: sampleDir}
	}
	return s
}

// Record folds a sample into the node's beliefs and breaker.
func (s *Stats) Record(n *nodes.Node, smp Sample) {
	smp.Node = n.ID
	if smp.Time.IsZero() {
		smp.Time = time.Now()
	}
	if s.log != nil {
		s.log.write(smp)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(n)
	hl := s.cfg.Load().Control.HalfLife
	e.Seen = smp.Time
	e.recent = append(e.recent, smp)
	if len(e.recent) > recentLen {
		e.recent = e.recent[1:]
	}
	if e.breaker.record(smp.Err == "", smp.Time) {
		slog.Warn("breaker open", "node", n.Name, "until", e.breaker.until.Format(time.TimeOnly), "err", smp.Err)
	}
	if smp.Err != "" {
		return
	}
	if smp.Kind == KindPing && smp.TTFB > 0 {
		e.RTT.Observe(math.Log(float64(smp.TTFB)/float64(time.Millisecond)), 1, smp.Time, hl)
	}
	if r := smp.Mbps(); r > 0 {
		w := 1.0
		if smp.Kind != KindPassive {
			w = ProbeWeight
		}
		e.Rate.Observe(math.Log(r), w, smp.Time, hl)
	}
}

// Healthy notes a successful request, closing the node's breaker without
// logging a sample: control traffic is too chatty to log.
func (s *Stats) Healthy(n *nodes.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(n).breaker.record(true, time.Now())
}

// State returns the node's beliefs as of now; a node never seen starts from
// its provider's pooled belief.
func (s *Stats) State(n *nodes.Node) State { return s.StateAt(n, time.Now()) }

// StateAt returns the node's beliefs as of a given time.
func (s *Stats) StateAt(n *nodes.Node, now time.Time) State {
	hl := s.cfg.Load().Control.HalfLife
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(n)
	return State{
		RTT:       e.RTT.AsOf(now, hl),
		Rate:      e.Rate.AsOf(now, hl),
		OpenUntil: e.breaker.until,
		Recent:    append([]Sample(nil), e.recent...),
	}
}

func (s *Stats) entry(n *nodes.Node) *entry {
	if e := s.nodes[n.ID]; e != nil {
		return e
	}
	strength := s.cfg.Load().Control.PriorStrength
	e := &entry{
		Provider: n.Provider,
		RTT:      s.pooled(n.Provider, func(e *entry) Belief { return e.RTT }, defaultRTT, strength),
		Rate:     s.pooled(n.Provider, func(e *entry) Belief { return e.Rate }, defaultRate, strength),
	}
	s.nodes[n.ID] = e
	return e
}

// pooled builds a prior worth strength samples from the measured nodes of
// the same provider, else of all providers, else the fallback.
func (s *Stats) pooled(provider string, get func(*entry) Belief, fallback prior, strength float64) Belief {
	for _, sameProvider := range []bool{true, false} {
		var mus, vars []float64
		for _, e := range s.nodes {
			b := get(e)
			if b.Measured() && (!sameProvider || e.Provider == provider) {
				mus = append(mus, b.Mu)
				vars = append(vars, b.Beta/b.Alpha)
			}
		}
		if len(mus) > 0 {
			mu, within := mean(mus), mean(vars)
			between := 0.0
			for _, m := range mus {
				between += (m - mu) * (m - mu) / float64(len(mus))
			}
			return NewBelief(mu, math.Sqrt(within+between), strength)
		}
	}
	return NewBelief(fallback.mu, fallback.sigma, strength)
}

func mean(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// Persist restores beliefs from path and recent samples from the sample log,
// then saves beliefs every 5 min and on exit.
func (s *Stats) Persist(ctx context.Context, path string) {
	if err := s.Restore(path); err != nil {
		slog.Warn("could not restore beliefs; starting fresh", "err", err)
	}
	if err := s.restoreRecent(); err != nil {
		slog.Warn("could not restore recent samples", "err", err)
	}
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
		if err := s.Save(path); err != nil {
			slog.Error("could not save beliefs", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Save writes beliefs to path, dropping nodes unseen for 7 days.
func (s *Stats) Save(path string) error {
	s.mu.Lock()
	for id, e := range s.nodes {
		if time.Since(e.Seen) > keepVanished {
			delete(s.nodes, id)
		}
	}
	raw, err := json.Marshal(s.nodes)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Restore loads beliefs saved by Save; a missing file is not an error.
func (s *Stats) Restore(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(raw, &s.nodes)
}

// restoreRecent fills each known node's recent samples from the sample log,
// which outlives a restart. The log is read without the lock; samples that
// Record took in meanwhile stay, and only older logged ones go before them.
func (s *Stats) restoreRecent() error {
	if s.log == nil {
		return nil
	}
	s.mu.Lock()
	ids := slices.Collect(maps.Keys(s.nodes))
	s.mu.Unlock()
	found, err := lastSamples(s.log.dir, ids, recentLen)

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, logged := range found {
		e := s.nodes[id]
		if e == nil {
			continue
		}
		if len(e.recent) > 0 {
			first := e.recent[0].Time
			logged = slices.DeleteFunc(logged, func(smp Sample) bool { return !smp.Time.Before(first) })
		}
		all := append(logged, e.recent...)
		e.recent = all[max(0, len(all)-recentLen):]
	}
	return err
}

// breaker opens after 3 consecutive node faults, for 30 s doubling to 10 min.
// After it closes, a single fault reopens it; a success resets it.
type breaker struct {
	fails   int
	backoff time.Duration
	until   time.Time
}

func (b *breaker) record(ok bool, now time.Time) (tripped bool) {
	if ok {
		*b = breaker{}
		return false
	}
	if now.Before(b.until) {
		return false
	}
	if b.fails++; b.fails < 3 {
		return false
	}
	b.backoff = min(max(2*b.backoff, 30*time.Second), 10*time.Minute)
	b.until = now.Add(b.backoff)
	b.fails = 2
	return true
}
