// Package measure turns probes and real traffic into per-node estimates of
// rate and RTT, plus a circuit breaker. Everything is measured against the
// Emby server itself.
package measure

import (
	"math"
	"time"
)

// minKnown is the evidence, in samples, that makes an estimate trusted.
const minKnown = 3

// Estimate is a running mean and spread of log values whose evidence halves
// every half-life: an exponentially weighted mean and variance, and the
// weight of the samples behind them. A node left alone keeps its mean but
// loses weight until it is no longer known, and is tested again.
type Estimate struct {
	Mean   float64   `json:"mean"`   // of the log values
	Var    float64   `json:"var"`    // of one value around the mean
	Weight float64   `json:"weight"` // samples' worth, faded
	At     time.Time `json:"at"`     // of the last sample
}

// Observe folds in the value x after fading old evidence by
// 2^(−Δt/halfLife).
func (e *Estimate) Observe(x float64, now time.Time, halfLife time.Duration) {
	*e = e.AsOf(now, halfLife)
	w := e.Weight + 1
	d := x - e.Mean
	e.Mean += d / w
	e.Var += (d*(x-e.Mean) - e.Var) / w
	e.Weight, e.At = w, now
}

// AsOf returns the estimate as of now: its evidence faded, its values kept.
func (e Estimate) AsOf(now time.Time, halfLife time.Duration) Estimate {
	if !e.At.IsZero() && now.After(e.At) {
		e.Weight *= math.Exp2(-now.Sub(e.At).Seconds() / halfLife.Seconds())
	}
	return e
}

// Known reports whether enough recent samples back the estimate.
func (e Estimate) Known() bool { return e.Weight >= minKnown }

// Measured reports whether the estimate holds any sample.
func (e Estimate) Measured() bool { return !e.At.IsZero() }

// Typical is the typical value on the linear scale.
func (e Estimate) Typical() float64 { return math.Exp(e.Mean) }

// Low is a cautious value, one spread under the typical: what a rate keeps
// to most of the time. It is 0 with no samples.
func (e Estimate) Low() float64 {
	if !e.Measured() {
		return 0
	}
	return math.Exp(e.Mean - math.Sqrt(e.Var))
}

// Range is the 90% range of one sample on the linear scale.
func (e Estimate) Range() (lo, hi float64) {
	sd := 1.645 * math.Sqrt(e.Var)
	return math.Exp(e.Mean - sd), math.Exp(e.Mean + sd)
}

// with returns e and o pooled, as if one estimate had seen both's samples.
func (e Estimate) with(o Estimate) Estimate {
	if o.Weight == 0 {
		return e
	}
	if e.Weight == 0 {
		return o
	}
	w := e.Weight + o.Weight
	m := (e.Weight*e.Mean + o.Weight*o.Mean) / w
	v := (e.Weight*(e.Var+sq(e.Mean-m)) + o.Weight*(o.Var+sq(o.Mean-m))) / w
	at := e.At
	if o.At.After(at) {
		at = o.At
	}
	return Estimate{Mean: m, Var: v, Weight: w, At: at}
}

func sq(x float64) float64 { return x * x }
