package control

import (
	"cmp"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

const (
	minSwitchGap = 20 * time.Second
	switchRamp   = time.Second      // a new node's ramp-up before it delivers
	earnGap      = 10 * time.Second // the longest a session earns for between steps: not while paused
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
	bitrate   float64
	node      *nodes.Node
	switched  time.Time
	credit    float64   // bytes the session may spend on tests
	earned    time.Time // when it last earned credit
	failovers int
	streams   map[*Stream]struct{}
	viewers   int // player connections
	idle      time.Time
	buffer    time.Duration
	live      float64
	full      bool // the read-ahead filled since the last step
	risk      float64
}

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
		n, why := c.pickMedia(bitrate)
		if n == nil {
			return nil, ErrNoNode
		}
		p = &Playback{c: c, key: key, started: now, bitrate: bitrate, node: n, streams: map[*Stream]struct{}{}}
		if c.cfg.Load().Probes.Budget > 0 {
			p.credit = ProbeBytes // a session that starts on a bad node may test at once
		}
		c.plays[key] = p
		slog.Info("playback started", "session", key, "media", n.Name, "bitrate_mbps", round1(bitrate), "reason", why)
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

// pickMedia chooses at play start (B = 0), and says why: among nodes that
// meet the target, the primary, then the lowest RTT.
func (c *Controller) pickMedia(bitrate float64) (*nodes.Node, string) {
	if n := c.pinned(c.cfg.Load().Pins.Media); n != nil {
		return n, "pinned"
	}
	o, ok := c.best(c.weigh(c.usable(nil), 0, bitrate, time.Now(), false), func(a, b option) int {
		return cmp.Or(cmp.Compare(btoi(a.n != c.primary), btoi(b.n != c.primary)), cmp.Compare(a.rtt, b.rtt))
	})
	switch {
	case o.n == nil:
		return nil, ""
	case !ok:
		return o.n, "least expected stall; none meets the target"
	}
	why := "lowest RTT that meets the target"
	if o.n == c.primary {
		why = "primary meets the target"
	}
	return o.n, why
}

// Step is the controller loop for one session, called every 2 s by each of
// its streams; only the lead stream decides. It looks H ahead for each action
// and returns a node to switch to, or nil to stay:
//
//  1. Stay if it meets the target ε (a full read-ahead always does).
//  2. Else the switch that meets ε with the least expected stall time.
//  3. Else whichever action, staying included, stalls least.
//
// A switch is judged at B − g: nothing arrives during its gap g. That prices a
// move, so a node under target stays however fast another looks, and the
// session keeps one exit IP whenever possible.
func (s *Stream) Step(o Observation) *nodes.Node {
	p, c := s.Playback, s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.delivered = o.Delivered; !s.leads() {
		return nil
	}
	p.buffer, p.full = o.buffer(), o.Full
	now := time.Now()
	stay := c.weighStay(p, now)
	p.risk, p.live = stay.risk, math.Exp(stay.rate.Mu)
	if c.holds(p, stay, now) != "" {
		return nil
	}
	n, _ := c.choose(stay, c.moves(p, now))
	return n
}

// Explore picks a node to test beside the session's media node, or nil: a
// speed test whose bytes are dropped, and the only test Embolt runs. Tests
// run one at a time, and only while the session's credit covers one; the
// caller reports what it spent with Probed.
//
// A session that meets its target, with a fallback known to meet it from an
// empty buffer, has nothing to learn and tests nothing. The fallback's
// belief fades with half_life until it no longer passes, and tests resume.
// Otherwise each usable node draws once from its rate posterior, and the
// best node that out-draws the media node is tested. A
// faster node is no reason to move a session that meets its target, but it
// informs the next pick and failover.
//
// Call it after Step, which judges the media node.
func (s *Stream) Explore() *nodes.Node {
	p, c := s.Playback, s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !s.leads() {
		return nil
	}
	cfg := c.cfg.Load()
	p.earn(cfg.Probes.Budget, now)
	eps := cfg.Control.StallRisk
	if c.probing || p.credit < ProbeBytes || now.Before(c.refused) || c.pinned(cfg.Pins.Media) == p.node ||
		p.risk <= eps && slices.ContainsFunc(c.usable(func(n *nodes.Node) bool { return n != p.node }), func(n *nodes.Node) bool {
			return stallRisk(c.stats.StateAt(n, now).Rate, 0, p.bitrate) <= eps
		}) {
		return nil
	}
	beliefs := map[*nodes.Node]measure.Belief{p.node: c.stats.StateAt(p.node, now).Rate}
	for _, n := range c.usable(nil) {
		beliefs[n] = c.stats.StateAt(n, now).Rate
	}
	n := byDraw(beliefs)[0]
	if n == p.node {
		return nil
	}
	c.probing = true
	slog.Info("exploring", "session", p.key, "media", p.node.Name, "node", n.Name)
	return n
}

// Probed ends the session's test, which spent the given bytes.
func (s *Stream) Probed(spent int64) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probing = false
	s.credit -= float64(spent)
}

// earn adds the session's budget since it last earned: that share of the
// bytes its bitrate plays meanwhile, whatever its node delivers, so a session
// stuck on a slow node earns as fast as any. It holds at most two tests.
func (p *Playback) earn(budget float64, now time.Time) {
	if !p.earned.IsZero() {
		p.credit += budget * p.bitrate * 1e6 / 8 * min(now.Sub(p.earned), earnGap).Seconds()
	}
	p.credit = min(p.credit, 2*ProbeBytes)
	p.earned = now
}

