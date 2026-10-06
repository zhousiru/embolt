package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// TestBest: every choice meets the target first, by the caller's preference,
// and falls back on the one least short when nothing does.
func TestBest(t *testing.T) {
	a, b := &nodes.Node{Name: "a"}, &nodes.Node{Name: "b"}
	byName := func(x, y option) int { return strings.Compare(x.n.Name, y.n.Name) }
	if o, ok := best([]option{{n: b, safe: 30, need: 20}, {n: a, safe: 25, need: 20}}, byName); o.n != a || !ok {
		t.Errorf("both pass: picked %v (meets %v), want a by preference", o.n, ok)
	}
	if o, ok := best([]option{{n: a, safe: 10, need: 20}, {n: b, safe: 21, need: 20}}, byName); o.n != b || !ok {
		t.Errorf("only b passes: picked %v (meets %v), want b", o.n, ok)
	}
	if o, ok := best([]option{{n: a, safe: 10, need: 20}, {n: b, safe: 15, need: 21}}, byName); o.n != b || ok {
		t.Errorf("none passes: picked %v (meets %v), want b, the least short", o.n, ok)
	}
	if o, ok := best([]option{{n: a, safe: 0, need: -5}}, byName); ok {
		t.Errorf("a node never measured: picked %v as meeting the target", o.n)
	}
	if o, _ := best(nil, byName); o.n != nil {
		t.Errorf("no options: picked %v", o.n)
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
	stats := measure.NewStats(store)
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
	if got, why := s.Step(obs); got != n["b"] || why != "risk" {
		t.Errorf("Step = %v (%s), want a switch to b for risk", got, why)
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
	if got, _ := s.Step(obs); got != nil {
		t.Errorf("Step = %v, want stay (safe %.1f, need %.1f)", got, s.safe, s.need)
	}
	if got, _ := s.Step(Observation{Full: true}); got != nil {
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
	if got, _ := scan.Step(Observation{Delivered: time.Second, Elapsed: 6 * time.Second}); got != nil {
		t.Errorf("a side connection switched the session to %v", got)
	}
	// The same starvation on the lead stream does move the session.
	if got, _ := play.Step(Observation{Delivered: 30 * time.Second, Elapsed: 40 * time.Second}); got != n["b"] {
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
				t.Errorf("primary %v, want %s (a %.0f ms, b %.0f ms)", got, tc.want, c.burst(n["a"]), c.burst(n["b"]))
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
		t.Errorf("primary %v once idle, want b (a %.0f ms, b %.0f ms)", c.primary, c.burst(n["a"]), c.burst(n["b"]))
	}
}

// TestSessionShowsItsSteps: the session view shows the last step, keeps the
// steps for the chart, and outlives the session once it ends.
func TestSessionShowsItsSteps(t *testing.T) {
	const stepGap = 2 * time.Second
	c, n := testController(t, map[string][]float64{"a": cycle(20, 517, 571, 353, 692), "b": cycle(20, 110, 120, 95, 130)})
	s, _ := c.Play("tv/1", 22.2)
	if st := c.Session("tv/1").Session.State; st != "starting" {
		t.Errorf("before a step: state %q, want starting", st)
	}
	s.Step(Observation{ReadAhead: 30 * time.Second, Full: true, Fetched: 40})
	d := c.Session("tv/1")
	if d.Session.State != "ok" || d.Session.FetchedMbps != 40 || len(d.History) != 1 || d.History[0].Buffer != 30 {
		t.Fatalf("full read-ahead: %+v, history %+v", d.Session, d.History)
	}

	record(c.stats, n["a"], 0, 2.5, 5.8)
	s.Step(Observation{ReadAhead: 20 * time.Second})
	if st := c.Session("tv/1").Session.State; st != "risk" {
		t.Errorf("a starving node: state %q, want risk", st)
	}
	c.plays["tv/1"].stepped = time.Now().Add(-stepGap)
	s.Step(Observation{})
	if d := c.Session("tv/1"); d.Session.State != "low" || d.Session.LowSeconds < stepGap.Seconds() || len(d.History) != 3 {
		t.Errorf("an empty buffer: %+v, want low", d.Session)
	}

	s.Release()
	if st := c.Session("tv/1").Session.State; st != "idle" {
		t.Errorf("no stream: state %q, want idle", st)
	}
	c.plays["tv/1"].idle = time.Now().Add(-playLinger - time.Second)
	c.expire()
	if r := c.Recent(); len(r) != 1 || r[0].Key != "tv/1" || r[0].State != "ended" || r[0].Ended.IsZero() {
		t.Fatalf("recent %+v, want tv/1 ended", r)
	}
	if d := c.Session("tv/1"); d.Session == nil || d.Session.State != "ended" || len(d.History) != 3 {
		t.Errorf("an ended session: %+v", d)
	}
	if c.Session("tv/2").Session != nil {
		t.Error("an unknown session has a view")
	}
}

// TestStepDoesNotReturnToANodeItJustLeft replays 14:32 of the 2026-10-05
// session 5e82c7d6: a 40 Mbps video flapping between two nodes every 22 s.
// The session left b when it sagged to ~25 Mbps and now reads a, a little
// under the bitrate. Judged by its typical rate, b looked certain to keep up
// and the session went straight back; judged as a is, by what it did half a
// minute ago, b falls short.
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
	if st := c.stats.State(b); st.Now.Typical() >= st.Rate.Typical() {
		t.Fatalf("b's rate now %.1f Mbps, typical %.1f: want the sag to weigh", st.Now.Typical(), st.Rate.Typical())
	}
	if got, _ := s.Step(Observation{ReadAhead: buffer}); got != nil {
		t.Errorf("Step = %v, want stay: b sagged half a minute ago", got)
	}
	c.mu.Lock()
	moves := c.moves(s.Playback, time.Now())
	c.mu.Unlock()
	if len(moves) != 1 || moves[0].meets() {
		t.Errorf("b judged by its rate now: %+v, want short of the target", moves)
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

// TestRateNowSeesASag replays the first switch of the 2026-10-05 test: a
// node known to be fast, whose stream fell from 9 to 1.3 Mbps on a 5.1 Mbps
// video with a nearly empty buffer. Its recent samples must decide.
func TestRateNowSeesASag(t *testing.T) {
	fast := cycle(40, 70, 80, 90)
	c, n := testController(t, map[string][]float64{"a": fast, "b": fast})
	judge := func(name string, buffer time.Duration, mbps float64) option {
		return c.weigh([]*nodes.Node{n[name]}, buffer, mbps, time.Now(), false)[0]
	}
	// No samples lately is no news: the node is judged by its typical rate.
	if o := judge("a", 20*time.Second, 5.3); !o.meets() {
		t.Errorf("fast node without recent samples: %+v, want it to meet the target", o)
	}
	record(c.stats, n["a"], 0, 60, 70, 65)
	record(c.stats, n["b"], 0, 9.0, 6.3, 4.7, 1.3)
	if o := judge("a", 5*time.Second, 5.1); !o.meets() {
		t.Errorf("healthy node: %+v, want it to meet the target", o)
	}
	if o := judge("b", 5*time.Second, 5.1); o.meets() {
		t.Errorf("sagging node: safe %.1f Mbps for a need of %.1f, want short so the controller acts", o.safe, o.need)
	}
}

// TestExploreWithinBudget: a session tests a node that may be faster,
// whatever its buffer; one test runs at a time, and the next waits until
// the session has played the test's bytes over its budget.
func TestExploreWithinBudget(t *testing.T) {
	c, n := testController(t, nil)
	s, _ := c.Play("tv/1", 40)
	media, other := s.Node(), n["a"]
	if media == other {
		other = n["b"]
	}
	record(c.stats, media, 0, cycle(40, 3)...)
	record(c.stats, other, time.Hour, cycle(40, 100)...)
	s.Step(Observation{}) // the media node falls short
	if got := s.Explore(); got != other {
		t.Fatalf("with nothing buffered: Explore = %v, want %v", got, other)
	}
	if got := s.Explore(); got != nil {
		t.Errorf("explored %v while a test runs", got)
	}
	s.Probed(ProbeBytes)
	// 5% of 40 Mbps is 250 kB/s: a 32 MiB test buys 134 s of quiet.
	if wait := time.Until(s.nextTest); wait < 133*time.Second || wait > 135*time.Second {
		t.Errorf("next test in %v, want 134 s", wait)
	}
	if got := s.Explore(); got != nil {
		t.Fatalf("explored %v right after a test", got)
	}
	s.nextTest = time.Now()
	if got := s.Explore(); got != other {
		t.Fatalf("Explore = %v once the session earned a test, want %v", got, other)
	}
}

// TestExploreWhatMayBeFaster: a node never measured is tested, and one that
// may be faster, given its evidence, once it has not been sampled for
// retestAfter; a node shown slower, or sampled lately, is not, until its
// evidence fades.
func TestExploreWhatMayBeFaster(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		mbps  []float64
		tests bool
	}{
		{"never measured", 0, nil, true},
		{"measured once", time.Hour, []float64{60}, true},
		{"faster, sampled long ago", time.Hour, cycle(40, 100), true},
		{"faster, sampled lately", 10 * time.Minute, cycle(40, 100), false},
		{"slower", time.Hour, cycle(40, 3), false},
		{"slower, measured once a day ago", 24 * time.Hour, []float64{20}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n := testController(t, nil)
			s, _ := c.Play("tv/1", 10)
			media, other := s.Node(), n["a"]
			if media == other {
				other = n["b"]
			}
			record(c.stats, media, 0, cycle(40, 30)...)
			record(c.stats, other, tc.age, tc.mbps...)
			s.Step(Observation{ReadAhead: time.Minute, Full: true})
			if got := s.Explore(); (got == other) != tc.tests || got != nil && got != other {
				t.Errorf("Explore = %v, want a test of %v: %v", got, other, tc.tests)
			}
		})
	}
}

// TestMigratesToAFarFasterNode: a session that meets its target moves to a
// node a test found far faster, once it has been on its node for
// migrateGap; a node only a little faster never takes it.
func TestMigratesToAFarFasterNode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other float64
		moves bool
	}{
		{"far faster", 120, true},
		{"a little faster", 36, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n := testController(t, map[string][]float64{"a": cycle(40, 30, 32, 28), "b": cycle(40, 30, 32, 28)})
			s, _ := c.Play("tv/1", 10)
			media, other := s.Node(), n["a"]
			if media == other {
				other = n["b"]
			}
			record(c.stats, media, 0, cycle(10, 30, 32, 28)...)
			record(c.stats, other, 0, cycle(4, tc.other)...) // a test, just now
			full := Observation{ReadAhead: time.Minute, Full: true}
			if got, _ := s.Step(full); got != nil {
				t.Fatalf("moved to %v moments after play started", got)
			}
			s.started = s.started.Add(-migrateGap)
			got, why := s.Step(full)
			switch {
			case tc.moves && (got != other || why != "faster"):
				t.Errorf("Step = %v (%s), want a move to %v for speed", got, why, other)
			case !tc.moves && got != nil:
				t.Errorf("Step = %v (%s), want stay", got, why)
			}
		})
	}
}
