package measure

import (
	"math"
	"testing"
	"time"
)

func TestEstimateFollowsRecentSamples(t *testing.T) {
	var e Estimate
	now := time.Unix(1e9, 0)
	e.Observe(100, now)
	if e.Value != 100 || !e.Measured() {
		t.Fatalf("first sample: %+v, want it taken as is", e)
	}
	for range 3 {
		e.Observe(10, now)
	}
	// A sag of three samples outweighs what came before.
	if want := 10 + 90*math.Pow(1-alpha, 3); math.Abs(e.Value-want) > 1e-9 || e.Value > 45 {
		t.Errorf("after a sag: %.1f, want %.1f", e.Value, want)
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
