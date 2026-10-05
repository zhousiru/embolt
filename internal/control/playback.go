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
	minSwitchGap   = 20 * time.Second
	standbyRefresh = 5 * time.Minute
	switchRamp     = time.Second // a new node's ramp-up before it delivers
	exploreGap     = 3 * time.Minute
)

// StreamMemory is how far back a stream's own rate samples count.
const StreamMemory = 30 * time.Second

// Playback is one viewing session: one device watching one item. It holds
// the media node, the warm standby, and the controller's view of the buffer.
// Seeks and the player's side connections join the same Playback, so the
// session keeps one exit IP.
type Playback struct {
	c       *Controller
	key     string
	started time.Time

	// Guarded by c.mu.
	bitrate   float64
	node      *nodes.Node
	standby   *nodes.Node
	standbyAt time.Time
	switched  time.Time
	explored  time.Time // last read a stretch through another node
	failovers int
	streams   map[*Stream]struct{}
	idle      time.Time
	buffer    time.Duration
	live      float64
	recent    []float64 // the lead stream's rate samples, Mbps
	full      bool      // the read-ahead filled since the last step
	risk      float64
}

// Stream is one player connection within a playback.
type Stream struct {
	*Playback
	delivered time.Duration // media handed to the player; guarded by c.mu
}

