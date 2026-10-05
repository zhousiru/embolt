package proxy

import (
	"bytes"
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
	st := &stream{s: s, play: play, req: req, bitrate: 20, node: media,
		off: 1000, end: size - 1, total: size, validator: `"v1"`, ring: make(chan []byte, 16)}

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
