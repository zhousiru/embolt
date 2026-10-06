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
	minSwitchGap = 20 * time.Second
	switchRamp   = time.Second            // a new node's ramp-up before it delivers
	guessRTT     = 300 * time.Millisecond // for a switch to a node never pinged
	migrateGap   = 5 * time.Minute        // the least time on a node before a move for speed
	migrateGain  = 1.5                    // how much faster a node must be to take a session that keeps up
	fresh        = 2 * time.Minute        // how recent a test must be to move a session for speed
	retestAfter  = 30 * time.Minute       // how often a node that may be faster is tested again
	minTestGap   = 30 * time.Second       // between a session's tests, however few bytes they read
	logEvery     = 30 * time.Second       // of a session's state
)

// A test reads up to ProbeBytes through an explored node, beside the media
// node, for up to ProbeTime.
const (
	ProbeBytes = 32 << 20
	ProbeTime  = 8 * time.Second
)

// Playback is one viewing session: one device watching one item. It holds
// the media node and the controller's view of the buffer.
// Seeks and the player's side connections join the same Playback, so the
// session keeps one exit IP.
// A playback lasts while it has a stream: a file it reads from upstream,
// which any number of player connections read.
type Playback struct {
	c       *Controller
	key     string
	started time.Time

	// Guarded by c.mu.
	bitrate    float64
	node       *nodes.Node
	switched   time.Time
	nextTest   time.Time // the earliest the session may test another node
	tests      int
	failovers  int
	streams    map[*Stream]struct{}
	viewers    int // player connections
	idle       time.Time
	buffer     time.Duration
	full       bool // the read-ahead filled since the last step
	live       float64
	safe, need float64 // of the media node, at the last step
	logged     time.Time

	// What the pane shows, as of the last step.
	state   string        // starting, ok, risk or low
	fetched float64       // Mbps read from upstream over the last step
	primed  bool          // the buffer has passed the low mark
	low     time.Duration // under the low mark since
	stepped time.Time
	history []view.Point // the last historyLen steps
}

// historyLen steps make the pane's 10 min of a session.
const historyLen = 300

// Stream is one file a playback reads from upstream, judged as a whole: its
// observation covers every region the player reads.
type Stream struct {
	*Playback
	delivered time.Duration // media handed to players; guarded by c.mu
}

// Observation is what a stream reports at each step.
type Observation struct {
	ReadAhead time.Duration // media in Embolt's read-ahead
	Delivered time.Duration // media handed to players since the stream opened
	Elapsed   time.Duration // since the stream opened
	Full      bool          // the read-ahead filled since the last step
	Fetched   float64       // Mbps read from upstream since the last step
}

// buffer is the media ahead of playback: the read-ahead plus a lower bound
// on the player's own buffer. Startup and pauses only lower the bound.
func (o Observation) buffer() time.Duration {
	return o.ReadAhead + max(0, o.Delivered-o.Elapsed)
}

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
		n, why, opts := c.pickMedia(bitrate)
		if n == nil {
			return nil, ErrNoNode
		}
		p = &Playback{c: c, key: key, started: now, bitrate: bitrate, node: n, streams: map[*Stream]struct{}{}}
		c.plays[key] = p
		slog.Info("playback started", "session", key, "media", n.Name, "bitrate_mbps", round1(bitrate),
			"why", why, "options", top(opts))
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