// Observation is what a stream reports at each step.
type Observation struct {
	ReadAhead time.Duration // media in Embolt's read-ahead
	Delivered time.Duration // media handed to the player since the stream opened
	Elapsed   time.Duration // since the stream opened
	Full      bool          // the read-ahead filled since the last step
	Recent    []float64     // the stream's rate samples (Mbps) within StreamMemory
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
		c.plays[key] = p
		slog.Info("playback started", "session", key, "media", n.Name, "bitrate_mbps", round1(bitrate), "reason", why)
	}
	p.bitrate = bitrate
	s := &Stream{Playback: p}
	p.streams[s] = struct{}{}
	return s, nil
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
// meet the target, one no other session uses, so one dip hits one viewer,
// then the primary, then the lowest RTT.
func (c *Controller) pickMedia(bitrate float64) (*nodes.Node, string) {
	if n := c.pinned(c.cfg.Load().Pins.Media); n != nil {
		return n, "pinned"
	}
	busy := c.busy()
	o, ok := c.best(c.weigh(c.usable(nil), 0, bitrate, time.Now(), false), func(a, b option) int {
		return cmp.Or(cmp.Compare(btoi(busy[a.n]), btoi(busy[b.n])),
			cmp.Compare(btoi(a.n != c.primary), btoi(b.n != c.primary)), cmp.Compare(a.rtt, b.rtt))
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
	if busy[o.n] {
		why += ", shared with another session"
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
	p.buffer, p.live, p.recent, p.full = o.buffer(), meanOf(o.Recent), slices.Clone(o.Recent), o.Full
	now := time.Now()
	stay := c.weighStay(p, now)
	p.risk = stay.risk
	if c.holds(p, stay, now) != "" {
		return nil
	}
	n, _ := c.choose(stay, c.moves(p, now), c.standbyFor(p))
	return n
}

// Explore picks a node to read the session's next stretch through, or nil:
// a speed test whose bytes are played, and the only test Embolt runs. It
// runs only when the read-ahead has filled, so the media node keeps up and
// the buffer can carry a test, at most once per exploreGap, on the node whose
// result is worth the most, within the exit-IP cap. The stream comes back to
// the media node afterwards: a faster node is no reason to move a session
// that meets its target, but it informs the next pick, standby and failover.
func (s *Stream) Explore(o Observation) *nodes.Node {
	p, c := s.Playback, s.c
	cfg := c.cfg.Load()
	if !o.Full || !cfg.Probes.Explore {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if s.delivered = o.Delivered; !s.leads() || now.Before(c.refused) ||
		now.Sub(p.switched) < minSwitchGap || now.Sub(p.explored) < exploreGap ||
		c.pinned(cfg.Pins.Media) == p.node {
		return nil
	}
	n := c.firstWithinIPCap(c.ranked(c.busy(), p.bitrate, now), now)
	if n == nil {
		return nil
	}
	p.explored = now
	slog.Debug("exploring", "session", p.key, "media", p.node.Name, "node", n.Name)
	return n
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

// weigh judges nodes by their beliefs at the session's buffer and bitrate.
// A switch delivers nothing for its gap g, so with moving set each node is
// judged at B − g: that prices a move.
func (c *Controller) weigh(ns []*nodes.Node, buffer time.Duration, bitrate float64, now time.Time, moving bool) []option {
	opts := make([]option, 0, len(ns))
	for _, n := range ns {
		o := option{n: n, rate: c.stats.StateAt(n, now).Rate, rtt: c.rttKey(n)}
		if moving {
			o.gap = c.switchGap(n)
		}
		o.risk, o.stall = stallRisk(o.rate, buffer-o.gap, bitrate), expectedStall(o.rate, buffer-o.gap, bitrate)
		opts = append(opts, o)
	}
	return opts
}

// weighStay judges the media node at the session's buffer, by the stream's
// own samples.
func (c *Controller) weighStay(p *Playback, now time.Time) option {
	cfg := c.cfg.Load().Control
	st := c.stats.StateAt(p.node, now)
	stay := option{n: p.node, rate: streamRate(cfg, st.Rate, p.recent, now)}
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
// else the move that meets ε with the least expected stall (the standby on a
// tie), else whichever action stalls least.
func (c *Controller) choose(stay option, moves []option, standby *nodes.Node) (*nodes.Node, string) {
	o, ok := c.best(append([]option{stay}, moves...), func(a, b option) int {
		return cmp.Or(cmp.Compare(btoi(a.n != stay.n), btoi(b.n != stay.n)), cmp.Compare(a.stall, b.stall),
			cmp.Compare(btoi(a.n != standby), btoi(b.n != standby)), cmp.Compare(a.rtt, b.rtt))
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
// connection error), without asking the model whether to move: the standby,
// else the best other node.
func (p *Playback) Failover(failed *nodes.Node) *nodes.Node {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if sb := c.standbyFor(p); sb != nil && sb != failed {
		return sb
	}
	others := c.usable(func(n *nodes.Node) bool { return n != failed && n != p.node })
	o, _ := c.best(c.weigh(others, p.buffer, p.bitrate, time.Now(), true), byStall)
	return o.n
}

// Switched records that the stream now runs on n; a new standby follows.
func (p *Playback) Switched(n *nodes.Node, reason string) {
	c := p.c
	c.mu.Lock()
	defer c.mu.Unlock()
	slog.Info("failover", "session", p.key, "from", p.node.Name, "to", n.Name, "reason", reason,
		"buffer_s", round1(p.buffer.Seconds()), "rate_mbps", round1(p.live), "bitrate_mbps", round1(p.bitrate),
		"stall_risk", math.Round(p.risk*1e4)/1e4)
	failovers.WithLabelValues(reason).Inc()
	p.node, p.standby = n, nil
	p.switched = time.Now()
	p.failovers++
}

// standbyFor keeps a warm standby: among the nodes that meet the target at
// the current buffer, one sharing neither provider nor /24 with the media
// node, else neither /24, else any, then the least expected stall.
// Re-picked every 5 min.
func (c *Controller) standbyFor(p *Playback) *nodes.Node {
	if sb := p.standby; sb != nil && sb != p.node && time.Since(p.standbyAt) < standbyRefresh && !c.open(sb) {
		return sb
	}
	m := p.node
	shared := func(n *nodes.Node) int { return 2*btoi(n.Subnet == m.Subnet) + btoi(n.Provider == m.Provider) }
	others := c.usable(func(n *nodes.Node) bool { return n != m })
	o, _ := c.best(c.weigh(others, p.buffer, p.bitrate, time.Now(), true), func(a, b option) int {
		return cmp.Or(cmp.Compare(shared(a.n), shared(b.n)), byStall(a, b))
	})
	if o.n != nil {
		p.standby, p.standbyAt = o.n, time.Now()
	}
	return o.n
}

// byStall prefers the least expected stall, then the lowest RTT.
func byStall(a, b option) int {
	return cmp.Or(cmp.Compare(a.stall, b.stall), cmp.Compare(a.rtt, b.rtt))
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
