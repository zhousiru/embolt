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
// rate now; ten minutes on, the node is judged by its typical rate again.
// Either way the rate now rests on the typical rate's evidence.
func TestRateNowFadesBackToTypical(t *testing.T) {
	s := NewStats(config.Static(config.Default()))
	n := &nodes.Node{ID: "a", Name: "a"}
	t0 := time.Unix(1e9, 0)
	for i := range 60 {
		s.Record(n, rateSample([]float64{55, 60, 65}[i%3], t0.Add(-time.Hour+time.Duration(i)*Window)))
	}
	for i := range 10 {
		s.Record(n, rateSample(10, t0.Add(-30*time.Second+time.Duration(i)*Window)))
	}

	sagged := s.StateAt(n, t0)
	if got := sagged.Now.Typical(); got > 15 {
		t.Errorf("half a minute after a sag to 10 Mbps: rate now %.1f Mbps, want ≈ 10", got)
	}
	if got := sagged.Rate.Typical(); got < 40 {
		t.Errorf("typical rate %.1f Mbps, want it barely moved by the sag", got)
	}

	later := s.StateAt(n, t0.Add(10*time.Minute))
	if now, typ := later.Now.Typical(), later.Rate.Typical(); math.Abs(now-typ)/typ > 0.01 {
		t.Errorf("ten minutes on: rate now %.1f Mbps, want the typical %.1f", now, typ)
	}
	if later.Now.Weight != later.Rate.Weight {
		t.Errorf("ten minutes on: rate now worth %.2f samples, want the typical rate's %.2f", later.Now.Weight, later.Rate.Weight)
	}
}
