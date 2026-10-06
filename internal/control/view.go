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

// keepEnded is how long the pane shows a session after it ends, and
// maxEnded how many.
const (
	keepEnded = 24 * time.Hour
	maxEnded  = 50
)

// ended is a session the controller has let go, kept for the pane.
type ended struct {
	view    view.Session
	history []view.Point
}

// Sessions lists the sessions playing, oldest first.
func (c *Controller) Sessions() []view.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []view.Session{}
	for _, p := range c.plays {
		out = append(out, c.sessionView(p))
	}
	slices.SortFunc(out, func(a, b view.Session) int { return a.Started.Compare(b.Started) })
	return out
}

// Recent lists the sessions ended within keepEnded, newest first.
func (c *Controller) Recent() []view.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []view.Session{}
	for _, e := range slices.Backward(c.ended) {
		if time.Since(e.view.Ended) < keepEnded {
			out = append(out, e.view)
		}
	}
	return out
}

// Session shows a session, playing or ended, with its recent steps.
func (c *Controller) Session(key string) view.SessionDetail {
	d := view.SessionDetail{History: []view.Point{}, Events: []view.Event{}}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.plays[key]; p != nil {
		v := c.sessionView(p)
		d.Session, d.History = &v, slices.Clone(p.history)
		return d
	}
	for _, e := range slices.Backward(c.ended) {
		if e.view.Key == key {
			v := e.view
			d.Session, d.History = &v, slices.Clone(e.history)
			break
		}
	}
	return d
}

// sessionView is p as of its last step. Caller holds c.mu.
func (c *Controller) sessionView(p *Playback) view.Session {
	state := cmp.Or(p.state, "starting")
	if len(p.streams) == 0 {
		state = "idle"
	}
	v := view.Session{
		Key:          p.key,
		State:        state,
		Media:        ref(p.node),
		Players:      p.viewers,
		BitrateMbps:  p.bitrate,
		AheadSeconds: p.ahead.Seconds(),
		FetchedMbps:  p.fetched,
		Failovers:    p.failovers,
		Tests:        p.tests,
		LowSeconds:   p.low.Seconds(),
		Started:      p.started,
	}
	if r := c.stats.State(p.node).Rate; r.Measured() {
		v.NodeMbps = r.Value
	}
	return v
}

// end keeps p for the pane once it is let go. Caller holds c.mu.
func (c *Controller) end(p *Playback) {
	v := c.sessionView(p)
	v.State, v.Ended, v.Players, v.FetchedMbps = "ended", p.idle, 0, 0
	c.ended = append(c.ended, ended{view: v, history: p.history})
	if len(c.ended) > maxEnded {
		c.ended = slices.Delete(c.ended, 0, len(c.ended)-maxEnded)
	}
}

// Testing is the node under a speed test, if any, and until when tests are
// paused after the server refused one.
func (c *Controller) Testing() (*view.NodeRef, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	paused := time.Time{}
	if time.Now().Before(c.refused) {
		paused = c.refused
	}
	return refPtr(c.testing), paused
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
		LowMarkSeconds:   LowMark.Seconds(),
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
		Sampled:     st.Rate.At,
		BreakerOpen: st.Open(now),
		Roles:       append([]string{}, roles...),
	}
	if v.BreakerOpen {
		v.OpenUntil = st.OpenUntil
	}
	return v
}

func estimate(e measure.Estimate) view.Estimate {
	return view.Estimate{Measured: e.Measured(), Mean: e.Value}
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
