package control

import (
	"context"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// testController has three direct nodes, a, b and c, with the given rate
// samples from 10 minutes ago.
func testController(t *testing.T, rates map[string][]float64) (*Controller, map[string]*nodes.Node) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
upstream: {url: "http://127.0.0.1:1"}
proxies:
  - {name: a, type: direct, udp: false}
  - {name: b, type: direct, udp: true}
  - {name: c, type: direct, udp: true, tfo: true}
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
	stats := measure.NewStats()
	byName := map[string]*nodes.Node{}
	for _, n := range pool.All() {
		byName[n.Name] = n
		record(stats, n, 10*time.Minute, rates[n.Name]...)
	}
	if len(byName) != 3 {
		t.Fatalf("nodes %v, want a, b and c", byName)
	}
	return New(store, pool, stats), byName
}

// record records rate samples on n, one per window, the last taken age ago.
func record(stats *measure.Stats, n *nodes.Node, age time.Duration, mbps ...float64) {
	last := time.Now().Add(-age)
	for i, r := range mbps {
		at := last.Add(-time.Duration(len(mbps)-1-i) * measure.Window)
		stats.Record(n, measure.Sample{Kind: measure.KindPassive, Bytes: int64(r * 1e6 / 8 * 2), Dur: measure.Window, Time: at})
	}
}

// step runs s's step as if stepGap passed since the last, so a deficit
// accrues as it does between real steps.
func step(s *Stream, o Observation) (*nodes.Node, string) {
	if !s.stepped.IsZero() {
		s.stepped = s.stepped.Add(-stepGap)
	}
	return s.Step(o)
}

// behind is an observation of a read-ahead of secs seconds on a node
// delivering almost nothing: each step adds most of stepGap to the deficit.
func behind(secs float64) Observation {
	return Observation{ReadAhead: time.Duration(secs * float64(time.Second)), Fetched: 1}
}

func TestStartsOnTheFastestNode(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {20}, "b": {80}})
	if s, _ := c.Play("tv/1", 10); s.Node() != n["b"] {
		t.Errorf("media node %v, want b, the fastest", s.Node())
	}
}

// TestStartPrefersAFastEnoughPrimary: a session starts on the primary when
// its rate has headroom over the bitrate, so it shares the browsing exit IP.
func TestStartPrefersAFastEnoughPrimary(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {30}})
	c.primary = n["b"]
	if s, _ := c.Play("tv/1", 20); s.Node() != n["b"] {
		t.Errorf("20 Mbps: media node %v, want b, the primary, at 30 Mbps", s.Node())
	}
	if s, _ := c.Play("tv/2", 30); s.Node() != n["a"] {
		t.Errorf("30 Mbps: media node %v, want a: the primary has no headroom", s.Node())
	}
}

// TestStepMovesWhenBehind: a node delivering under the bitrate with the
// read-ahead under riskAhead for riskSteps steps moves the session to a
// faster node, and only to a faster one.
func TestStepMovesWhenBehind(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {50}})
	s, _ := c.Play("tv/1", 22)
	if s.Node() != n["a"] {
		t.Fatalf("media node %v, want a", s.Node())
	}
	for _, secs := range []float64{15, 14, 13} {
		if got, why := step(s, behind(secs)); got != nil {
			t.Fatalf("a behind, the fastest node: Step = %v (%s), want stay", got, why)
		}
	}
	record(c.stats, n["a"], 0, 5, 5, 5) // a sags under b
	if got, why := step(s, behind(12)); got != n["b"] || why != "risk" {
		t.Errorf("a sagging: Step = %v (%s), want a move to b for risk", got, why)
	}
}

