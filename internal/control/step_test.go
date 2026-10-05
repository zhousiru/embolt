package control

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

func TestExpectedStall(t *testing.T) {
	known := func(mbps float64) measure.Belief { return measure.NewBelief(math.Log(mbps), 0.01, 1e4) }
	// Need at B = 0 for 20 Mbps video is 21.67 Mbps over 120 s.
	if s := expectedStall(known(200), 0, 20); s > 1e-6 {
		t.Errorf("fast node: %.3f s of stall, want 0", s)
	}
	want := 120.0 / 20 * (20*(1+10.0/120) - 10) // (H/V)·(R_req − r) at r = 10 Mbps
	if s := expectedStall(known(10), 0, 20); math.Abs(s-want) > 0.05 {
		t.Errorf("10 Mbps node: %.2f s of stall, want %.2f", s, want)
	}
	if s := expectedStall(measure.Belief{}, 0, 20); math.Abs(s-120.0/20*20*(1+10.0/120)) > 1e-9 {
		t.Errorf("dead node: %.2f s of stall", s)
	}
}

// testController has two direct nodes, a and b, with the given rate beliefs.
func testController(t *testing.T, rates map[string][]float64) (*Controller, map[string]*nodes.Node) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
upstream: {url: "http://127.0.0.1:1"}
proxies:
  - {name: a, type: direct, udp: false}
  - {name: b, type: direct, udp: true}
data_dir: ` + t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	store := config.Static(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := nodes.NewPool(store)
	go pool.Run(ctx)
	<-pool.Updated()
	stats := measure.NewStats(store, "")
	byName := map[string]*nodes.Node{}
	for _, n := range pool.All() {
		byName[n.Name] = n
		for _, r := range rates[n.Name] {
			stats.Record(n, measure.Sample{Kind: measure.KindPassive, Bytes: int64(r * 1e6 / 8 * 2), Dur: measure.Window})
		}
	}
	return New(store, pool, stats), byName
}

// TestStepLeavesASaggingNodeAtOnce replays 14:53 of the 2026-10-05 retest:
// the media node had streamed at ~500 Mbps minutes earlier, then delivered
// 2.5 and 5.8 Mbps on a 22 Mbps video with nothing buffered. With a healthy
// standby, the first step with those samples must switch.
func TestStepLeavesASaggingNodeAtOnce(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {517, 571, 353, 692}, "b": {110, 120, 95, 130}})
	s, err := c.Play("tv/593931", 22.2)
	if err != nil {
		t.Fatal(err)
	}
	if s.Node() != n["a"] {
		t.Fatalf("media node %v, want a", s.Node())
	}
	obs := Observation{Delivered: 2 * time.Second, Elapsed: 6 * time.Second, Recent: []float64{2.5, 5.8}}
	if got := s.Step(obs); got != n["b"] {
		t.Errorf("Step = %v, want a switch to b", got)
	}
}

func TestStepStaysWhenThePlayerHasBuffer(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {30, 32, 28}, "b": {5, 6, 5}})
	s, _ := c.Play("tv/1", 22.2)
	if s.Node() != n["a"] {
		t.Fatalf("media node %v, want a", s.Node())
	}
	// The stream runs a little above the bitrate and the player holds ~40 s:
	// no reason to spend an exit IP.
	obs := Observation{Delivered: 70 * time.Second, Elapsed: 30 * time.Second, Recent: []float64{26, 24, 27}}
	if got := s.Step(obs); got != nil {
		t.Errorf("Step = %v, want stay (risk %.4f)", got, s.risk)
	}
	if got := s.Step(Observation{Full: true}); got != nil {
		t.Errorf("Step with a full read-ahead = %v, want stay", got)
	}
}

func TestOnlyTheLeadStreamDecides(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {517, 571, 353, 692}, "b": {110, 120, 95, 130}})
	play, _ := c.Play("tv/1", 22.2)
	if play.Node() != n["a"] {
		t.Fatalf("media node %v, want a", play.Node())
	}
	scan, _ := c.Play("tv/1", 22.2)
	play.Step(Observation{Delivered: 30 * time.Second, Elapsed: 20 * time.Second, Full: true})
	// The scan reads a little, then nothing: alone it would look like a stall.
	if got := scan.Step(Observation{Delivered: time.Second, Elapsed: 6 * time.Second, Recent: []float64{1, 1}}); got != nil {
		t.Errorf("a side connection switched the session to %v", got)
	}
	// The same starvation on the lead stream does move the session.
	if got := play.Step(Observation{Delivered: 30 * time.Second, Elapsed: 40 * time.Second, Recent: []float64{1, 1}}); got != n["b"] {
		t.Errorf("lead stream starving: Step = %v, want a switch to b", got)
	}
}

// TestPrimaryWeighsSpeed: the primary carries images too, so a node 43 ms
// slower to answer but 6× faster to deliver wins it.
func TestPrimaryWeighsSpeed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rate       map[string][]float64
		rttA, rttB time.Duration
		want       string
	}{
		{"faster node wins", map[string][]float64{"a": {12, 11, 13, 12}, "b": {75, 70, 80, 76}}, 230 * time.Millisecond, 273 * time.Millisecond, "b"},
		{"same speed, lower RTT wins", map[string][]float64{"a": {50, 48, 52, 50}, "b": {50, 52, 48, 50}}, 120 * time.Millisecond, 50 * time.Millisecond, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n := testController(t, tc.rate)
			for name, rtt := range map[string]time.Duration{"a": tc.rttA, "b": tc.rttB} {
				for range 4 {
					c.stats.Record(n[name], measure.Sample{Kind: measure.KindPing, TTFB: rtt})
				}
			}
			if got, _ := c.Primary(); got != n[tc.want] {
				t.Errorf("primary %v, want %s (a %.0f ms, b %.0f ms)", got, tc.want, c.burst(n["a"]).mean, c.burst(n["b"]).mean)
			}
		})
	}
}

// TestSessionShowsTheStepsChoice: the session view must judge nodes as Step
// does, and say why it stays or moves.
func TestSessionShowsTheStepsChoice(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {517, 571, 353, 692}, "b": {110, 120, 95, 130}})
	s, _ := c.Play("tv/1", 22.2)
	s.Step(Observation{Full: true})
	d := c.Session("tv/1")
	if d.Session == nil || d.Verdict == nil || d.Verdict.To != nil || d.Verdict.Reason != "read-ahead full" {
		t.Fatalf("full read-ahead: verdict %+v, want stay", d.Verdict)
	}
	if len(d.Choices) != 2 || d.Choices[0].Node.ID != n["a"].ID || d.Choices[0].Role != "media" || d.Choices[1].GapSeconds <= 0 {
		t.Fatalf("choices %+v, want a staying, then b with a gap", d.Choices)
	}

	obs := Observation{Delivered: 2 * time.Second, Elapsed: 6 * time.Second, Recent: []float64{2.5, 5.8}}
	s.Step(obs)
	d = c.Session("tv/1")
	if d.Verdict.To == nil || d.Verdict.To.ID != n["b"].ID {
		t.Errorf("starving: verdict %+v, want a switch to b", d.Verdict)
	}
	if c.Session("tv/2").Session != nil {
		t.Error("an unknown session has a view")
	}
}

func TestUnknownBitrateUsesTheRecentPeak(t *testing.T) {
	c, _ := testController(t, map[string][]float64{"a": {50}, "b": {50}})
	c.Play("tv/1", 80)
	s, _ := c.Play("tv/2", 0)
	if s.Bitrate() != 80 {
		t.Errorf("bitrate %v, want the recent peak 80", s.Bitrate())
	}
}
