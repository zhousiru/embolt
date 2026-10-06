package control

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// LowMark is the low mark: a session behind with a read-ahead under it is
// draining its player, which has only its own buffer left.
const LowMark = 10 * time.Second

// The primary is judged on a control burst: a library page of images, about
// 3 MB over a few round trips. RTT alone would pick a node that answers 40 ms
// sooner but takes seconds longer to deliver the page. A node whose rate is
// not measured yet is taken at guessMbps.
const (
	burstRTTs = 4
	burstMbit = 24.0 // 3 MB
	guessMbps = 20.0
)

// burstMs is how long a node takes for a control burst, in ms; +Inf with no
// measured RTT, so a guess never beats a measurement.
func burstMs(st measure.State) float64 {
	if !st.RTT.Measured() {
		return math.Inf(1)
	}
	mbps := guessMbps
	if st.Rate.Measured() {
		mbps = st.Rate.Value
	}
	return burstRTTs*st.RTT.Value + burstMbit*1000/mbps
}

// ranked is a node with its state as of the ranking.
type ranked struct {
	n  *nodes.Node
	st measure.State
}

// faster orders nodes for media, fastest first: a measured rate before
// none, then the higher rate, then the lower RTT.
func faster(a, b measure.State) int {
	return cmp.Or(
		cmp.Compare(btoi(!a.Rate.Measured()), btoi(!b.Rate.Measured())),
		cmp.Compare(b.Rate.Value, a.Rate.Value),
		cmp.Compare(rttMs(a), rttMs(b)))
}

// rttMs is a node's RTT, +Inf if never pinged.
func rttMs(st measure.State) float64 {
	if !st.RTT.Measured() {
		return math.Inf(1)
	}
	return st.RTT.Value
}

// rank orders ns for media, fastest first.
func (c *Controller) rank(ns []*nodes.Node) []ranked {
	out := make([]ranked, len(ns))
	for i, n := range ns {
		out[i] = ranked{n, c.stats.State(n)}
	}
	slices.SortStableFunc(out, func(a, b ranked) int { return faster(a.st, b.st) })
	return out
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
