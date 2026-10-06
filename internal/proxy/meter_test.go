package proxy

import (
	"math"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
)

// read feeds the meter chunks of 64 KB at mbps and returns the sample rates.
func readAt(m *meter, mbps float64, d time.Duration) []float64 {
	per := time.Duration(float64(chunkSize*8) / (mbps * 1e6) * float64(time.Second))
	var got []float64
	for t := time.Duration(0); t < d; t += per {
		if s, ok := m.read(chunkSize, per); ok {
			got = append(got, s.Mbps())
		}
	}
	return got
}

func TestMeterSamplesOnlyNetworkLimitedReading(t *testing.T) {
	var m meter
	m.restart()
	if got := readAt(&m, 40, 900*time.Millisecond); len(got) != 0 {
		t.Fatalf("samples during the ramp: %v", got)
	}
	got := readAt(&m, 40, 5*time.Second)
	if len(got) != 2 || math.Abs(got[0]-40) > 1 {
		t.Fatalf("after the ramp: %v, want two ≈ 40 Mbps windows", got)
	}

	// A pause: the burst that piled up while waiting reads in no time, and
	// must not count as a 40 000 Mbps window.
	m.paused()
	if s, ok := m.read(4<<20, time.Microsecond); ok {
		t.Fatalf("burst after a pause became a sample: %.0f Mbps", s.Mbps())
	}
	if got := readAt(&m, 40, 800*time.Millisecond); len(got) != 0 {
		t.Fatalf("samples during the ramp after a pause: %v", got)
	}

	if !m.full() {
		t.Fatal("a pause did not mark the read-ahead full")
	}
	if m.full() {
		t.Error("full was not cleared")
	}
}

func TestMeterCountsAStall(t *testing.T) {
	var m meter
	got := readAt(&m, 0.2, 3*time.Second) // a node down to 0.2 Mbps
	if len(got) == 0 || got[0] > 0.5 {
		t.Fatalf("stalled reading: %v, want a very slow sample", got)
	}
}

// TestMeterEnd: a reading that is over yields its open window, or, if no
// window completed, everything it read.
func TestMeterEnd(t *testing.T) {
	m := meter{kind: measure.KindExplore}
	m.restart()
	readAt(&m, 100, 4500*time.Millisecond) // the ramp, one window, 1.5 s open
	s, ok := m.end()
	if !ok || s.Kind != measure.KindExplore || math.Abs(s.Mbps()-100) > 2 {
		t.Fatalf("end = %+v (%.1f Mbps), %v; want the open window at 100 Mbps", s, s.Mbps(), ok)
	}

	// A fast node reads a whole test within its ramp: the whole read is its
	// sample, ramp and all.
	m = meter{kind: measure.KindExplore}
	m.restart()
	m.read(8<<20, 500*time.Millisecond)
	s, ok = m.end()
	if !ok || math.Abs(s.Mbps()-134.2) > 1 {
		t.Fatalf("end = %+v (%.1f Mbps), %v; want the whole read at 134 Mbps", s, s.Mbps(), ok)
	}
	if _, ok := (&meter{}).end(); ok {
		t.Error("a meter that read nothing gave a sample")
	}
}

// TestMeterIsUpPastItsRamp: an attempt is up once it reads past its ramp,
// and stays up through a pause; a new attempt is not up until its own ramp
// is over.
func TestMeterIsUpPastItsRamp(t *testing.T) {
	var m meter
	m.begin()
	readAt(&m, 40, 900*time.Millisecond)
	if m.upSince(time.Now()) {
		t.Fatal("up during the ramp")
	}
	readAt(&m, 40, 500*time.Millisecond)
	now := time.Now()
	if !m.upSince(now) {
		t.Fatal("not up past the ramp")
	}
	if m.upSince(now.Add(-time.Minute)) {
		t.Error("up since before it began")
	}
	m.paused()
	readAt(&m, 40, 100*time.Millisecond)
	if !m.upSince(now) {
		t.Error("a pause ended the attempt")
	}
	m.begin()
	if m.upSince(time.Now()) {
		t.Error("a new attempt is up before its ramp")
	}
}
