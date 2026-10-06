package control

import (
	"cmp"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
	"github.com/zhousiru/embolt/internal/view"
)

const (
	minSwitchGap = 20 * time.Second // after a switch, before the next
	riskAhead    = 20 * time.Second // a read-ahead under this lets the node fall behind...
	maxDeficit   = 3 * time.Second  // ...by this much media, and the session moves
	stepGap      = 2 * time.Second  // between steps, as the proxy runs them
	headroom     = 1.2              // over the bitrate, for the primary to take a new session
	migrateGap   = 5 * time.Minute  // the least time on a node before a move for speed
	migrateGain  = 1.5              // how much faster a node must be to take a session that keeps up
	retestAfter  = 30 * time.Minute // a rate this recent is fresh: not tested again, and fit to move for
	minTestGap   = 30 * time.Second // between a session's tests, however few bytes they read
	logEvery     = 30 * time.Second // of a session's state
)

// A test reads up to ProbeBytes through an explored node, beside the media
// node, for up to ProbeTime.
const (
	ProbeBytes = 32 << 20
	ProbeTime  = 8 * time.Second
)

// Playback is one viewing session: one device watching one item. It holds
// the media node and the controller's view of the read-ahead. The player's
// own buffer is unknown and never guessed at: a read-ahead that runs out
// is not a stall, but a node behind the bitrate drains the player.
// Seeks and the player's side connections join the same Playback, so the
// session keeps one exit IP.
// A playback lasts while it has a stream: a file it reads from upstream,
// which any number of player connections read.
type Playback struct {
	c       *Controller
	key     string
	started time.Time

	// Guarded by c.mu.
	bitrate   float64
	node      *nodes.Node
	switched  time.Time
	nextTest  time.Time // the earliest the session may test another node
	tests     int
	failovers int
	streams   map[*Stream]struct{}
	viewers   int // player connections
	idle      time.Time
	ahead     time.Duration
	full      bool          // the read-ahead filled since the last step
	deficit   time.Duration // media the node fell short of the bitrate while the read-ahead was thin
	logged    time.Time

	// What the pane shows, as of the last step.
	state   string        // starting, ok, risk or low
	fetched float64       // Mbps read from upstream over the last step
	low     time.Duration // behind with the read-ahead under the low mark
	stepped time.Time
	history []view.Point // the last historyLen steps
}

// historyLen steps make the pane's 10 min of a session.
const historyLen = 300

// Stream is one file a playback reads from upstream, judged as a whole: its
// observation covers every region the player reads.
type Stream struct {
	*Playback
	played float64 // bytes handed to players lately; guarded by c.mu
}

// Observation is what a stream reports at each step.
type Observation struct {
	ReadAhead time.Duration // media in Embolt's read-ahead
	Played    float64       // bytes handed to players lately, decaying
	Full      bool          // the read-ahead filled since the last step
	Fetched   float64       // Mbps read from upstream since the last step
}

// owe updates the session's deficit: the media its node fell short of the
// bitrate over the step, less what it delivered over, never under zero. A
// player filling its own buffer takes all it is handed and leaves the
// read-ahead thin, so the read-ahead alone says nothing; what the node
// delivers does. One step's rate swings widely, so the shortfall adds up
// over steps. A full or long read-ahead, or a fresh switch, clears it: each
// node answers for its own delivery.
func (p *Playback) owe(o Observation, now time.Time) {
	dt := stepGap
	if !p.stepped.IsZero() {
		dt = min(now.Sub(p.stepped), 2*stepGap)
	}
	if o.Full || o.ReadAhead >= riskAhead || now.Sub(p.switched) < minSwitchGap {
		p.deficit = 0
		return
	}
	p.deficit = max(0, p.deficit+time.Duration(float64(dt)*(1-o.Fetched/p.bitrate)))
}

// behind reports whether the session's node has fallen maxDeficit behind.
func (p *Playback) behind() bool { return p.deficit >= maxDeficit }

// Play joins or starts the session key with a new stream. bitrate is in
// Mbps, 0 if unknown. Callers must Release the stream when it ends.
func (c *Controller) Play(key string, bitrate float64) (*Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if bitrate <= 0 {
		bitrate = c.typicalMbps()
	} else if bitrate >= c.peak || now.Sub(c.peakAt) > peakMemory {
		c.peak, c.peakAt = bitrate, now
	}
	p := c.plays[key]
	if p == nil {
		n, why, ranked := c.pickMedia(bitrate)
		if n == nil {
			return nil, ErrNoNode
		}
		p = &Playback{c: c, key: key, started: now, bitrate: bitrate, node: n, streams: map[*Stream]struct{}{}}
		c.plays[key] = p
		slog.Info("playback started", "session", key, "media", n.Name, "bitrate_mbps", round1(bitrate),
			"why", why, "options", top(ranked))
	}
	p.bitrate = bitrate
	s := &Stream{Playback: p}
	p.streams[s] = struct{}{}
	return s, nil
}

