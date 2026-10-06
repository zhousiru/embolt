package control

import (
	"math"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
)

// The controller looks Horizon ahead, and counts the buffer falling under
// BufferMin as a stall.
const (
	Horizon   = 120 * time.Second
	BufferMin = 10 * time.Second
)

// requiredMbps is the average rate needed over the horizon H for a buffer of
// B seconds to stay above the low mark B_min at bitrate V:
//
//	R ≥ V · (1 − (B − B_min) / H)
//
// A full 60 s buffer needs 0.58 V; an empty one 1.08 V.
func requiredMbps(buffer time.Duration, bitrate float64) float64 {
	return bitrate * (1 - (buffer-BufferMin).Seconds()/Horizon.Seconds())
}

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
		mbps = st.Rate.Typical()
	}
	return burstRTTs*st.RTT.Typical() + burstMbit*1000/mbps
}
