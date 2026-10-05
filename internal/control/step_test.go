package control

import (
	"context"
	"math"
	"strings"
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

// TestBest: every choice meets the target first, by the caller's preference,
// and falls back on the least expected stall when nothing does.
func TestBest(t *testing.T) {
	c, n := testController(t, nil)
	a, b := n["a"], n["b"]
	byName := func(x, y option) int { return strings.Compare(x.n.Name, y.n.Name) }
	if o, ok := c.best([]option{{n: b, risk: 0.001, stall: 0.1}, {n: a, risk: 0.005, stall: 0.5}}, byName); o.n != a || !ok {
		t.Errorf("both pass: picked %v (meets %v), want a by preference", o.n, ok)
	}
	if o, ok := c.best([]option{{n: a, risk: 0.5, stall: 0.1}, {n: b, risk: 0.005, stall: 9}}, byName); o.n != b || !ok {
		t.Errorf("only b passes: picked %v (meets %v), want b", o.n, ok)
	}
	if o, ok := c.best([]option{{n: a, risk: 0.5, stall: 8}, {n: b, risk: 0.9, stall: 3}}, byName); o.n != b || ok {
		t.Errorf("none passes: picked %v (meets %v), want b, the least stall", o.n, ok)
	}
	if o, _ := c.best(nil, byName); o.n != nil {
		t.Errorf("no options: picked %v", o.n)
	}
}

// TestByDraw: Thompson sampling tests the barely measured node that may be
// fast, and the known fast one, but hardly ever the one known to be slow.
func TestByDraw(t *testing.T) {
	slow, fast, vague := &nodes.Node{Name: "slow"}, &nodes.Node{Name: "fast"}, &nodes.Node{Name: "vague"}
	beliefs := map[*nodes.Node]measure.Belief{
		slow:  measure.NewBelief(math.Log(3), 0.2, 40),
		fast:  measure.NewBelief(math.Log(100), 0.2, 40),
		vague: measure.NewBelief(math.Log(60), 1, 1),
	}
	const trials = 2000
	first := map[*nodes.Node]int{}
	for range trials {
		first[byDraw(beliefs)[0]]++
	}
	if first[slow] > trials/100 || first[vague] < trials/20 || first[fast] < trials/4 {
		t.Errorf("first picks of %d: slow %d, vague %d, fast %d", trials, first[slow], first[vague], first[fast])
	}
}

// testController has two direct nodes, a and b, with the given rate samples
// from 10 minutes ago: their typical rates, with nothing seen lately.
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
		record(stats, n, 10*time.Minute, rates[n.Name]...)
	}
	return New(store, pool, stats), byName
}

// cycle is n rate samples cycling through mbps: a rate well known.
func cycle(n int, mbps ...float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = mbps[i%len(mbps)]
	}
	return out
}

// record records rate samples on n, one per window, the last taken age ago.
func record(stats *measure.Stats, n *nodes.Node, age time.Duration, mbps ...float64) {
	last := time.Now().Add(-age)
	for i, r := range mbps {
		at := last.Add(-time.Duration(len(mbps)-1-i) * measure.Window)
		stats.Record(n, measure.Sample{Kind: measure.KindPassive, Bytes: int64(r * 1e6 / 8 * 2), Dur: measure.Window, Time: at})
	}
}

// TestStepLeavesASaggingNodeAtOnce replays 14:53 of the 2026-10-05 retest:
// the media node had streamed at ~500 Mbps minutes earlier, then delivered
// 2.5 and 5.8 Mbps on a 22 Mbps video with nothing buffered. With a healthy
// other node, the first step with those samples must switch.
func TestStepLeavesASaggingNodeAtOnce(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": cycle(20, 517, 571, 353, 692), "b": cycle(20, 110, 120, 95, 130)})
	s, err := c.Play("tv/593931", 22.2)
	if err != nil {
		t.Fatal(err)
	}
	if s.Node() != n["a"] {
		t.Fatalf("media node %v, want a", s.Node())
	}
	record(c.stats, n["a"], 0, 2.5, 5.8)
	obs := Observation{Delivered: 2 * time.Second, Elapsed: 6 * time.Second}
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
	record(c.stats, n["a"], 0, 26, 24, 27)
	obs := Observation{Delivered: 70 * time.Second, Elapsed: 30 * time.Second}
	if got := s.Step(obs); got != nil {
		t.Errorf("Step = %v, want stay (risk %.4f)", got, s.risk)
	}
	if got := s.Step(Observation{Full: true}); got != nil {
		t.Errorf("Step with a full read-ahead = %v, want stay", got)
	}
}