// Watch counts a player connection reading the playback until done is
// called.
func (p *Playback) Watch() (done func()) {
	p.c.mu.Lock()
	p.viewers++
	p.c.mu.Unlock()
	return func() {
		p.c.mu.Lock()
		p.viewers--
		p.c.mu.Unlock()
	}
}

func (s *Stream) Release() {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if delete(s.streams, s); len(s.streams) == 0 {
		s.idle = time.Now()
	}
}

// leads reports whether s drives the session: it delivered the most lately.
func (s *Stream) leads() bool {
	for o := range s.streams {
		if o.played > s.played {
			return false
		}
	}
	return true
}

func (p *Playback) Key() string { return p.key }

func (p *Playback) Node() *nodes.Node {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return p.node
}

func (p *Playback) Bitrate() float64 {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return p.bitrate
}

// pickMedia chooses at play start and says why: the primary when its rate
// has headroom over the bitrate, so browsing and playback share one exit
// IP; else the fastest node. It returns the nodes it ranked, for the logs.
func (c *Controller) pickMedia(bitrate float64) (*nodes.Node, string, []ranked) {
	if n := c.pinned(c.cfg.Load().Pins.Media); n != nil {
		return n, "pinned", nil
	}
	r := c.rank(c.usable(nil))
	if len(r) == 0 {
		return nil, "", nil
	}
	if c.primary != nil && !c.open(c.primary) {
		if st := c.stats.State(c.primary); st.Rate.Measured() && st.Rate.Value >= headroom*bitrate {
			return c.primary, "the primary is fast enough", r
		}
	}
	if !r[0].st.Rate.Measured() {
		return r[0].n, "nothing measured; lowest RTT", r
	}
	return r[0].n, "fastest", r
}

// Step is the controller loop for one session, called every 2 s by each of
// its streams; only the lead stream decides. It returns a node to switch to
// and why ("risk" or "faster"), or nil to stay. See decide.
func (s *Stream) Step(o Observation) (*nodes.Node, string) {
	p, c := s.Playback, s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.played = o.Played; !s.leads() {
		return nil, ""
	}
	now := time.Now()
	p.ahead, p.full = o.ReadAhead, o.Full
	p.owe(o, now)
	to, why, faster := c.decide(p, now)
	p.record(o, now)
	rate := c.stats.State(p.node).Rate
	if to == nil {
		if now.Sub(p.logged) >= logEvery {
			p.logged = now
			slog.Info("session", "session", p.key, "media", p.node.Name, "ahead_s", round1(p.ahead.Seconds()),
				"deficit_s", round1(p.deficit.Seconds()), "full", p.full, "fetched_mbps", round1(o.Fetched), "node_mbps", mbpsLog(rate),
				"bitrate_mbps", round1(p.bitrate), "verdict", why)
		}
		return nil, ""
	}
	reason := "risk"
	if faster {
		reason = "faster"
	}
	slog.Info("move", "session", p.key, "from", p.node.Name, "to", to.Name, "reason", reason, "why", why,
		"ahead_s", round1(p.ahead.Seconds()), "deficit_s", round1(p.deficit.Seconds()), "fetched_mbps", round1(o.Fetched),
		"bitrate_mbps", round1(p.bitrate), "node_mbps", mbpsLog(rate), "options", top(c.rank(c.usable(other(p.node)))))
	p.deficit = 0 // the next node starts afresh, and the move is not asked again before it lands
	return to, reason
}

// record keeps what the step saw for the pane.
func (p *Playback) record(o Observation, now time.Time) {
	under := p.behind() && p.ahead < LowMark
	if under && !p.stepped.IsZero() {
		p.low += now.Sub(p.stepped)
	}
	switch {
	case under:
		p.state = "low"
	case p.behind():
		p.state = "risk"
	default:
		p.state = "ok"
	}
	p.fetched, p.stepped = o.Fetched, now
	if len(p.history) == historyLen {
		p.history = slices.Delete(p.history, 0, 1)
	}
	p.history = append(p.history, view.Point{At: now, Ahead: p.ahead.Seconds(), Mbps: o.Fetched})
}

// decide moves a session, or keeps it, and says why:
//
//  1. Stay on a pinned node, or on one switched to moments ago.
//  2. When the node has fallen maxDeficit behind the bitrate, move to the
//     fastest other node, unless it is no faster than the media node: then
//     the server, not the node, is slow.
//  3. When the session has been on its node for migrateGap and keeps up,
//     move to a node whose fresh rate is migrateGain times the media
//     node's.
//  4. Else stay.
//
// Hard failures, no byte for 4 s or a connection error, never wait for a
// step: see Failover.
func (c *Controller) decide(p *Playback, now time.Time) (to *nodes.Node, why string, faster bool) {
	switch {
	case c.pinned(c.cfg.Load().Pins.Media) == p.node:
		return nil, "pinned", false
	case now.Sub(p.switched) < minSwitchGap:
		return nil, "switched moments ago", false
	case p.behind():
		return c.rescue(p)
	case now.Sub(p.started) < migrateGap || now.Sub(p.switched) < migrateGap:
		return nil, "keeps up", false
	}
	cur := c.stats.State(p.node).Rate
	for _, o := range c.rank(c.usable(other(p.node))) {
		if o.st.Rate.Measured() && now.Sub(o.st.Rate.At) < retestAfter {
			if cur.Measured() && o.st.Rate.Value >= migrateGain*cur.Value {
				return o.n, "a node tested lately is far faster", true
			}
			break // the fastest fresh node is not far faster, so none is
		}
	}
	return nil, "keeps up", false
}

