package measure

import (
	"context"
	"encoding/json"
	"log/slog"
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

const (
	minRateDur  = 200 * time.Millisecond
	minRateMbps = 0.1 // a stalled window still counts, as very slow

	recentHalfLife = 30 * time.Second // of a node's rate now, see State
	keepVanished   = 7 * 24 * time.Hour
	recentLen      = 20
)

type Kind string

const (
	KindPing    Kind = "ping"
	KindPassive Kind = "passive"
	KindExplore Kind = "explore" // playback read through another node for a stretch
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

// State is a copy of what is known about one node, in log-ms and log-Mbps.
// Rate is its typical rate, which fades over half_life; Now is its rate over
// the next minutes: the typical rate worth one sample, pooled with its
// samples of the last minute or so. A node seen sagging half a minute ago is
// judged by the sag; one that sagged minutes ago by its typical rate again.
// Now rests on Rate's evidence, so it is doubted as much.
type State struct {
	RTT, Rate Estimate
	Now       Estimate
	OpenUntil time.Time
}

func (s State) Open(now time.Time) bool { return now.Before(s.OpenUntil) }

// Stats holds every node's estimates and breaker.
type Stats struct {
	cfg *config.Store

	mu    sync.Mutex
	nodes map[string]*entry
}

type entry struct {
	RTT     Estimate  `json:"rtt"`
	Rate    Estimate  `json:"rate"`
	Seen    time.Time `json:"seen"`
	recent  Estimate  // rate, over recentHalfLife
	breaker breaker
	samples []Sample
}

// NewStats keeps estimates for every node.
func NewStats(cfg *config.Store) *Stats {
	return &Stats{cfg: cfg, nodes: map[string]*entry{}}
}

// Record folds a sample into the node's estimates and breaker.
func (s *Stats) Record(n *nodes.Node, smp Sample) {
	smp.Node = n.ID
	if smp.Time.IsZero() {
		smp.Time = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(n)
	hl := s.cfg.Load().Control.HalfLife
	e.Seen = smp.Time
	e.samples = append(e.samples, smp)
	if len(e.samples) > recentLen {
		e.samples = e.samples[1:]
	}
	if e.breaker.record(smp.Err == "", smp.Time) {
		slog.Warn("breaker open", "node", n.Name, "until", e.breaker.until.Format(time.TimeOnly), "err", smp.Err)
	}
	if smp.Err != "" {
		return
	}
	if smp.Kind == KindPing && smp.TTFB > 0 {
		e.RTT.Observe(math.Log(float64(smp.TTFB)/float64(time.Millisecond)), smp.Time, hl)
	}
	if r := smp.Mbps(); r > 0 {
		e.Rate.Observe(math.Log(r), smp.Time, hl)
		e.recent.Observe(math.Log(r), smp.Time, recentHalfLife)
	}
}

// Healthy notes a successful request, closing the node's breaker without
// logging a sample: control traffic is too chatty to log.
func (s *Stats) Healthy(n *nodes.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(n).breaker.record(true, time.Now())
}

// State returns the node's estimates as of now.
func (s *Stats) State(n *nodes.Node) State { return s.StateAt(n, time.Now()) }

// StateAt returns the node's estimates as of a given time.
func (s *Stats) StateAt(n *nodes.Node, now time.Time) State {
	hl := s.cfg.Load().Control.HalfLife
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(n)
	rate := e.Rate.AsOf(now, hl)
	typical := rate
	typical.Weight = min(typical.Weight, 1)
	live := typical.with(e.recent.AsOf(now, recentHalfLife))
	live.Weight = rate.Weight
	return State{
		RTT:       e.RTT.AsOf(now, hl),
		Rate:      rate,
		Now:       live,
		OpenUntil: e.breaker.until,
	}
}

// Open reports whether n's breaker is open.
func (s *Stats) Open(n *nodes.Node, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.nodes[n.ID]
	return e != nil && now.Before(e.breaker.until)
}

// Recent returns n's latest samples, oldest first.
func (s *Stats) Recent(n *nodes.Node) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.nodes[n.ID]; e != nil {
		return slices.Clone(e.samples)
	}
	return nil
}

func (s *Stats) entry(n *nodes.Node) *entry {
	if e := s.nodes[n.ID]; e != nil {
		return e
	}
	e := &entry{}
	s.nodes[n.ID] = e
	return e
}

// Persist restores estimates from path, then saves them every 5 min and on
// exit. Recent samples, which only the pane shows, start empty.
func (s *Stats) Persist(ctx context.Context, path string) {
	if err := s.Restore(path); err != nil {
		slog.Warn("could not restore estimates; starting fresh", "err", err)
	}
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
		if err := s.Save(path); err != nil {
			slog.Error("could not save estimates", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Save writes estimates to path, dropping nodes unseen for 7 days.
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

// Restore loads estimates saved by Save; a missing file is not an error.
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
