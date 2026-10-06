package control

import (
	"math"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
)

func TestBurst(t *testing.T) {
	at := func(rttMs, mbps float64) measure.State {
		var st measure.State
		st.RTT.Observe(rttMs, time.Now())
		if mbps > 0 {
			st.Rate.Observe(mbps, time.Now())
		}
		return st
	}
	// The 2026-10-05 pool: 43 ms more RTT buys 6× the speed, and a page of
	// images in 0.3 s instead of 2 s.
	if quick, nearby := burstMs(at(273, 75)), burstMs(at(230, 12)); quick+primaryMargin >= nearby {
		t.Errorf("273 ms at 75 Mbps: %.0f ms, 230 ms at 12 Mbps: %.0f ms; want the first far quicker", quick, nearby)
	}
	if got, want := burstMs(at(100, 0)), 4*100+burstMbit*1000/guessMbps; math.Abs(got-want) > 1e-6 {
		t.Errorf("unmeasured rate: %.0f ms, want %.0f at the guess", got, want)
	}
	if got := burstMs(measure.State{}); !math.IsInf(got, 1) {
		t.Errorf("unmeasured RTT: %.0f ms, want +Inf", got)
	}
}

func TestFasterRanksMeasuredFirst(t *testing.T) {
	var slow, fast, unknown, pinged measure.State
	slow.Rate.Observe(5, time.Now())
	fast.Rate.Observe(50, time.Now())
	pinged.RTT.Observe(80, time.Now())
	for _, tc := range []struct {
		name string
		a, b measure.State
	}{
		{"higher rate", fast, slow},
		{"measured over unmeasured", slow, unknown},
		{"pinged over not, both unmeasured", pinged, unknown},
	} {
		if faster(tc.a, tc.b) >= 0 || faster(tc.b, tc.a) <= 0 {
			t.Errorf("%s: want a before b", tc.name)
		}
	}
}