// rescue moves a session that is behind to the fastest other node, unless
// that node is no faster than its own.
func (c *Controller) rescue(p *Playback) (*nodes.Node, string, bool) {
	r := c.rank(c.usable(other(p.node)))
	cur := c.stats.State(p.node)
	switch {
	case len(r) == 0:
		return nil, "behind; no other usable node", false
	case cur.Rate.Measured() && faster(r[0].st, cur) >= 0:
		return nil, "behind; no faster node", false
	}
	return r[0].n, "behind", false
}

// other keeps every node but n.
func other(n *nodes.Node) func(*nodes.Node) bool {
	return func(x *nodes.Node) bool { return x != n }
}

// Explore picks a node to test beside the session's media node, or nil: a
// speed test whose bytes are dropped, and the only test Embolt runs. It
// tests every node in turn, the one sampled longest ago first and a node
// never measured before all, ties to the lowest RTT; a node sampled within
// retestAfter waits.
//
// Tests run one at a time, and a session spends on them at most
// probes.budget of its bitrate: after a test, the next waits until the
// session has played its bytes over budget, and at least minTestGap. The
// caller reports what a test read with Probed.
func (s *Stream) Explore() *nodes.Node {
	p, c := s.Playback, s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	cfg := c.cfg.Load()
	if !s.leads() || cfg.Probes.Budget <= 0 || c.testing != nil || now.Before(p.nextTest) || now.Before(c.refused) ||
		c.pinned(cfg.Pins.Media) == p.node {
		return nil
	}
	var pick *nodes.Node
	var pickSt measure.State
	for _, n := range c.usable(other(p.node)) {
		st := c.stats.State(n)
		if st.Rate.Measured() && now.Sub(st.Rate.At) < retestAfter {
			continue
		}
		if pick == nil || cmp.Or(st.Rate.At.Compare(pickSt.Rate.At), cmp.Compare(rttMs(st), rttMs(pickSt))) < 0 {
			pick, pickSt = n, st
		}
	}
	if pick == nil {
		return nil
	}
	c.testing = pick
	why := "never measured"
	if pickSt.Rate.Measured() {
		why = fmt.Sprintf("measured %.0f min ago", now.Sub(pickSt.Rate.At).Minutes())
	}
	slog.Info("explore", "session", p.key, "media", p.node.Name, "node", pick.Name, "why", why,
		"node_mbps", mbpsLog(pickSt.Rate), "media_mbps", mbpsLog(c.stats.State(p.node).Rate))
	return pick
}

// Probed ends the session's test, which read the given bytes.
func (s *Stream) Probed(spent int64) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	c.testing = nil
	s.tests++
	perSec := c.cfg.Load().Probes.Budget * s.bitrate * 1e6 / 8
	s.nextTest = time.Now().Add(max(minTestGap, time.Duration(float64(spent)/perSec*float64(time.Second))))
}

// Failover picks a replacement after a hard failure (no bytes for 4 s or a
// connection error): the fastest other node.
func (p *Playback) Failover(failed *nodes.Node) *nodes.Node {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.rank(c.usable(func(n *nodes.Node) bool { return n != failed && n != p.node })); len(r) > 0 {
		return r[0].n
	}
	return nil
}

// Switched records that the stream now runs on n.
func (p *Playback) Switched(n *nodes.Node, reason string) {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	p.failovers++
	slog.Info("switched", "session", p.key, "from", p.node.Name, "to", n.Name, "reason", reason,
		"ahead_s", round1(p.ahead.Seconds()), "from_mbps", mbpsLog(c.stats.State(p.node).Rate),
		"to_mbps", mbpsLog(c.stats.State(n).Rate), "bitrate_mbps", round1(p.bitrate), "failovers", p.failovers)
	failovers.WithLabelValues(reason).Inc()
	p.node = n
	p.switched = time.Now()
}

// top describes the fastest nodes for the logs, up to five: each with its
// rate in Mbps, or ? if never measured.
func top(r []ranked) string {
	var b strings.Builder
	for i, o := range r[:min(5, len(r))] {
		if i > 0 {
			b.WriteString(", ")
		}
		if o.st.Rate.Measured() {
			fmt.Fprintf(&b, "%s %.1f", o.n.Name, o.st.Rate.Value)
		} else {
			fmt.Fprintf(&b, "%s ?", o.n.Name)
		}
	}
	return b.String()
}

// mbpsLog is an estimate's value for the logs: -1 for none.
func mbpsLog(e measure.Estimate) float64 {
	if !e.Measured() {
		return -1
	}
	return round1(e.Value)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