// leads reports whether s drives the session: it has delivered the most.
func (s *Stream) leads() bool {
	for o := range s.streams {
		if o.delivered > s.delivered {
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

// pickMedia chooses at play start (B = 0), says why, and returns the options
// it weighed: the node that meets the target with the highest safe rate,
// unless the primary meets it and that node is not migrateGain faster, so
// browsing and playback share one exit IP; when none meets it, the one that
// falls least short.
func (c *Controller) pickMedia(bitrate float64) (*nodes.Node, string, []option) {
	if n := c.pinned(c.cfg.Load().Pins.Media); n != nil {
		return n, "pinned", nil
	}
	opts := c.weigh(c.usable(nil), 0, bitrate, time.Now(), false)
	o, ok := best(opts, bySafe)
	switch {
	case o.n == nil:
		return nil, "", opts
	case !ok:
		return o.n, "least short; none meets the target", opts
	}
	if i := slices.IndexFunc(opts, func(x option) bool { return x.n == c.primary }); i >= 0 &&
		opts[i].meets() && o.safe < migrateGain*opts[i].rate.Typical() {
		return c.primary, "primary meets the target", opts
	}
	return o.n, "highest safe rate that meets the target", opts
}

// Step is the controller loop for one session, called every 2 s by each of
// its streams; only the lead stream decides. It returns a node to switch to
// and why ("risk" or "faster"), or nil to stay. See decide.
func (s *Stream) Step(o Observation) (*nodes.Node, string) {
	p, c := s.Playback, s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.delivered = o.Delivered; !s.leads() {
		return nil, ""
	}
	now := time.Now()
	p.buffer, p.full = o.buffer(), o.Full
	v := c.decide(p, now)
	p.safe, p.need, p.live = v.stay.safe, max(0, v.stay.need), 0.0
	if v.stay.rate.Measured() {
		p.live = v.stay.rate.Typical()
	}
	p.record(o, v, now)
	if v.to == nil && now.Sub(p.logged) >= logEvery {
		p.logged = now
		slog.Info("session", "session", p.key, "media", p.node.Name, "buffer_s", round1(p.buffer.Seconds()),
			"full", p.full, "live_mbps", round1(p.live), "safe_mbps", round1(p.safe), "need_mbps", round1(p.need),
			"bitrate_mbps", round1(p.bitrate), "verdict", v.why)
	}
	if v.to == nil {
		return nil, ""
	}
	reason := "risk"
	if v.faster {
		reason = "faster"
	}
	slog.Info("move", "session", p.key, "from", p.node.Name, "to", v.to.Name, "reason", reason, "why", v.why,
		"buffer_s", round1(p.buffer.Seconds()), "bitrate_mbps", round1(p.bitrate),
		"safe_mbps", round1(p.safe), "need_mbps", round1(p.need), "options", top(v.moves))
	return v.to, reason
}

// record keeps what the step saw for the pane.
func (p *Playback) record(o Observation, v verdict, now time.Time) {
	under := p.buffer < BufferMin
	if p.primed && under && !p.stepped.IsZero() {
		p.low += now.Sub(p.stepped)
	}
	p.primed = p.primed || !under
	switch {
	case !p.primed:
		p.state = "starting"
	case under:
		p.state = "low"
	case !v.stay.meets():
		p.state = "risk"
	default:
		p.state = "ok"
	}
	p.fetched, p.stepped = o.Fetched, now
	if len(p.history) == historyLen {
		p.history = slices.Delete(p.history, 0, 1)
	}
	p.history = append(p.history, view.Point{At: now, Buffer: p.buffer.Seconds(), Mbps: o.Fetched})
}

// verdict is what a step does with a session, and the options it weighed.
type verdict struct {
	stay   option
	moves  []option // nil if the step weighed none
	to     *nodes.Node
	why    string
	faster bool // a move for speed: staying meets the target too
}

// decide judges each action over the next H:
//
//  1. Stay on a pinned node, or on one switched to moments ago.
//  2. If staying meets the target (a full read-ahead always does), stay,
//     unless the session has been on its node for migrateGap and a node
//     tested just now meets the target with a safe rate migrateGain times
//     the media node's typical rate now: then move for speed.
//  3. Else the move that meets the target with the highest safe rate.
//  4. Else whichever action, staying included, falls least short.
//
// A switch is judged at B − g: nothing arrives during its gap g. That prices
// a move, and the bar for a move for speed keeps a session on one exit IP
// unless another is far better.
func (c *Controller) decide(p *Playback, now time.Time) verdict {
	v := verdict{stay: c.weighStay(p, now)}
	switch {
	case c.pinned(c.cfg.Load().Pins.Media) == p.node:
		v.why = "pinned"
		return v
	case now.Sub(p.switched) < minSwitchGap:
		v.why = "switched moments ago"
		return v
	case !v.stay.meets():
		v.moves = c.moves(p, now)
		v.to, v.why = choose(v.stay, v.moves)
		return v
	}
	v.why = "meets the target"
	if p.full {
		v.why = "read-ahead full"
	}
	if now.Sub(p.started) < migrateGap || now.Sub(p.switched) < migrateGap {
		return v
	}
	v.moves = c.moves(p, now)
	if o, ok := best(v.moves, bySafe); ok && now.Sub(o.rate.At) < fresh && o.safe >= migrateGain*v.stay.rate.Typical() {
		v.to, v.why, v.faster = o.n, "a node tested just now is far faster", true
	}
	return v
}

// Explore picks a node to test beside the session's media node, or nil: a
// speed test whose bytes are dropped, and the only test Embolt runs. Of the
// nodes that may be faster than the media node, given their evidence, it
// tests the one that may be fastest, by upside: a node never measured
// first, of those the quickest to answer pings. A node measured lately is
// left alone for retestAfter, and one shown slower until its evidence
// fades. A test that finds a node far faster moves the session at a later
// step.
//
// Tests run one at a time, and a session spends on them at most
// probes.budget of its bitrate: after a test, the next waits until the
// session has played its bytes over budget, and at least minTestGap. The
// caller reports what a test read with Probed.
//
// Call it after Step, which judges the media node.
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
	media := c.stats.StateAt(p.node, now).Rate
	var pick *nodes.Node
	var pickSt measure.State
	for _, n := range c.usable(func(n *nodes.Node) bool { return n != p.node }) {
		st := c.stats.StateAt(n, now)
		if r := st.Rate; r.Measured() && (r.Upside() <= media.Typical() || now.Sub(r.At) < retestAfter) {
			continue
		}
		if pick == nil || cmp.Or(cmp.Compare(st.Rate.Upside(), pickSt.Rate.Upside()), cmp.Compare(pickSt.RTT.Upside(), st.RTT.Upside())) > 0 {
			pick, pickSt = n, st
		}
	}
	if pick == nil {
		return nil
	}
	pickRate := pickSt.Rate
	c.testing = pick
	why := "maybe faster"
	if !pickRate.Measured() {
		why = "never measured"
	}
	slog.Info("explore", "session", p.key, "media", p.node.Name, "node", pick.Name, "why", why,
		"node_mbps", mbpsLog(pickRate), "media_mbps", mbpsLog(media))
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

// option is one node judged for a session.
type option struct {
	n    *nodes.Node
	rate measure.Estimate // its rate now
	gap  time.Duration    // of a switch
	safe float64          // Mbps it keeps to most of the time, given its evidence: rate.Low
	need float64          // Mbps the session needs from it
	rtt  float64          // ms, the tie-break
}

// meets reports whether the node is shown to deliver what the session
// needs: a safe rate of 0 shows nothing.
func (o option) meets() bool { return o.safe > 0 && o.safe >= o.need }

// best is the one rule behind every node choice: among the options that
// meet the target, the first by prefer; when none does, the one that falls
// least short, ties to prefer. It reports whether the pick meets the
// target. With no options, the pick has no node.
func best(opts []option, prefer func(a, b option) int) (option, bool) {
	if len(opts) == 0 {
		return option{}, false
	}
	if passing := slices.DeleteFunc(slices.Clone(opts), func(o option) bool { return !o.meets() }); len(passing) > 0 {
		return slices.MinFunc(passing, prefer), true
	}
	return slices.MinFunc(opts, func(a, b option) int {
		return cmp.Or(cmp.Compare(b.safe-b.need, a.safe-a.need), prefer(a, b))
	}), false
}

// bySafe prefers the highest safe rate, then the lowest RTT.
func bySafe(a, b option) int {
	return cmp.Or(cmp.Compare(b.safe, a.safe), cmp.Compare(a.rtt, b.rtt))
}

// weigh judges nodes by their rates now at the session's buffer and bitrate.
// A switch delivers nothing for its gap g, so with moving set each node is
// judged at B − g: that prices a move.
func (c *Controller) weigh(ns []*nodes.Node, buffer time.Duration, bitrate float64, now time.Time, moving bool) []option {
	opts := make([]option, 0, len(ns))
	for _, n := range ns {
		st := c.stats.StateAt(n, now)
		o := option{n: n, rate: st.Now, safe: st.Now.Low(), rtt: math.Inf(1)}
		rtt := guessRTT
		if st.RTT.Measured() {
			o.rtt = st.RTT.Typical()
			rtt = time.Duration(o.rtt * float64(time.Millisecond))
		}
		if moving {
			o.gap = switchRamp + 4*rtt // connect through the node, then ramp up
		}
		o.need = requiredMbps(buffer-o.gap, bitrate)
		opts = append(opts, o)
	}
	return opts
}

// weighStay judges the media node at the session's buffer, as weigh judges
// the nodes the session might move to. An open breaker shows it delivers
// nothing; a full read-ahead has shown it keeps up, so it needs nothing
// more.
func (c *Controller) weighStay(p *Playback, now time.Time) option {
	stay := c.weigh([]*nodes.Node{p.node}, p.buffer, p.bitrate, now, false)[0]
	if c.stats.Open(p.node, now) {
		stay.safe = 0
	}
	if p.full {
		stay.need = 0
	}
	return stay
}

// moves judges a switch to each other usable node.
func (c *Controller) moves(p *Playback, now time.Time) []option {
	return c.weigh(c.usable(func(n *nodes.Node) bool { return n != p.node }), p.buffer, p.bitrate, now, true)
}

// choose picks a move for a session that falls short of its target, or nil
// to stay, and says why: the move that meets the target with the highest
// safe rate, else whichever action falls least short.
func choose(stay option, moves []option) (*nodes.Node, string) {
	o, ok := best(append([]option{stay}, moves...), func(a, b option) int {
		return cmp.Or(cmp.Compare(btoi(a.n != stay.n), btoi(b.n != stay.n)), bySafe(a, b))
	})
	switch {
	case o.n == stay.n && len(moves) == 0:
		return nil, "no other usable node"
	case o.n == stay.n:
		return nil, "no move falls less short; none meets the target"
	case ok:
		return o.n, "meets the target with the highest safe rate"
	}
	return o.n, "least short; none meets the target"
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Failover picks a replacement after a hard failure (no bytes for 4 s or a
// connection error), without asking whether to move: the best other node,
// judged then, as a step would judge a move.
func (p *Playback) Failover(failed *nodes.Node) *nodes.Node {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	others := c.usable(func(n *nodes.Node) bool { return n != failed && n != p.node })
	o, _ := best(c.weigh(others, p.buffer, p.bitrate, time.Now(), true), bySafe)
	return o.n
}

// Switched records that the stream now runs on n.
func (p *Playback) Switched(n *nodes.Node, reason string) {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	p.failovers++
	slog.Info("switched", "session", p.key, "from", p.node.Name, "to", n.Name, "reason", reason,
		"buffer_s", round1(p.buffer.Seconds()), "live_mbps", round1(p.live), "bitrate_mbps", round1(p.bitrate),
		"failovers", p.failovers)
	failovers.WithLabelValues(reason).Inc()
	p.node = n
	p.switched = time.Now()
}

// top describes the best options for the logs, up to five: each node's safe
// rate over its need, marked ✓ when it meets the target.
func top(opts []option) string {
	opts = slices.Clone(opts)
	slices.SortFunc(opts, func(a, b option) int { return cmp.Or(cmp.Compare(btoi(!a.meets()), btoi(!b.meets())), bySafe(a, b)) })
	var b strings.Builder
	for i, o := range opts[:min(5, len(opts))] {
		if i > 0 {
			b.WriteString(", ")
		}
		mark := ""
		if o.meets() {
			mark = "✓"
		}
		fmt.Fprintf(&b, "%s %.1f/%.1f%s", o.n.Name, o.safe, max(0, o.need), mark)
	}
	return b.String()
}

// mbpsLog is an estimate's typical rate for the logs: -1 for none.
func mbpsLog(e measure.Estimate) float64 {
	if !e.Measured() {
		return -1
	}
	return round1(e.Typical())
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