// TestStepStaysWhileTheNodeKeepsUp: a thin read-ahead is no reason to move
// while upstream delivers the bitrate, as a player filling its own buffer
// after a start or a seek leaves it; nor is one that is full or long.
func TestStepStaysWhileTheNodeKeepsUp(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {50}})
	s, _ := c.Play("tv/1", 22)
	record(c.stats, n["a"], 0, 1, 1, 1) // a's average sagged under b, but it delivers
	for _, o := range []Observation{
		{ReadAhead: time.Second, Fetched: 22},
		{ReadAhead: 0, Fetched: 22},
		{ReadAhead: 14 * time.Second, Full: true, Fetched: 1},
		{ReadAhead: 30 * time.Second, Fetched: 1},
	} {
		for range 3 {
			if got, _ := step(s, o); got != nil || s.state != "ok" {
				t.Errorf("%+v: Step = %v, state %q; want stay, ok", o, got, s.state)
			}
		}
	}
}

// TestStepRidesOutNoise: a node that delivers the bitrate on average, in
// steps that swing far under and over it, never falls behind.
func TestStepRidesOutNoise(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {50}})
	s, _ := c.Play("tv/1", 22)
	record(c.stats, n["a"], 0, 1, 1, 1) // b is faster on paper: only the rule holds the session
	for i := range 30 {
		mbps := 34.0
		if i%2 == 0 {
			mbps = 10
		}
		if got, _ := step(s, Observation{Fetched: mbps}); got != nil || s.state != "ok" {
			t.Fatalf("step %d at %v Mbps: Step = %v, state %q, deficit %v; want stay, ok", i, mbps, got, s.state, s.deficit)
		}
	}
	// Under the bitrate for good, it falls behind and moves.
	for range 4 {
		if got, _ := step(s, Observation{Fetched: 10}); got != nil {
			if got != n["b"] {
				t.Errorf("behind: moved to %v, want b", got)
			}
			return
		}
	}
	t.Errorf("a node at 10 Mbps for 8 s on a 22 Mbps file: still there, deficit %v", s.deficit)
}

func TestOnlyTheLeadStreamDecides(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {50}})
	play, _ := c.Play("tv/1", 22)
	if play.Node() != n["a"] {
		t.Fatalf("media node %v, want a", play.Node())
	}
	scan, _ := c.Play("tv/1", 22)
	record(c.stats, n["a"], 0, 1, 1, 1)
	step(play, Observation{ReadAhead: 30 * time.Second, Played: 1e6, Fetched: 22})
	// The scan reads a little, slowly: alone it would look behind.
	for range 3 {
		if got, _ := scan.Step(Observation{ReadAhead: time.Second, Played: 1e3, Fetched: 1}); got != nil {
			t.Errorf("a side connection switched the session to %v", got)
		}
	}
	// The same on the lead stream does move the session.
	step(play, Observation{ReadAhead: time.Second, Played: 1e6, Fetched: 1})
	if got, _ := step(play, Observation{ReadAhead: time.Second, Played: 1e6, Fetched: 1}); got != n["b"] {
		t.Errorf("lead stream behind: Step = %v, want a switch to b", got)
	}
}

// TestStepDoesNotReturnToANodeItJustLeft replays 14:32 of the 2026-10-05
// session 5e82c7d6: a 40 Mbps video flapping between two nodes every 22 s.
// The session left b when it sagged to ~25 Mbps and now reads a, a little
// under the bitrate. b's average follows its sag, so it is no way out.
func TestStepDoesNotReturnToANodeItJustLeft(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {40, 44, 37, 42, 39, 41, 36, 45}, "b": {33, 35, 37, 33, 35, 37}})
	a, b := n["a"], n["b"]
	record(c.stats, b, 25*time.Second, 24, 27, 22, 26, 25, 23, 27, 24, 26, 22)
	record(c.stats, a, 0, 37, 35, 39, 36, 38, 34, 37, 36, 38, 35, 37)
	s, _ := c.Play("tv/1512410", 40)
	if s.Node() != a {
		t.Fatalf("media node %v, want a, the faster of late", s.Node())
	}
	for _, secs := range []float64{19, 18, 17} {
		if got, why := step(s, behind(secs)); got != nil {
			t.Errorf("Step = %v (%s), want stay: b sagged half a minute ago", got, why)
		}
	}
}

