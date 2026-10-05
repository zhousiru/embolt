package measure

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestStudentTCDF(t *testing.T) {
	for _, tc := range []struct{ df, x, want float64 }{
		{1, 1, 0.75},            // Cauchy
		{2, 1, 0.7886751345948}, // 1/2 + 1/(2√3)
		{5, 2.015048, 0.95},     // table value
		{30, -1.697261, 0.05},   // table value
		{1e6, 1.959964, 0.975},  // ≈ normal
		{3, 0, 0.5},
	} {
		got := StudentT{Df: tc.df, Scale: 1}.CDF(tc.x)
		if math.Abs(got-tc.want) > 1e-5 {
			t.Errorf("CDF(df=%v, x=%v) = %v, want %v", tc.df, tc.x, got, tc.want)
		}
	}
}

func TestQuantileInvertsCDF(t *testing.T) {
	d := StudentT{Df: 4, Loc: 2, Scale: 0.5}
	for _, p := range []float64{0.05, 0.5, 0.9} {
		if got := d.CDF(d.Quantile(p)); math.Abs(got-p) > 1e-9 {
			t.Errorf("CDF(Quantile(%v)) = %v", p, got)
		}
	}
}

func TestBeliefLearns(t *testing.T) {
	b := NewBelief(math.Log(20), 1, 2)
	r := rand.New(rand.NewPCG(1, 2))
	now := time.Unix(0, 0)
	for i := range 200 {
		b.Observe(math.Log(50)+0.2*r.NormFloat64(), now.Add(time.Duration(i)*time.Second), 2*time.Hour)
	}
	if got := math.Exp(b.Mu); math.Abs(got-50) > 3 {
		t.Errorf("mean = %.1f Mbps, want ≈ 50", got)
	}
	// The predictive keeps the jitter; the mean's posterior shrinks.
	if s := b.Predictive().Scale; math.Abs(s-0.2) > 0.04 {
		t.Errorf("predictive scale = %.3f, want ≈ 0.2", s)
	}
	if s := b.Mean().Scale; s > 0.03 {
		t.Errorf("mean scale = %.3f, want small", s)
	}
}

func TestFadeWidensAndKeepsVariance(t *testing.T) {
	b := NewBelief(0, 1, 2)
	t0 := time.Unix(0, 0)
	for i := range 20 {
		b.Observe(float64(i%2), t0, time.Hour)
	}
	later := b.AsOf(t0.Add(time.Hour), time.Hour)
	if math.Abs(later.Kappa-b.Kappa/2) > 1e-9 {
		t.Errorf("kappa after one half-life = %v, want %v", later.Kappa, b.Kappa/2)
	}
	if math.Abs(later.Beta/later.Alpha-b.Beta/b.Alpha) > 1e-9 {
		t.Error("fade changed the variance estimate")
	}
	if later.Mean().Scale <= b.Mean().Scale {
		t.Error("fade did not widen the mean's posterior")
	}
	if idle := b.AsOf(t0.Add(1000*time.Hour), time.Hour); idle.Kappa < minEvidence-1e-9 {
		t.Errorf("kappa faded to %v, below the floor", idle.Kappa)
	}
}

func TestCappedKeepsJitter(t *testing.T) {
	b := NewBelief(math.Log(100), 0.2, 200)
	c := b.Capped(2)
	if c.Kappa != 2 || c.Alpha != b.Alpha || c.Beta != b.Beta {
		t.Fatalf("Capped(2) = %+v from %+v; want only Kappa cut", c, b)
	}
	// Doubt about the mean grows, the predictive keeps its light tails.
	if c.Mean().Scale <= b.Mean().Scale || c.Predictive().Df != b.Predictive().Df {
		t.Error("Capped did not widen the mean alone")
	}
}

func TestBreaker(t *testing.T) {
	var b breaker
	now := time.Unix(0, 0)
	for i := range 2 {
		if b.record(false, now) {
			t.Fatalf("tripped after %d faults", i+1)
		}
	}
	if !b.record(false, now) || b.until != now.Add(30*time.Second) {
		t.Fatal("3rd fault did not open for 30 s")
	}
	now = b.until
	if !b.record(false, now) || b.until != now.Add(time.Minute) {
		t.Fatal("a fault after reopening did not double the backoff")
	}
	b.record(true, now)
	if b != (breaker{}) {
		t.Fatal("a success did not reset the breaker")
	}
}
