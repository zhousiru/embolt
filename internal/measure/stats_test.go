package measure

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/nodes"
)

func TestRecentSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store := config.Static(config.Default())
	samples, beliefs := filepath.Join(dir, "samples"), filepath.Join(dir, "beliefs.json")
	a, b := &nodes.Node{ID: "a"}, &nodes.Node{ID: "b"}

	// Before: an explored stretch on a, then more pings than fit, spread over two days.
	before := NewStats(store, samples)
	t.Cleanup(func() { before.log.file.Close() }) // Windows cannot remove an open file
	t0 := time.Now().Add(-26 * time.Hour)
	before.Record(a, Sample{Kind: KindExplore, Time: t0, Bytes: 10e6, Dur: 2 * time.Second})
	for i := range recentLen + 5 {
		before.Record(b, Sample{Kind: KindPing, Time: t0.Add(time.Duration(i) * time.Hour), TTFB: 100 * time.Millisecond})
	}
	before.Record(a, Sample{Kind: KindPing, Time: time.Now().Add(-time.Minute), TTFB: 90 * time.Millisecond})
	if err := before.Save(beliefs); err != nil {
		t.Fatal(err)
	}

	after := NewStats(store, samples)
	t.Cleanup(func() { after.log.file.Close() })
	if err := after.Restore(beliefs); err != nil {
		t.Fatal(err)
	}
	live := Sample{Kind: KindPing, Time: time.Now(), TTFB: 80 * time.Millisecond}
	after.Record(a, live) // a ping that lands before the log is read
	if err := after.restoreRecent(); err != nil {
		t.Fatal(err)
	}

	got := after.State(a).Recent
	if len(got) != 3 || got[0].Kind != KindExplore || got[2].TTFB != live.TTFB {
		t.Errorf("a's recent = %+v, want the explored stretch, the ping, then the live ping once", got)
	}
	gotB := after.State(b).Recent
	if len(gotB) != recentLen || !gotB[0].Time.Equal(t0.Add(5*time.Hour)) {
		t.Errorf("b's recent: %d samples from %v, want the last %d", len(gotB), gotB[0].Time, recentLen)
	}
}
