package control

import (
	"math"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/measure"
)

// The model looks Horizon ahead, and counts the buffer falling under
// BufferMin as a stall.
const (
	Horizon   = 120 * time.Second
	BufferMin = 10 * time.Second
)

// windows is how many rate samples the horizon spans.
const windows = float64(Horizon / measure.Window)

// requiredMbps is the average rate needed over the horizon H for a buffer of
// B seconds to stay above the low mark B_min at bitrate V:
//
//	R_H ≥ V · (1 − (B − B_min) / H)
//
// A full 60 s buffer needs 0.58 V; an empty one 1.08 V.
func requiredMbps(buffer time.Duration, bitrate float64) float64 {
	return bitrate * (1 - (buffer-BufferMin).Seconds()/Horizon.Seconds())
}

// stallRisk is P(R_H < required), where R_H is the average of the next H/w
// rate samples under the belief rate. Averaging shrinks one window's jitter
// but not the doubt about the mean. It treats windows as independent, which
// flatters the average; B_min, the two-evaluations rule and replay guard
// against that.
func stallRisk(rate measure.Belief, buffer time.Duration, bitrate float64) float64 {
	need := requiredMbps(buffer, bitrate)
	if need <= 0 {
		return 0
	}
	return rate.Average(windows).CDF(math.Log(need))
}

// expectedStall is the expected stall time over the horizon, in seconds:
//
//	(H/V) · E[(R_req − R_H)⁺]
//
// At a steady rate r < V the buffer reaches B_min after (B − B_min)/(1 − r/V)
// seconds, then playback stalls for a share 1 − r/V of the rest. A zero
// belief stands for a node that delivers nothing.
func expectedStall(rate measure.Belief, buffer time.Duration, bitrate float64) float64 {
	need := requiredMbps(buffer, bitrate)
	if need <= 0 {
		return 0
	}
	perMbps := Horizon.Seconds() / bitrate
	if rate.Kappa == 0 {
		return perMbps * need
	}
	// E[(need − e^X)⁺] = ∫ e^x F(x) dx up to ln need. Simpson's rule runs
	// where F changes, within 40 scales of the centre; e^x makes everything
	// 12 log-units below ln need negligible, and above the window F is flat.
	t := rate.Average(windows)
	hi := math.Log(need)
	a, b := max(hi-12, t.Loc-40*t.Scale), min(hi, t.Loc+40*t.Scale)
	sum := 0.0
	if a < b {
		const steps = 256
		h := (b - a) / steps
		for i := 0; i <= steps; i++ {
			w := 2.0 + 2.0*float64(i%2)
			if i == 0 || i == steps {
				w = 1
			}
			x := a + float64(i)*h
			sum += w * math.Exp(x) * t.CDF(x) * h / 3
		}
	}
	if b < hi {
		sum += (math.Exp(hi) - math.Exp(max(a, b))) * t.CDF(max(a, b))
	}
	return perMbps * sum
}

// streamRate is what a stream says about its own node. With no samples yet it
// says nothing new, and the node is judged like any other. With samples, the
// node's history counts as one sample of typical rate (its jitter estimate
// kept) and the stream's samples of the last StreamMemory decide: the node
// belief alone fades over hours and would take minutes to notice a sag.
func streamRate(p config.Control, node measure.Belief, recent []float64, now time.Time) measure.Belief {
	if len(recent) == 0 {
		return node
	}
	b := node.Capped(1)
	for _, mbps := range recent {
		b.Observe(math.Log(mbps), now, p.HalfLife)
	}
	return b
}

// The primary is judged on a control burst: a library page of images, about
// 3 MB over a few round trips. RTT alone would pick a node that answers 40 ms
// sooner but takes seconds longer to deliver the page.
const (
	burstRTTs = 4
	burstMbit = 24.0 // 3 MB
)

// timing is an estimated duration in ms and the spread of that estimate.
type timing struct{ mean, sd float64 }

// upper is the 90% upper bound: low only when the node is both quick and
// well measured.
func (t timing) upper() float64 { return t.mean + 1.2816*t.sd }

// burstTime is how long a node takes for a control burst: burstRTTs typical
// round trips plus burstMbit at the typical rate. It uses the posteriors of
// the mean, whose doubt shrinks with data, mapped to ms by the delta method.
func burstTime(rtt, rate measure.Belief) timing {
	rm, rs := expWithSpread(rtt.Mean())
	inv := rate.Mean()
	inv.Loc = -inv.Loc // ln(1/rate): seconds per Mbit
	im, is := expWithSpread(inv)
	const msPerBurst = burstMbit * 1000
	return timing{burstRTTs*rm + msPerBurst*im, math.Hypot(burstRTTs*rs, msPerBurst*is)}
}

// probFaster is P(a takes at least margin less than b).
func probFaster(a, b timing, margin time.Duration) float64 {
	z := (b.mean - a.mean - float64(margin)/float64(time.Millisecond)) / math.Hypot(a.sd, b.sd)
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// expWithSpread maps a log-scale posterior to the linear scale: e^loc and its
// standard deviation.
func expWithSpread(t measure.StudentT) (mean, sd float64) {
	mean = math.Exp(t.Loc)
	sd = t.Scale
	if t.Df > 2 {
		sd *= math.Sqrt(t.Df / (t.Df - 2))
	} else {
		sd *= 10 // infinite variance: as good as unknown
	}
	return mean, mean * sd
}