func TestOnlyTheLeadStreamDecides(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": cycle(20, 517, 571, 353, 692), "b": cycle(20, 110, 120, 95, 130)})
	play, _ := c.Play("tv/1", 22.2)
	if play.Node() != n["a"] {
		t.Fatalf("media node %v, want a", play.Node())
	}
	scan, _ := c.Play("tv/1", 22.2)
	play.Step(Observation{Delivered: 30 * time.Second, Elapsed: 20 * time.Second, Full: true})
	// The scan reads a little, then nothing: alone it would look like a stall.
	record(c.stats, n["a"], 0, 1, 1)
	if got := scan.Step(Observation{Delivered: time.Second, Elapsed: 6 * time.Second}); got != nil {
		t.Errorf("a side connection switched the session to %v", got)
	}
	// The same starvation on the lead stream does move the session.
	if got := play.Step(Observation{Delivered: 30 * time.Second, Elapsed: 40 * time.Second}); got != n["b"] {
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

// TestPrimaryMovesOnlyWhenSureAndIdle: a node clearly quicker takes over
// after a ping round, but not while something plays.
func TestPrimaryMovesOnlyWhenSureAndIdle(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {40, 42, 38, 40}})
	ping := func(name string, rtt time.Duration) {
		for range 4 {
			c.stats.Record(n[name], measure.Sample{Kind: measure.KindPing, TTFB: rtt})
		}
	}
	ping("a", 80*time.Millisecond)
	if got, _ := c.Primary(); got != n["a"] {
		t.Fatalf("primary %v, want a, the only measured node", got)
	}
	for _, r := range []float64{200, 210, 190, 205, 195, 200} {
		c.stats.Record(n["b"], measure.Sample{Kind: measure.KindPassive, Bytes: int64(r * 1e6 / 8 * 2), Dur: measure.Window})
	}
	ping("b", 30*time.Millisecond)
	s, _ := c.Play("tv/1", 10)
	c.reconsiderPrimary()
	if c.primary != n["a"] {
		t.Errorf("primary moved to %v during playback", c.primary)
	}
	s.Release()
	c.reconsiderPrimary()
	if c.primary != n["b"] {
		t.Errorf("primary %v once idle, want b (a %.0f ms, b %.0f ms)", c.primary, c.burst(n["a"]).mean, c.burst(n["b"]).mean)
	}
}

// TestSessionShowsTheStepsChoice: the session view must judge nodes as Step
// does, and say why it stays or moves.
func TestSessionShowsTheStepsChoice(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": cycle(20, 517, 571, 353, 692), "b": cycle(20, 110, 120, 95, 130)})
	s, _ := c.Play("tv/1", 22.2)
	s.Step(Observation{Full: true})
	d := c.Session("tv/1")
	if d.Session == nil || d.Verdict == nil || d.Verdict.To != nil || d.Verdict.Reason != "read-ahead full" {
		t.Fatalf("full read-ahead: verdict %+v, want stay", d.Verdict)
	}
	if len(d.Choices) != 2 || d.Choices[0].Node.ID != n["a"].ID || d.Choices[0].Role != "media" || d.Choices[1].GapSeconds <= 0 {
		t.Fatalf("choices %+v, want a staying, then b with a gap", d.Choices)
	}

	record(c.stats, n["a"], 0, 2.5, 5.8)
	obs := Observation{Delivered: 2 * time.Second, Elapsed: 6 * time.Second}
	s.Step(obs)
	d = c.Session("tv/1")
	if d.Verdict.To == nil || d.Verdict.To.ID != n["b"].ID {
		t.Errorf("starving: verdict %+v, want a switch to b", d.Verdict)
	}
	if c.Session("tv/2").Session != nil {
		t.Error("an unknown session has a view")
	}
}

