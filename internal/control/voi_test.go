package control

import (
	"math"
	"testing"
	"time"

	"github.com/siruzhou/embolt/internal/config"
	"github.com/siruzhou/embolt/internal/measure"
)

func TestProbeValue(t *testing.T) {
	p := config.Default().Control
	now := time.Now()
	const bitrate = 20.0                                // needs 21.7 Mbps at play start
	known := measure.NewBelief(math.Log(150), 0.2, 40)  // well measured, far above
	hopeless := measure.NewBelief(math.Log(3), 0.2, 40) // well measured, far below
	vague := measure.NewBelief(math.Log(25), 0.8, 2)    // near the line, barely measured

	if v := probeValue(p, known, 0, bitrate, now); v > minProbeValue {
		t.Errorf("testing a known-fast node again: worth %.3f s", v)
	}
	if v := probeValue(p, hopeless, 0, bitrate, now); v > minProbeValue {
		t.Errorf("testing a known-slow node while a safe one exists: worth %.3f s", v)
	}
	// With no safe node yet, learning whether the vague one passes matters.
	noneSafe := expectedStall(hopeless, 0, bitrate)
	if v := probeValue(p, vague, noneSafe, bitrate, now); v < 10*minProbeValue {
		t.Errorf("testing the vague node when nothing is safe: worth %.3f s, want clearly positive", v)
	}
	// Once a known-fast node exists, the vague one is worth much less.
	if safe, alone := probeValue(p, vague, 0, bitrate, now), probeValue(p, vague, noneSafe, bitrate, now); safe >= alone/10 {
		t.Errorf("vague node: worth %.3f s beside a safe node vs %.3f s alone", safe, alone)
	}
}
