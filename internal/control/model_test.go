package control

import (
	"math"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
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

func TestBurst(t *testing.T) {
	pinged := func(rttMs float64, pings int) measure.State {
		var st measure.State
		for range pings {
			st.RTT.Observe(math.Log(rttMs), time.Now(), time.Hour)
		}
		return st
	}
	at := func(rttMs, mbps float64) measure.State {
		st := pinged(rttMs, 100)
		now := time.Now()
		if mbps > 0 {
			st.Rate.Observe(math.Log(mbps), now, time.Hour)
		}
		return st
	}
	// The 2026-10-05 pool: 43 ms more RTT buys 6× the speed, and a page of
	// images in 0.3 s instead of 2 s.
	if quick, nearby := burstMs(at(273, 75)), burstMs(at(230, 12)); quick+primaryMargin >= nearby {
		t.Errorf("273 ms at 75 Mbps: %.0f ms, 230 ms at 12 Mbps: %.0f ms; want the first far quicker", quick, nearby)
	}
	if st := at(100, 0); math.Abs(burstMs(st)-(4*st.RTT.Upside()+burstMbit*1000/guessMbps)) > 1e-6 {
		t.Errorf("unmeasured rate: %.0f ms, want it at the guess", burstMs(st))
	}
	if once, often := burstMs(pinged(100, 1)), burstMs(pinged(100, 100)); once <= often+primaryMargin {
		t.Errorf("pinged once: %.0f ms, pinged often: %.0f ms; want the first counted far slower", once, often)
	}
	if got := burstMs(measure.State{}); !math.IsInf(got, 1) {
		t.Errorf("unmeasured RTT: %.0f ms, want +Inf", got)
	}
}