// TestStepDoesNotReturnToANodeItJustLeft replays 14:32 of the 2026-10-05
// session 5e82c7d6: a 40 Mbps video flapping between two nodes every 22 s.
// The session left b when it sagged to ~25 Mbps and now reads a, a little
// under the target. Judged by its typical rate, b looked certain to keep up
// and the session went straight back; judged as a is, by what it did half a
// minute ago, b is worse than staying.
func TestStepDoesNotReturnToANodeItJustLeft(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {40, 44, 37, 42, 39, 41, 36, 45}, "b": cycle(60, 33, 35, 37)})
	a, b := n["a"], n["b"]
	record(c.stats, b, 25*time.Second, 24, 27, 22, 26, 25, 23, 27, 24, 26, 22)
	record(c.stats, a, 0, 37, 35, 39, 36, 38, 34, 37, 36, 38, 35, 37)
	s, _ := c.Play("tv/1512410", 40)
	if s.Node() != a {
		t.Fatalf("media node %v, want a, the faster of late", s.Node())
	}
	const buffer = 40 * time.Second
	if r := stallRisk(c.stats.State(b).Rate, buffer-c.switchGap(b), 40); r > c.cfg.Load().Control.StallRisk {
		t.Fatalf("b's typical rate: risk %.4f, want it to pass, as it did when it drew the session back", r)
	}
	if got := s.Step(Observation{ReadAhead: buffer}); got != nil {
		t.Errorf("Step = %v, want stay: b sagged half a minute ago", got)
	}
	if s.risk <= c.cfg.Load().Control.StallRisk {
		t.Errorf("a's risk %.4f is under target: the step never weighed a move", s.risk)
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

// TestExploreWithinBudget: a session tests at once a node that out-draws its
// media node, whatever its buffer; one test runs at a time, and the next
// waits for the session to earn one again at its bitrate.
func TestExploreWithinBudget(t *testing.T) {
	c, n := testController(t, nil)
	s, _ := c.Play("tv/1", 40)
	media, other := s.Node(), n["a"]
	if media == other {
		other = n["b"]
	}
	record(c.stats, media, 0, cycle(40, 3)...)
	record(c.stats, other, 0, cycle(40, 100)...)
	s.Step(Observation{}) // the media node falls short
	if got := s.Explore(); got != other {
		t.Fatalf("with nothing buffered: Explore = %v, want %v", got, other)
	}
	if got := s.Explore(); got != nil {
		t.Errorf("explored %v while a test runs", got)
	}
	s.Probed(ProbeBytes)
	// 5% of 40 Mbps for 10 s is 2.5 MB: a 32 MiB test takes 14 steps.
	for i := 1; i <= 14; i++ {
		s.earned = s.earned.Add(-time.Minute) // earns for earnGap, not the minute
		got := s.Explore()
		if i < 14 && got != nil {
			t.Fatalf("explored after earning %d times", i)
		}
		if i == 14 && got != other {
			t.Fatalf("Explore = %v once the session earned a test, want %v", got, other)
		}
	}
}

// TestNoTestWhileFine: a session that keeps up, with a fallback known to
// keep up, tests nothing; once the fallback's belief has faded, it does.
func TestNoTestWhileFine(t *testing.T) {
	c, n := testController(t, nil)
	s, _ := c.Play("tv/1", 40)
	media, other := s.Node(), n["a"]
	if media == other {
		other = n["b"]
	}
	record(c.stats, media, 0, cycle(40, 100)...)
	record(c.stats, other, time.Hour, cycle(40, 100)...)
	s.Step(Observation{ReadAhead: time.Minute, Full: true})
	for range 100 {
		if got := s.Explore(); got != nil {
			t.Fatalf("tested %v with the media node and a fallback both keeping up", got)
		}
	}

	c, n = testController(t, nil)
	s, _ = c.Play("tv/1", 40)
	media, other = s.Node(), n["a"]
	if media == other {
		other = n["b"]
	}
	record(c.stats, media, 0, cycle(40, 100)...)
	record(c.stats, other, 24*time.Hour, cycle(40, 100)...)
	s.Step(Observation{ReadAhead: time.Minute, Full: true})
	for range 100 {
		if s.Explore() == other {
			return
		}
	}
	t.Error("never tested a fallback last seen a day ago")
}

// TestNoTestOfSlowerNodes: a media node known to be fast out-draws a node
// known to be slow, so the session tests nothing.
func TestNoTestOfSlowerNodes(t *testing.T) {
	c, n := testController(t, nil)
	s, _ := c.Play("tv/1", 40)
	media, other := s.Node(), n["a"]
	if media == other {
		other = n["b"]
	}
	record(c.stats, media, 0, cycle(40, 100)...)
	record(c.stats, other, 0, cycle(40, 3)...)
	for range 100 {
		if got := s.Explore(); got != nil {
			t.Fatalf("tested %v, known to be slower than the media node", got)
		}
	}
}
