package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// exploreSetup serves file, has a player read its first mb, and waits until
// the main read's window is full. It returns the server, the upstream's log
// of Range headers, the player's answer, the span, and a node other than the
// media node. failTests makes upstream drop every bounded range: a test.
func exploreSetup(t *testing.T, file []byte, failTests bool) (*Server, func() []string, *http.Response, *span, *nodes.Node) {
	t.Helper()
	var mu sync.Mutex
	var ranges []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rng)
		mu.Unlock()
		if failTests && !strings.HasSuffix(rng, "-") {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
	}))
	t.Cleanup(upstream.Close)
	// A 1 s read-ahead at the first-run 40 Mbps is a 5 MB window.
	s, pool := newTestServer(t, upstream.URL, "control: {read_ahead: 1s}")
	px := httptest.NewServer(s)
	t.Cleanup(px.Close)

	resp := openMedia(t, px.URL, "bytes=0-", "tv")
	t.Cleanup(func() { resp.Body.Close() })
	if _, err := io.ReadFull(resp.Body, make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	var sp *span
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.ra.mu.Lock()
		for _, f := range s.ra.files {
			sp = f.spans[0]
		}
		full := sp != nil && sp.ready && !sp.room()
		s.ra.mu.Unlock()
		if full {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the read-ahead never filled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var other *nodes.Node
	for _, n := range pool.All() {
		if n != sp.f.play.Node() {
			other = n
		}
	}
	return s, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(ranges) }, resp, sp, other
}

// runTest runs a test of sp's file through n to its end, and returns the
// range it asked for.
func runTest(t *testing.T, s *Server, sp *span, n *nodes.Node) string {
	t.Helper()
	s.ra.mu.Lock()
	tt := sp.f.test(sp)
	play := sp.f.play
	s.ra.mu.Unlock()
	tt.run(t.Context(), play, n)
	return fmt.Sprintf("bytes=%d-%d", tt.from, tt.end)
}

// TestExploreBesideTheMediaNode: a test reads a stretch through another node
// and drops it, while the media node reads on through one connection. The
// player gets every byte, and no failover is counted.
func TestExploreBesideTheMediaNode(t *testing.T) {
	file := randomFile(24 << 20)
	s, ranges, resp, sp, other := exploreSetup(t, file, false)
	tested := runTest(t, s, sp, other)

	rest, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(rest, file[1<<20:]) {
		t.Fatalf("the player got %d bytes after the first MB (%v), want the rest of the file", len(rest), err)
	}
	if got, want := ranges(), []string{"bytes=0-", tested}; !slices.Equal(got, want) {
		t.Errorf("upstream saw %q, want %q: the media node's read and the test", got, want)
	}
	if f := s.ctrl.Sessions()[0].Failovers; f != 0 {
		t.Errorf("a test counted %d failovers", f)
	}
	recent := s.stats.Recent(other)
	if len(recent) == 0 || recent[len(recent)-1].Kind != measure.KindExplore || recent[len(recent)-1].Err != "" {
		t.Errorf("the tested node's samples %+v, want an explore rate", recent)
	}
}

// TestFailedExploreCostsNothing: a test whose node fails leaves the media
// node reading on; the failure counts against the tested node only.
func TestFailedExploreCostsNothing(t *testing.T) {
	file := randomFile(24 << 20)
	s, ranges, resp, sp, other := exploreSetup(t, file, true)
	tested := runTest(t, s, sp, other)

	rest, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(rest, file[1<<20:]) {
		t.Fatalf("the player got %d bytes after the first MB (%v), want the rest of the file", len(rest), err)
	}
	if got := ranges(); len(got) < 2 || got[0] != "bytes=0-" || slices.ContainsFunc(got[1:], func(r string) bool { return r != tested }) {
		t.Errorf("upstream saw %q, want the media node's read, then only the test %q", got, tested)
	}
	recent := s.stats.Recent(other)
	if len(recent) == 0 || recent[len(recent)-1].Err == "" {
		t.Errorf("the tested node's samples %+v, want its failure", recent)
	}
	for _, smp := range s.stats.Recent(sp.f.play.Node()) {
		if smp.Err != "" {
			t.Errorf("the test's failure counted against the media node: %+v", smp)
		}
	}
}

// TestFeedsMoveTogether: a switch moves each of a file's feeds and counts
// one failover for the session; a feed that then stalls on the old node
// follows the session rather than picking a node of its own.
func TestFeedsMoveTogether(t *testing.T) {
	file := make([]byte, 1<<20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
	}))
	defer upstream.Close()

	s, pool := newTestServer(t, upstream.URL)
	play, err := s.ctrl.Play("tv/1", 20)
	if err != nil {
		t.Fatal(err)
	}
	defer play.Release()
	media := play.Node()
	var other *nodes.Node
	for _, n := range pool.All() {
		if n != media {
			other = n
		}
	}
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/Videos/42/stream", nil)
	size := int64(len(file))
	newFeed := func(off int64) *feed {
		s.ra.mu.Lock()
		sp := s.ra.file("tv/1").open(&reader{pos: off})
		s.ra.mu.Unlock()
		sp.ready, sp.f.window, sp.f.bitrate = true, 1<<20, 20
		ctx, cancel := context.WithCancel(t.Context())
		fd := &feed{s: s, sp: sp, play: play, req: req, node: media,
			ctx: ctx, cancel: cancel, off: off, end: size - 1, total: size, validator: `"v1"`}
		sp.feed = fd
		return fd
	}
	resumeOn := func(fd *feed, cause error, want *nodes.Node) {
		t.Helper()
		resp, err := fd.resume(t.Context(), cause)
		if err != nil {
			t.Fatalf("resume after %v: %v", cause, err)
		}
		resp.Body.Close()
		if fd.node != want {
			t.Fatalf("after %v: on %v, want %v", cause, fd.node, want)
		}
	}

	video, audio, side := newFeed(1000), newFeed(400_000), newFeed(800_000)
	resumeOn(video, switchTo{other, "risk"}, other)
	resumeOn(audio, switchTo{other, "risk"}, other)
	resumeOn(side, errStall, other) // the session already moved: follow it
	if f := s.ctrl.Sessions()[0].Failovers; f != 1 {
		t.Errorf("one move of three feeds counted %d failovers, want 1", f)
	}
}

// TestServerErrorKeepsTheNode: a feed resuming after a stall onto a node
// whose answer is a server error ends there, rather than trying more nodes
// that would get the same answer.
func TestServerErrorKeepsTheNode(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	s, _ := newTestServer(t, upstream.URL)
	play, err := s.ctrl.Play("tv/1", 20)
	if err != nil {
		t.Fatal(err)
	}
	defer play.Release()
	req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/Videos/42/stream", nil)
	s.ra.mu.Lock()
	sp := s.ra.file("tv/1").open(&reader{pos: 0})
	s.ra.mu.Unlock()
	fd := &feed{s: s, sp: sp, play: play, req: req, node: play.Node(), off: 1000, end: 1 << 20, total: 1<<20 + 1}
	if _, err := fd.resume(t.Context(), errStall); err == nil || errors.Is(err, control.ErrNoNode) {
		t.Fatalf("resume = %v, want the server's error", err)
	}
	if asked != 2 {
		t.Errorf("upstream asked %d times, want 2: one node, retried once", asked)
	}
	if f := s.ctrl.Sessions()[0].Failovers; f != 0 {
		t.Errorf("a server error counted %d failovers", f)
	}
}
