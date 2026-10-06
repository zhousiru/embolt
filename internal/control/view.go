package control

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
	"github.com/zhousiru/embolt/internal/view"
)

// Sessions lists live playbacks.
func (c *Controller) Sessions() []view.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []view.Session{}
	for _, p := range c.plays {
		out = append(out, sessionView(p))
	}
	slices.SortFunc(out, func(a, b view.Session) int { return a.Started.Compare(b.Started) })
	return out
}

func sessionView(p *Playback) view.Session {
	return view.Session{
		Key:           p.key,
		Media:         ref(p.node),
		Streams:       p.viewers,
		BitrateMbps:   p.bitrate,
		BufferSeconds: p.buffer.Seconds(),
		LiveMbps:      p.live,
		SafeMbps:      p.safe,
		NeedMbps:      p.need,
		Failovers:     p.failovers,
		Started:       p.started,
	}
}

// Session shows a live session and the choice its next step faces: every
// usable node judged at the session's buffer and bitrate, as Step judges
// them.
func (c *Controller) Session(key string) view.SessionDetail {
	d := view.SessionDetail{Choices: []view.Choice{}, Events: []view.Event{}}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.plays[key]
	if p == nil {
		return d
	}
	now := time.Now()
	sv := sessionView(p)
	d.Session = &sv
	v := c.decide(p, now)
	d.Verdict = &view.Verdict{To: refPtr(v.to), Reason: v.why}
	moves := v.moves
	if moves == nil {
		moves = c.moves(p, now)
	}
	slices.SortFunc(moves, func(a, b option) int { return cmp.Or(cmp.Compare(btoi(!a.meets()), btoi(!b.meets())), bySafe(a, b)) })
	for i, o := range append([]option{v.stay}, moves...) {
		role := ""
		if i == 0 {
			role = "media"
		}
		d.Choices = append(d.Choices, view.Choice{
			Node:       ref(o.n),
			Role:       role,
			GapSeconds: o.gap.Seconds(),
			Known:      o.known,
			SafeMbps:   o.safe,
			NeedMbps:   max(0, o.need),
			RateMbps:   estimate(o.rate),
		})
	}
	return d
}

// PrimaryRef is the current primary, if any, without electing one.
func (c *Controller) PrimaryRef() *view.NodeRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	return refPtr(c.primary)
}

// Limits are the control settings the pane draws against.
func (c *Controller) Limits() view.Limits {
	k := c.cfg.Load().Control
	return view.Limits{
		BufferMinSeconds: BufferMin.Seconds(),
		ReadAheadSeconds: k.ReadAhead.Seconds(),
	}
}

// Nodes lists every node: usable before open, measured before not, then by
// rate.
func (c *Controller) Nodes() []view.Node {
	roles := c.roles()
	out := []view.Node{}
	for _, n := range c.pool.All() {
		out = append(out, c.nodeView(n, roles[n]))
	}
	slices.SortFunc(out, func(a, b view.Node) int {
		return cmp.Or(
			cmp.Compare(btoi(a.BreakerOpen), btoi(b.BreakerOpen)),
			cmp.Compare(btoi(!a.RateMbps.Measured), btoi(!b.RateMbps.Measured)),
			cmp.Compare(b.RateMbps.Mean, a.RateMbps.Mean),
			cmp.Compare(a.RTTMs.Mean, b.RTTMs.Mean))
	})
	return out
}

func (c *Controller) Node(id string) (view.NodeDetail, bool) {
	n := c.pool.Get(id)
	if n == nil {
		return view.NodeDetail{}, false
	}
	d := view.NodeDetail{Node: c.nodeView(n, c.roles()[n]), Samples: []view.Sample{}}
	for _, s := range slices.Backward(c.stats.Recent(n)) {
		d.Samples = append(d.Samples, view.Sample{
			Time:   s.Time,
			Kind:   string(s.Kind),
			Mbps:   s.Mbps(),
			TTFBMs: float64(s.TTFB) / float64(time.Millisecond),
			Error:  errClass(s.Err),
		})
	}
	return d, true
}

// roles maps nodes to their roles.
func (c *Controller) roles() map[*nodes.Node][]string {
	cfg := c.cfg.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	roles := map[*nodes.Node][]string{}
	add := func(n *nodes.Node, role string) {
		if n != nil && !slices.Contains(roles[n], role) {
			roles[n] = append(roles[n], role)
		}
	}
	add(c.primary, "primary")
	for _, p := range c.plays {
		add(p.node, "media")
	}
	add(c.pool.Find(cfg.Pins.Primary), "pinned")
	add(c.pool.Find(cfg.Pins.Media), "pinned")
	return roles
}

func (c *Controller) nodeView(n *nodes.Node, roles []string) view.Node {
	st := c.stats.State(n)
	now := time.Now()
	v := view.Node{
		ID:          n.ID,
		Name:        n.Name,
		Protocol:    n.Protocol,
		Provider:    n.Provider,
		RTTMs:       estimate(st.RTT),
		RateMbps:    estimate(st.Rate),
		BreakerOpen: st.Open(now),
		Roles:       append([]string{}, roles...),
	}
	if v.BreakerOpen {
		v.OpenUntil = st.OpenUntil
	}
	return v
}

// estimate shows a log-scale estimate on the linear scale: its typical
// value and the 90% range of one sample.
func estimate(e measure.Estimate) view.Estimate {
	lo, hi := e.Range()
	return view.Estimate{Measured: e.Measured(), Mean: e.Typical(), Low: lo, High: hi, Evidence: e.Weight}
}

// errClass reduces an error to its kind: raw messages can name the node's
// server, which the pane must never show.
func errClass(err string) string {
	switch e := strings.ToLower(err); {
	case e == "":
		return ""
	case strings.Contains(e, "timeout"), strings.Contains(e, "deadline"):
		return "timeout"
	case strings.Contains(e, "refused"):
		return "refused"
	case strings.Contains(e, "reset"), strings.Contains(e, "broken pipe"):
		return "reset"
	case strings.Contains(e, "eof"):
		return "eof"
	case strings.Contains(e, "tls"), strings.Contains(e, "certificate"):
		return "tls"
	default:
		return "error"
	}
}

func ref(n *nodes.Node) view.NodeRef { return view.NodeRef{ID: n.ID, Name: n.Name} }

func refPtr(n *nodes.Node) *view.NodeRef {
	if n == nil {
		return nil
	}
	r := ref(n)
	return &r
}
