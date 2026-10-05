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

// pickMedia chooses at play start (B = 0), and says why: the primary if it
// passes, else the lowest-RTT node that passes, else the lowest risk. A second
// session avoids the first one's node when another passes, so one dip hits
// one viewer.
func (c *Controller) pickMedia(bitrate float64) (*nodes.Node, string) {
	if n := c.pinned(c.cfg.Load().Pins.Media); n != nil {
		return n, "pinned"
	}
	busy := c.busy()
	free := c.usable(func(n *nodes.Node) bool { return !busy[n] })
	all := c.usable(nil)
	for i, cands := range [][]*nodes.Node{free, all} {
		shared := ""
		if i > 0 {
			shared = ", shared with another session"
		}
		passing := c.passing(cands, 0, bitrate)
		if c.primary != nil && slices.Contains(passing, c.primary) {
			return c.primary, "primary meets the target" + shared
		}
		if n := c.lowestRTT(passing); n != nil {
			return n, "lowest RTT that meets the target" + shared
		}
	}
	return c.leastRisk(all, 0, bitrate), "least risk; none meets the target"
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
	n, _ := c.choose(stay, c.weighMoves(p, c.standbyFor(p), now))
	return n
}

// Explore picks a node to read the session's next stretch through, or nil:
// a speed test whose bytes are played. It runs only when the read-ahead has
// filled, so the media node keeps up and the buffer can carry a test, at most
// once per exploreGap, on the node whose result is worth the most (as
// speedRound picks), within the same exit-IP cap. The stream comes back to
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
	picks := c.withinIPCap(c.ranked(c.busy(), p.bitrate, now), now, 1)
	if len(picks) == 0 {
		return nil
	}
	p.explored, c.explored = now, now
	slog.Debug("exploring", "session", p.key, "media", p.node.Name, "node", picks[0].Name)
	return picks[0]
}

// option is one action the step weighs.
type option struct {
	n           *nodes.Node
	rate        measure.Belief // as judged
	gap         time.Duration  // of a switch
	risk, stall float64        // p_stall and expected stall seconds over the horizon
	notStandby  bool
	rtt         float64
}

// weighStay judges the media node at the session's buffer, by the stream's
// own samples.
func (c *Controller) weighStay(p *Playback, now time.Time) option {
	cfg := c.cfg.Load().Control
	st := c.stats.StateAt(p.node, now)
	stay := option{n: p.node, rate: st.Rate}
	switch {
	case st.Open(now):
		stay.risk, stay.stall = 1, expectedStall(measure.Belief{}, p.buffer, p.bitrate)
	case p.full: // the node has shown it keeps up: nothing to predict
		stay.rate = streamRate(cfg, st.Rate, p.recent, now)
	default:
		stay.rate = streamRate(cfg, st.Rate, p.recent, now)
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

// weighMoves judges a switch to each other usable node at B − g: nothing
// arrives during its gap g.
func (c *Controller) weighMoves(p *Playback, standby *nodes.Node, now time.Time) []option {
	var moves []option
	for _, n := range c.usable(func(n *nodes.Node) bool { return n != p.node }) {
		rate, gap := c.stats.StateAt(n, now).Rate, c.switchGap(n)
		after := p.buffer - gap
		moves = append(moves, option{n, rate, gap, stallRisk(rate, after, p.bitrate), expectedStall(rate, after, p.bitrate), n != standby, c.rttKey(n)})
	}
	return moves
}

// choose picks a move, or nil to stay, and says why: the move that meets ε
// with the least expected stall, else whichever action stalls least.
func (c *Controller) choose(stay option, moves []option) (*nodes.Node, string) {
	if len(moves) == 0 {
		return nil, "no other usable node"
	}
	eps := c.cfg.Load().Control.StallRisk
	if passing := slices.DeleteFunc(slices.Clone(moves), func(m option) bool { return m.risk > eps }); len(passing) > 0 {
		return slices.MinFunc(passing, betterMove).n, "meets the target with the least expected stall"
	}
	if best := slices.MinFunc(moves, betterMove); best.stall < stay.stall {
		return best.n, "least expected stall; none meets the target"
	}
	return nil, "no move stalls less; none meets the target"
}

func betterMove(a, b option) int {
	return cmp.Or(cmp.Compare(a.stall, b.stall), cmp.Compare(btoi(a.notStandby), btoi(b.notStandby)), cmp.Compare(a.rtt, b.rtt))
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
	return c.leastRisk(others, p.buffer, p.bitrate)
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

// standbyFor keeps a warm standby: the lowest risk at the current buffer
// among nodes sharing neither provider nor /24 with the media node, relaxed
// step by step when no such node exists. Re-picked every 5 min.
func (c *Controller) standbyFor(p *Playback) *nodes.Node {
	if sb := p.standby; sb != nil && sb != p.node && time.Since(p.standbyAt) < standbyRefresh && !c.open(sb) {
		return sb
	}
	m := p.node
	tiers := []func(*nodes.Node) bool{
		func(n *nodes.Node) bool { return n.Provider != m.Provider && n.Subnet != m.Subnet },
		func(n *nodes.Node) bool { return n.Subnet != m.Subnet },
		func(*nodes.Node) bool { return true },
	}
	for _, diverse := range tiers {
		cands := c.usable(func(n *nodes.Node) bool { return n != m && diverse(n) })
		if n := c.leastRisk(cands, p.buffer, p.bitrate); n != nil {
			p.standby, p.standbyAt = n, time.Now()
			return n
		}
	}
	return nil
}

// risk is a node's stall risk judged by its belief alone: for nodes that
// carry no stream of this session.
func (c *Controller) risk(n *nodes.Node, buffer time.Duration, bitrate float64) float64 {
	st := c.stats.State(n)
	if st.Open(time.Now()) {
		return 1
	}
	return stallRisk(st.Rate, buffer, bitrate)
}

func (c *Controller) passing(ns []*nodes.Node, buffer time.Duration, bitrate float64) []*nodes.Node {
	eps := c.cfg.Load().Control.StallRisk
	var out []*nodes.Node
	for _, n := range ns {
		if c.risk(n, buffer, bitrate) <= eps {
			out = append(out, n)
		}
	}
	return out
}

// leastRisk picks the lowest stall risk, ties broken by RTT: before any rate
// data every node ties, and a low-latency node is the better guess.
func (c *Controller) leastRisk(ns []*nodes.Node, buffer time.Duration, bitrate float64) *nodes.Node {
	type scored struct {
		n         *nodes.Node
		risk, rtt float64
	}
	if len(ns) == 0 {
		return nil
	}
	s := make([]scored, len(ns))
	for i, n := range ns {
		s[i] = scored{n, c.risk(n, buffer, bitrate), c.rttKey(n)}
	}
	return slices.MinFunc(s, func(a, b scored) int {
		return cmp.Or(cmp.Compare(a.risk, b.risk), cmp.Compare(a.rtt, b.rtt))
	}).n
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
