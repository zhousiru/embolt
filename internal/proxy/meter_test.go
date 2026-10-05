package proxy

import (
	"math"
	"testing"
	"time"
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

	full, recent := m.take()
	if !full || len(recent) != 2 {
		t.Fatalf("take = %v, %v; want full and the 2 samples", full, recent)
	}
	if full, _ := m.take(); full {
		t.Error("full was not cleared by take")
	}
	m.moved()
	if _, recent := m.take(); len(recent) != 0 {
		t.Errorf("samples survived a move to another node: %v", recent)
	}
}

func TestMeterCountsAStall(t *testing.T) {
	var m meter
	got := readAt(&m, 0.2, 3*time.Second) // a node down to 0.2 Mbps
	if len(got) == 0 || got[0] > 0.5 {
		t.Fatalf("stalled reading: %v, want a very slow sample", got)
	}
}
