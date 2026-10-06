package measure

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestEstimateLearns(t *testing.T) {
	var e Estimate
	r := rand.New(rand.NewPCG(1, 2))
	t0 := time.Unix(0, 0)
	for i := range 200 {
		e.Observe(math.Log(50)+0.2*r.NormFloat64(), t0.Add(time.Duration(i)*time.Second), 2*time.Hour)
	}
	if got := e.Typical(); math.Abs(got-50) > 3 {
		t.Errorf("typical %.1f Mbps, want ≈ 50", got)
	}
	if sd := math.Sqrt(e.Var); math.Abs(sd-0.2) > 0.03 {
		t.Errorf("spread %.3f, want ≈ 0.2", sd)
	}
	if lo := e.Low(); math.Abs(lo-50*math.Exp(-0.2)) > 3 {
		t.Errorf("low %.1f Mbps, want ≈ %.1f", lo, 50*math.Exp(-0.2))
	}
}

func TestFadeKeepsValues(t *testing.T) {
	var e Estimate
	t0 := time.Unix(0, 0)
	for i := range 10 {
		e.Observe(float64(i%2), t0, time.Hour)
	}
	later := e.AsOf(t0.Add(time.Hour), time.Hour)
	if math.Abs(later.Weight-e.Weight/2) > 1e-9 || later.Mean != e.Mean || later.Var != e.Var {
		t.Errorf("after one half-life %+v, from %+v: want only the weight halved", later, e)
	}
	if !e.Known() || e.AsOf(t0.Add(3*time.Hour), time.Hour).Known() {
		t.Error("10 samples should be known now, and not 3 half-lives on")
	}
}

// TestWithPools: pooling two estimates is one estimate of both's samples.
func TestWithPools(t *testing.T) {
	var a, b, both Estimate
	t0 := time.Unix(0, 0)
	for i, x := range []float64{1, 2, 3, 7, 8} {
		if i < 3 {
			a.Observe(x, t0, time.Hour)
		} else {
			b.Observe(x, t0, time.Hour)
		}
		both.Observe(x, t0, time.Hour)
	}
	got := a.with(b)
	if math.Abs(got.Mean-both.Mean) > 1e-12 || math.Abs(got.Var-both.Var) > 1e-12 || got.Weight != both.Weight {
		t.Errorf("a with b = %+v, one estimate of all = %+v", got, both)
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