// option is one node judged for a session.
type option struct {
	n           *nodes.Node
	rate        measure.Belief // as judged
	gap         time.Duration  // of a switch
	risk, stall float64        // p_stall and expected stall seconds over the horizon
	rtt         float64        // the tie-break, see rttKey
}

// best is the one rule behind every node choice, a chance-constrained step:
// among the options that meet the target ε, the first by prefer; when none
// does, the least expected stall, ties to prefer. It reports whether the
// pick meets ε. With no options, the pick has no node.
func (c *Controller) best(opts []option, prefer func(a, b option) int) (option, bool) {
	if len(opts) == 0 {
		return option{}, false
	}
	eps := c.cfg.Load().Control.StallRisk
	if passing := slices.DeleteFunc(slices.Clone(opts), func(o option) bool { return o.risk > eps }); len(passing) > 0 {
		return slices.MinFunc(passing, prefer), true
	}
	return slices.MinFunc(opts, func(a, b option) int { return cmp.Or(cmp.Compare(a.stall, b.stall), prefer(a, b)) }), false
}

// weigh judges nodes by their rates now at the session's buffer and bitrate.
// A switch delivers nothing for its gap g, so with moving set each node is
// judged at B − g: that prices a move.
func (c *Controller) weigh(ns []*nodes.Node, buffer time.Duration, bitrate float64, now time.Time, moving bool) []option {
	opts := make([]option, 0, len(ns))
	for _, n := range ns {
		o := option{n: n, rate: c.stats.StateAt(n, now).Now, rtt: c.rttKey(n)}
		if moving {
			o.gap = c.switchGap(n)
		}
		o.risk, o.stall = stallRisk(o.rate, buffer-o.gap, bitrate), expectedStall(o.rate, buffer-o.gap, bitrate)
		opts = append(opts, o)
	}
	return opts
}

// weighStay judges the media node at the session's buffer, by its rate now,
// as weigh judges the nodes the session might move to.
func (c *Controller) weighStay(p *Playback, now time.Time) option {
	st := c.stats.StateAt(p.node, now)
	stay := option{n: p.node, rate: st.Now}
	switch {
	case st.Open(now):
		stay.risk, stay.stall = 1, expectedStall(measure.Belief{}, p.buffer, p.bitrate)
	case !p.full: // a full read-ahead has shown the node keeps up: nothing to predict
		stay.risk, stay.stall = stallRisk(stay.rate, p.buffer, p.bitrate), expectedStall(stay.rate, p.buffer, p.bitrate)
	}
	return stay
}

// holds says why the session stays without weighing a move, or "" when it
// must weigh one.
func (c *Controller) holds(p *Playback, stay option, now time.Time) string {
	cfg := c.cfg.Load()
	switch {
	case p.full:
		return "read-ahead full"
	case stay.risk <= cfg.Control.StallRisk:
		return "meets the target"
	case c.pinned(cfg.Pins.Media) == p.node:
		return "pinned"
	case now.Sub(p.switched) < minSwitchGap:
		return "switched moments ago"
	}
	return ""
}

// moves judges a switch to each other usable node.
func (c *Controller) moves(p *Playback, now time.Time) []option {
	return c.weigh(c.usable(func(n *nodes.Node) bool { return n != p.node }), p.buffer, p.bitrate, now, true)
}

// choose picks a move, or nil to stay, and says why: staying if it meets ε,
// else the move that meets ε with the least expected stall, else whichever
// action stalls least.
func (c *Controller) choose(stay option, moves []option) (*nodes.Node, string) {
	o, ok := c.best(append([]option{stay}, moves...), func(a, b option) int {
		return cmp.Or(cmp.Compare(btoi(a.n != stay.n), btoi(b.n != stay.n)), byStall(a, b))
	})
	switch {
	case o.n == stay.n && ok:
		return nil, "meets the target"
	case o.n == stay.n && len(moves) == 0:
		return nil, "no other usable node"
	case o.n == stay.n:
		return nil, "no move stalls less; none meets the target"
	case ok:
		return o.n, "meets the target with the least expected stall"
	}
	return o.n, "least expected stall; none meets the target"
}

// switchGap estimates how long a switch to n delivers nothing: about four
// round trips to connect through it, then its ramp-up.
func (c *Controller) switchGap(n *nodes.Node) time.Duration {
	rtt := time.Duration(math.Exp(c.stats.State(n).RTT.Mu) * float64(time.Millisecond))
	return switchRamp + 4*rtt
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Failover picks a replacement after a hard failure (no bytes for 4 s or a
// connection error), without asking the model whether to move: the best
// other node, judged then, as a step would judge a move.
func (p *Playback) Failover(failed *nodes.Node) *nodes.Node {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	others := c.usable(func(n *nodes.Node) bool { return n != failed && n != p.node })
	o, _ := c.best(c.weigh(others, p.buffer, p.bitrate, time.Now(), true), byStall)
	return o.n
}

// Switched records that the stream now runs on n.
func (p *Playback) Switched(n *nodes.Node, reason string) {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	slog.Info("failover", "session", p.key, "from", p.node.Name, "to", n.Name, "reason", reason,
		"buffer_s", round1(p.buffer.Seconds()), "rate_mbps", round1(p.live), "bitrate_mbps", round1(p.bitrate),
		"stall_risk", math.Round(p.risk*1e4)/1e4)
	failovers.WithLabelValues(reason).Inc()
	p.node = n
	p.switched = time.Now()
	p.failovers++
}

// byStall prefers the least expected stall, then the lowest RTT.
func byStall(a, b option) int {
	return cmp.Or(cmp.Compare(a.stall, b.stall), cmp.Compare(a.rtt, b.rtt))
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
