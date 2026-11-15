package control

import (
	"testing"

	"github.com/zhousiru/embolt/internal/measure"
)

// A node whose latest attempt failed is not chosen as primary over one that
// answered, however quick its old estimates make it look; a node not tried
// yet comes between them.
func TestPrimaryPrefersNodesThatAnswer(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {20}})
	c.stats.Record(n["a"], measure.Sample{Kind: measure.KindPassive, Err: "TLS handshake timeout"})
	if p, _ := c.Primary(); p != n["b"] {
		t.Fatalf("primary %v, want b: a just failed", p)
	}
	c.stats.Record(n["b"], measure.Sample{Kind: measure.KindPassive, Err: "EOF"})
	if alt := c.PrimaryFailed(n["b"]); alt != n["c"] {
		t.Fatalf("after b failed: %v, want c, not tried yet, over a, which failed", alt)
	}
}

// A primary that failed its latest attempt gives way at the next ping round
// to a node that answered, even while something plays.
func TestReconsiderLeavesAFailedPrimary(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {20}})
	if p, _ := c.Primary(); p != n["a"] {
		t.Fatalf("primary %v, want a", p)
	}
	c.Play("tv/1", 10)
	c.stats.Record(n["a"], measure.Sample{Kind: measure.KindPing, Err: "i/o timeout"})
	c.reconsiderPrimary()
	if c.primary != n["b"] {
		t.Fatalf("primary %v, want b", c.primary)
	}
}