func TestStepWaitsAfterASwitch(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {50}})
	s, _ := c.Play("tv/1", 22)
	s.Switched(n["c"], "stall")
	for _, secs := range []float64{3, 2, 1} {
		if got, _ := step(s, behind(secs)); got != nil {
			t.Errorf("moments after a switch: Step = %v, want stay", got)
		}
	}
	s.switched = s.switched.Add(-minSwitchGap)
	if got, _ := step(s, behind(1)); got != nil {
		t.Errorf("one step after the switch settled: Step = %v, want stay: the deficit starts afresh", got)
	}
	if got, _ := step(s, behind(1)); got != n["a"] {
		t.Errorf("once the switch settled: Step = %v, want a, the fastest", got)
	}
}

// TestFailoverTakesTheFastestOther: a hard failure goes to the fastest node
// but the one that failed and the session's own.
func TestFailoverTakesTheFastestOther(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {100}, "b": {50}, "c": {20}})
	s, _ := c.Play("tv/1", 10)
	if got := s.Failover(n["a"]); got != n["b"] {
		t.Errorf("a failed: Failover = %v, want b", got)
	}
	s.Switched(n["b"], "stall")
	if got := s.Failover(n["b"]); got != n["a"] {
		t.Errorf("b failed: Failover = %v, want a", got)
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
		{"faster node wins", map[string][]float64{"a": {12}, "b": {75}}, 230 * time.Millisecond, 273 * time.Millisecond, "b"},
		{"same speed, lower RTT wins", map[string][]float64{"a": {50}, "b": {50}}, 120 * time.Millisecond, 50 * time.Millisecond, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n := testController(t, tc.rate)
			for name, rtt := range map[string]time.Duration{"a": tc.rttA, "b": tc.rttB} {
				c.stats.Record(n[name], measure.Sample{Kind: measure.KindPing, TTFB: rtt})
			}
			if got, _ := c.Primary(); got != n[tc.want] {
				t.Errorf("primary %v, want %s (a %.0f ms, b %.0f ms)", got, tc.want, c.burst(n["a"]), c.burst(n["b"]))
			}
		})
	}
}

