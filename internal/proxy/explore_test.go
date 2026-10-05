package proxy

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// TestExploreComesBack: an explored stretch reads the next bytes through
// another node, and whatever ends it, the stream resumes on the media node at
// the next byte without counting a failover.
func TestExploreComesBack(t *testing.T) {
	file := make([]byte, 1<<20)
	rand.NewChaCha8([32]byte{}).Read(file)
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
	s.ra.mu.Lock()
	sp := s.ra.file("tv/1").open(&reader{pos: 1000})
	s.ra.mu.Unlock()
	sp.ready, sp.f.window, sp.f.bitrate = true, 1<<20, 20
	ctx, cancel := context.WithCancel(t.Context())
	st := &feed{s: s, sp: sp, play: play, req: req, node: media,
		ctx: ctx, cancel: cancel, off: 1000, end: size - 1, total: size, validator: `"v1"`}
	sp.feed = st

	resumeAt := func(cause error, want *nodes.Node, exploring bool) {
		t.Helper()
		resp, err := st.resume(t.Context(), cause)
		if err != nil {
			t.Fatalf("resume after %v: %v", cause, err)
		}
		resp.Body.Close()
		if st.node != want || st.exploring.Load() != exploring {
			t.Fatalf("after %v: on %v (exploring %v), want %v (exploring %v)", cause, st.node, st.exploring.Load(), want, exploring)
		}
		if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", st.off, size-1, size); got != want {
			t.Fatalf("after %v: Content-Range %q, want %q", cause, got, want)
		}
		st.off += 4096 // as if the stretch was read
	}
	for _, end := range []error{errExplored, errStall} {
		resumeAt(exploreOn{other}, other, true)
		resumeAt(end, media, false)
	}

	if f := s.ctrl.Sessions()[0].Failovers; f != 0 {
		t.Errorf("exploring counted %d failovers", f)
	}
	recent := s.stats.State(other).Recent
	if last := recent[len(recent)-1]; last.Kind != measure.KindExplore || last.Err == "" {
		t.Errorf("explored node's last sample %+v, want the stall as an explore fault", last)
	}
	for _, smp := range s.stats.State(media).Recent {
		if smp.Err != "" {
			t.Errorf("the explored node's stall counted against the media node: %+v", smp)
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
	resumeOn(video, switchTo{other}, other)
	resumeOn(audio, switchTo{other}, other)
	resumeOn(side, errStall, other) // the session already moved: follow it
	if f := s.ctrl.Sessions()[0].Failovers; f != 1 {
		t.Errorf("one move of three feeds counted %d failovers, want 1", f)
	}
}
