package measure

import (
	"math"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/nodes"
)

func rateSample(mbps float64, at time.Time) Sample {
	return Sample{Kind: KindPassive, Bytes: int64(mbps * 1e6 / 8 * Window.Seconds()), Dur: Window, Time: at}
}

// TestRateNowFadesBackToTypical: a sag half a minute old decides a node's
// rate now; ten minutes on, the node is judged by its typical rate again,
// worth one sample, as a node not seen lately.
func TestRateNowFadesBackToTypical(t *testing.T) {
	s := NewStats(config.Static(config.Default()), "")
	n := &nodes.Node{ID: "a", Name: "a"}
	t0 := time.Unix(1e9, 0)
	for i := range 60 {
		s.Record(n, rateSample([]float64{55, 60, 65}[i%3], t0.Add(-time.Hour+time.Duration(i)*Window)))
	}
	for i := range 10 {
		s.Record(n, rateSample(10, t0.Add(-30*time.Second+time.Duration(i)*Window)))
	}

	sagged := s.StateAt(n, t0)
	if got := math.Exp(sagged.Now.Mu); got > 15 {
		t.Errorf("half a minute after a sag to 10 Mbps: rate now %.1f Mbps, want ≈ 10", got)
	}
	if got := math.Exp(sagged.Rate.Mu); got < 40 {
		t.Errorf("typical rate %.1f Mbps, want it barely moved by the sag", got)
	}

	later := s.StateAt(n, t0.Add(10*time.Minute))
	if now, typ := math.Exp(later.Now.Mu), math.Exp(later.Rate.Mu); math.Abs(now-typ)/typ > 0.01 {
		t.Errorf("ten minutes on: rate now %.1f Mbps, want the typical %.1f", now, typ)
	}
	if k := later.Now.Kappa; math.Abs(k-1) > 0.01 {
		t.Errorf("ten minutes on: rate now worth %.2f samples, want 1", k)
	}
}

// TestAddWeighsLikeRepeats: a sample worth 2 is two samples.
func TestAddWeighsLikeRepeats(t *testing.T) {
	a, b := NewBelief(1, 0.5, 2), NewBelief(1, 0.5, 2)
	a.add(3, 2)
	b.add(3, 1)
	b.add(3, 1)
	if math.Abs(a.Mu-b.Mu) > 1e-12 || math.Abs(a.Kappa-b.Kappa) > 1e-12 ||
		math.Abs(a.Alpha-b.Alpha) > 1e-12 || math.Abs(a.Beta-b.Beta) > 1e-12 {
		t.Errorf("add(x, 2) = %+v, two add(x, 1) = %+v", a, b)
	}
}
