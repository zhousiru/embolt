// Package measure turns probes and real traffic into per-node beliefs: a
// Normal–Inverse-Gamma posterior for log-rate and one for log-RTT, plus a
// circuit breaker. Everything is measured against the Emby server itself.
package measure

import (
	"math"
	"time"
)

// minEvidence keeps a long-idle belief from fading into an improper one.
const minEvidence = 0.5

// Belief is a Normal–Inverse-Gamma posterior over a quantity with unknown
// mean and variance: σ² ~ InvGamma(Alpha, Beta), μ | σ² ~ N(Mu, σ²/Kappa).
type Belief struct {
	Mu    float64   `json:"mu"`
	Kappa float64   `json:"kappa"`
	Alpha float64   `json:"alpha"`
	Beta  float64   `json:"beta"`
	At    time.Time `json:"at"`
}

// NewBelief starts from mean mu and spread sigma, worth n samples.
func NewBelief(mu, sigma, n float64) Belief {
	alpha := 1 + n/2 // keeps E[σ²] = Beta/(Alpha-1) = sigma² finite
	return Belief{Mu: mu, Kappa: n, Alpha: alpha, Beta: sigma * sigma * (alpha - 1)}
}

// Observe folds in the sample x after fading old evidence by
// 2^(−Δt/halfLife).
func (b *Belief) Observe(x float64, now time.Time, halfLife time.Duration) {
	b.fade(now, halfLife)
	b.add(x, 1)
	b.At = now
}

// add folds in the sample x worth w samples.
func (b *Belief) add(x, w float64) {
	k := b.Kappa + w
	d := x - b.Mu
	b.Beta += b.Kappa * w * d * d / (2 * k)
	b.Mu += w * d / k
	b.Kappa = k
	b.Alpha += w / 2
}

// Measured reports whether the belief holds any real sample.
func (b Belief) Measured() bool { return !b.At.IsZero() }

// AsOf returns the belief as of now, faded but not updated.
func (b Belief) AsOf(now time.Time, halfLife time.Duration) Belief {
	b.fade(now, halfLife)
	return b
}

// Capped returns the belief with its typical value worth at most n samples:
// the prior for a node's rate now, which its recent samples then decide. The
// jitter estimate is kept, since a node's rate drifts but its jitter much
// less.
func (b Belief) Capped(n float64) Belief {
	b.Kappa = min(b.Kappa, n)
	return b
}

// fade scales all evidence alike, which keeps the variance estimate and
// widens the uncertainty, so an idle node drifts back towards "unknown".
func (b *Belief) fade(now time.Time, halfLife time.Duration) {
	if b.Measured() && now.After(b.At) {
		f := math.Exp2(-now.Sub(b.At).Seconds() / halfLife.Seconds())
		if b.Kappa*f < minEvidence {
			f = min(1, minEvidence/b.Kappa)
		}
		b.scale(f)
		b.At = now
	}
}

func (b *Belief) scale(f float64) {
	b.Kappa *= f
	b.Alpha *= f
	b.Beta *= f
}

// Average is the distribution of the mean of the next k observations. One
// observation's jitter shrinks by 1/k; the doubt about μ itself does not.
func (b Belief) Average(k float64) StudentT {
	return StudentT{Df: 2 * b.Alpha, Loc: b.Mu, Scale: math.Sqrt(b.Beta / b.Alpha * (1/b.Kappa + 1/k))}
}

// Predictive is the distribution of the next observation, jitter and all.
func (b Belief) Predictive() StudentT { return b.Average(1) }

// Mean is the posterior of μ: which value is typical, a doubt that shrinks
// with data. Used to compare nodes and to pick what to probe.
func (b Belief) Mean() StudentT { return b.Average(math.Inf(1)) }

// StudentT is a location-scale Student-t distribution.
type StudentT struct{ Df, Loc, Scale float64 }

func (t StudentT) CDF(x float64) float64 {
	z := (x - t.Loc) / t.Scale
	tail := 0.5 * regIncBeta(t.Df/2, 0.5, t.Df/(t.Df+z*z))
	if z > 0 {
		return 1 - tail
	}
	return tail
}

// Quantile inverts CDF by bisection.
func (t StudentT) Quantile(p float64) float64 {
	lo, hi := t.Loc-1e3*t.Scale, t.Loc+1e3*t.Scale
	for range 100 {
		mid := (lo + hi) / 2
		if t.CDF(mid) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// regIncBeta is the regularized incomplete beta function I_x(a, b),
// by Lentz's continued fraction.
func regIncBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	if x > (a+1)/(a+b+2) {
		return 1 - regIncBeta(b, a, 1-x)
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab-la-lb+a*math.Log(x)+b*math.Log1p(-x)) / a

	const tiny = 1e-300
	f, c, d := 1.0, 1.0, 0.0
	for i := range 300 {
		m := float64(i / 2)
		var num float64
		switch {
		case i == 0:
			num = 1
		case i%2 == 0:
			num = m * (b - m) * x / ((a + 2*m - 1) * (a + 2*m))
		default:
			num = -(a + m) * (a + b + m) * x / ((a + 2*m) * (a + 2*m + 1))
		}
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		d = 1 / d
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		f *= c * d
		if math.Abs(1-c*d) < 1e-12 {
			break
		}
	}
	return front * (f - 1)
}