// TestPrimaryMovesOnlyWhenQuickerAndIdle: a node clearly quicker takes over
// after a ping round, but not while something plays.
func TestPrimaryMovesOnlyWhenQuickerAndIdle(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {40}})
	c.stats.Record(n["a"], measure.Sample{Kind: measure.KindPing, TTFB: 80 * time.Millisecond})
	if got, _ := c.Primary(); got != n["a"] {
		t.Fatalf("primary %v, want a, the only one pinged", got)
	}
	record(c.stats, n["b"], 0, 200)
	c.stats.Record(n["b"], measure.Sample{Kind: measure.KindPing, TTFB: 30 * time.Millisecond})
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
	c, _ := testController(t, map[string][]float64{"a": {500}, "b": {110}})
	s, _ := c.Play("tv/1", 22.2)
	if st := c.Session("tv/1").Session.State; st != "starting" {
		t.Errorf("before a step: state %q, want starting", st)
	}
	step(s, Observation{ReadAhead: 30 * time.Second, Full: true, Fetched: 40})
	d := c.Session("tv/1")
	if d.Session.State != "ok" || d.Session.FetchedMbps != 40 || d.Session.NodeMbps != 500 ||
		len(d.History) != 1 || d.History[0].Ahead != 30 {
		t.Fatalf("full read-ahead: %+v, history %+v", d.Session, d.History)
	}

	step(s, behind(15))
	if st := c.Session("tv/1").Session.State; st != "ok" {
		t.Errorf("one step short: state %q, want ok", st)
	}
	step(s, behind(15))
	if st := c.Session("tv/1").Session.State; st != "risk" {
		t.Errorf("behind: state %q, want risk", st)
	}
	step(s, behind(1))
	if d := c.Session("tv/1"); d.Session.State != "low" || d.Session.LowSeconds < stepGap.Seconds() || len(d.History) != 4 {
		t.Errorf("behind under the low mark: %+v, want low", d.Session)
	}
	step(s, Observation{ReadAhead: 30 * time.Second, Fetched: 40})
	if d := c.Session("tv/1"); d.Session.State != "ok" || d.Session.LowSeconds >= 2*stepGap.Seconds() {
		t.Errorf("a long read-ahead again: %+v, want ok, low unchanged", d.Session)
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
	if d := c.Session("tv/1"); d.Session == nil || d.Session.State != "ended" || len(d.History) != 5 {
		t.Errorf("an ended session: %+v", d)
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

// TestExploreWithinBudget: one test runs at a time, and the next waits
// until the session has played the test's bytes over its budget.
func TestExploreWithinBudget(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {50}})
	s, _ := c.Play("tv/1", 40)
	if s.Node() != n["a"] {
		t.Fatalf("media node %v, want a", s.Node())
	}
	first := s.Explore()
	if first == nil || first == n["a"] {
		t.Fatalf("Explore = %v, want b or c, never measured", first)
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
	if got := s.Explore(); got == nil {
		t.Fatal("Explore = nil once the session earned a test")
	}
}

// TestExploreTakesTurns: a node never measured is tested first, then the
// one measured longest ago; a node measured within retestAfter waits.
func TestExploreTakesTurns(t *testing.T) {
	c, n := testController(t, map[string][]float64{"a": {50}})
	s, _ := c.Play("tv/1", 10)
	explore := func() *nodes.Node {
		t.Helper()
		got := s.Explore()
		if got != nil {
			s.Probed(0)
			s.nextTest = time.Time{}
		}
		return got
	}
	record(c.stats, n["b"], 2*time.Hour, 10)
	if got := explore(); got != n["c"] {
		t.Fatalf("Explore = %v, want c, never measured", got)
	}
	record(c.stats, n["c"], time.Hour, 900)
	if got := explore(); got != n["b"] {
		t.Fatalf("Explore = %v, want b, measured longest ago, slow as it was", got)
	}
	record(c.stats, n["b"], 10*time.Minute, 10)
	if got := explore(); got != n["c"] {
		t.Fatalf("Explore = %v, want c, measured an hour ago", got)
	}
	record(c.stats, n["c"], 0, 900)
	if got := explore(); got != nil {
		t.Errorf("Explore = %v, want none: every node measured lately", got)
	}
}

// TestMigratesToAFarFasterNode: a session that keeps up moves to a node
// measured lately at migrateGain times its own node's rate, once it has
// been on its node for migrateGap.
func TestMigratesToAFarFasterNode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mbps  float64
		age   time.Duration
		moves bool
	}{
		{"far faster", 120, 10 * time.Minute, true},
		{"a little faster", 36, 10 * time.Minute, false},
		{"far faster, long ago", 120, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, n := testController(t, map[string][]float64{"a": {30}})
			s, _ := c.Play("tv/1", 10)
			if s.Node() != n["a"] {
				t.Fatalf("media node %v, want a", s.Node())
			}
			record(c.stats, n["b"], tc.age, tc.mbps)
			full := Observation{ReadAhead: time.Minute, Full: true}
			if got, _ := step(s, full); got != nil {
				t.Fatalf("moved to %v moments after play started", got)
			}
			s.started = s.started.Add(-migrateGap)
			got, why := step(s, full)
			switch {
			case tc.moves && (got != n["b"] || why != "faster"):
				t.Errorf("Step = %v (%s), want a move to b for speed", got, why)
			case !tc.moves && got != nil:
				t.Errorf("Step = %v (%s), want stay", got, why)
			}
		})
	}
}
