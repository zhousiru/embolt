// Package measure turns probes and real traffic into per-node estimates of
// rate and RTT, plus a circuit breaker. Everything is measured against the
// Emby server itself.
package measure

import (
	"math"
	"time"
)

// doubt is the spread of a mean that rests on one sample, in log units: the
// mean's own spread is doubt/√weight. It keeps a mean that rests on a few
// samples, or on old ones, from passing for a sure one.
const doubt = 0.5

// Estimate is a running mean and spread of log values whose evidence halves
// every half-life: an exponentially weighted mean and variance, and the
// weight of the samples behind them. A node left alone keeps its mean but
// loses weight, so its mean is doubted more and more, until it is tested
// again.
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

// Measured reports whether the estimate holds any sample.
func (e Estimate) Measured() bool { return !e.At.IsZero() }

// Typical is the typical value on the linear scale.
func (e Estimate) Typical() float64 { return math.Exp(e.Mean) }

// Low is a cautious value, one spread under the typical: what a rate keeps
// to most of the time. The spread counts the mean's own doubt, so a value
// that rests on little evidence is low too. It is 0 with no evidence.
func (e Estimate) Low() float64 {
	if e.Weight <= 0 {
		return 0
	}
	return math.Exp(e.Mean - e.spread())
}

// Upside is the typical value were the mean one doubt too low: how high it
// may be, given the evidence. It is +Inf with none.
func (e Estimate) Upside() float64 {
	if e.Weight <= 0 {
		return math.Inf(1)
	}
	return math.Exp(e.Mean + doubt/math.Sqrt(e.Weight))
}

// spread is one value's spread around the mean, plus the mean's own doubt.
func (e Estimate) spread() float64 { return math.Sqrt(e.Var + doubt*doubt/e.Weight) }

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
