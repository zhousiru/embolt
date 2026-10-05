package control

import (
	"math"
	"testing"
	"time"

	"github.com/siruzhou/embolt/internal/config"
	"github.com/siruzhou/embolt/internal/measure"
)

func TestRequiredRate(t *testing.T) {
	for _, tc := range []struct {
		buffer time.Duration
		want   float64
	}{
		{60 * time.Second, 0.583}, // a full buffer
		{0, 1.083},                // play start
		{130 * time.Second, 0},    // enough buffer to ride out the horizon
	} {
		if got := requiredMbps(tc.buffer, 1); math.Abs(got-tc.want) > 1e-3 {
			t.Errorf("required at B=%v: %.3f V, want %.3f V", tc.buffer, got, tc.want)
		}
	}
}

func TestStallRisk(t *testing.T) {
	p := config.Default().Control
	steady := measure.NewBelief(math.Log(60), 0.1, 50) // 60 Mbps, little jitter
	jumpy := measure.NewBelief(math.Log(60), 0.8, 50)  // same median, much jitter
	slow := measure.NewBelief(math.Log(30), 0.1, 50)   // under a 40 Mbps stream
	risk := func(b measure.Belief, buffer time.Duration) float64 { return stallRisk(b, buffer, 40) }

	if r := risk(steady, 0); r > p.StallRisk {
		t.Errorf("steady 60 Mbps node at 40 Mbps: risk %.4f, want under target", r)
	}
	// Jitter between 2 s windows averages out over 120 s: judged per window,
	// the jumpy node would look unsafe; judged over the horizon it is close.
	need := math.Log(requiredMbps(0, 40))
	if r, single := risk(jumpy, 0), jumpy.Predictive().CDF(need); r > 0.05 || single < 0.3 {
		t.Errorf("jumpy 60 Mbps node: risk %.4f over the horizon (want < 5%%), %.4f per window", r, single)
	}
	if r := risk(slow, 0); r < 0.99 {
		t.Errorf("30 Mbps node on a 40 Mbps stream: risk %.4f, want ≈ 1", r)
	}
	if r := risk(slow, 130*time.Second); r != 0 {
		t.Errorf("risk with a buffer beyond the horizon: %v, want 0", r)
	}
	// Doubt about the mean does not average out: two vague samples stay risky.
	if r := risk(measure.NewBelief(math.Log(60), 0.8, 2), 0); r < 0.05 {
		t.Errorf("vague belief: risk %.4f, want well above target", r)
	}
}

// TestStreamRateSeesASag replays the first switch of the 2026-10-05 test: a
// node believed fast, whose stream fell from 9 to 1.3 Mbps on a 5.1 Mbps
// video with a nearly empty buffer. Its own samples must override the belief.
func TestStreamRateSeesASag(t *testing.T) {
	p := config.Default().Control
	now := time.Now()
	node := measure.NewBelief(math.Log(80), 0.3, 200)
	fine := streamRate(p, node, []float64{60, 70, 65}, now)
	sagging := streamRate(p, node, []float64{9.0, 6.3, 4.7, 1.3}, now)
	if r := stallRisk(fine, 5*time.Second, 5.1); r > p.StallRisk {
		t.Errorf("healthy stream: risk %.4f, want under target", r)
	}
	if r := stallRisk(sagging, 5*time.Second, 5.1); r <= p.StallRisk {
		t.Errorf("sagging stream: risk %.4f, want over target so the controller acts", r)
	}
	// No samples yet is no news: the stream is judged by the node's belief,
	// as calmly as any other node. This stopped the 15:11 flapping.
	if r := stallRisk(streamRate(p, node, nil, now), 20*time.Second, 5.3); r > p.StallRisk/100 {
		t.Errorf("stream without samples on a fast node: risk %.4f, want ≈ 0", r)
	}
}

func TestProbFaster(t *testing.T) {
	ms := func(x float64) measure.Belief { return measure.NewBelief(math.Log(x), 0.1, 40) }
	mbps := ms // same shape: a well-measured log-scale belief
	// At the same speed, RTT decides: 4 round trips of 70 ms more.
	fast, slow := burstTime(ms(50), mbps(50)), burstTime(ms(120), mbps(50))
	if p := probFaster(fast, slow, primaryMargin); p < 0.99 {
		t.Errorf("50 ms vs 120 ms: P = %.3f, want ≈ 1", p)
	}
	if p := probFaster(slow, fast, primaryMargin); p > 0.01 {
		t.Errorf("120 ms vs 50 ms: P = %.3f, want ≈ 0", p)
	}
	// The 2026-10-05 pool: 43 ms more RTT buys 6× the speed, and a page of
	// images in 0.3 s instead of 2 s.
	quick, nearby := burstTime(ms(273), mbps(75)), burstTime(ms(230), mbps(12))
	if p := probFaster(quick, nearby, primaryMargin); p < 0.99 {
		t.Errorf("273 ms at 75 Mbps vs 230 ms at 12 Mbps: P = %.3f, want ≈ 1 (%.0f vs %.0f ms)", p, quick.mean, nearby.mean)
	}
	vague := burstTime(measure.NewBelief(math.Log(50), 1, 1), mbps(50))
	if p := probFaster(vague, slow, primaryMargin); p > primaryConf {
		t.Errorf("one vague sample: P = %.3f, want under %.2f", p, primaryConf)
	}
}
