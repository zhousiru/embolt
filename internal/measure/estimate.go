// Package measure turns probes and real traffic into per-node estimates of
// rate and RTT, plus a circuit breaker. Everything is measured against the
// Emby server itself.
package measure

import "time"

// alpha is the weight of each new sample in a moving average: the last three
// samples, one speed test's worth, carry two thirds of it.
const alpha = 0.3

// Estimate is an exponentially weighted moving average, and when it was
// last sampled.
type Estimate struct {
	Value float64   `json:"value"`
	At    time.Time `json:"at"` // of the last sample, zero if none
}

// Observe folds in x. The first sample is taken as is.
func (e *Estimate) Observe(x float64, now time.Time) {
	if e.Measured() {
		x = e.Value + alpha*(x-e.Value)
	}
	e.Value, e.At = x, now
}

// Measured reports whether the estimate holds any sample.
func (e Estimate) Measured() bool { return !e.At.IsZero() }
