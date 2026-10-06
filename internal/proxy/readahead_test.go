package proxy

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mediaUpstream serves file at /Videos/1/original.mp4 and counts the reads.
func mediaUpstream(t *testing.T, file []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/Videos/1/") {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
	}))
	t.Cleanup(upstream.Close)
	return upstream, &reads
}

func randomFile(size int) []byte {
	file := make([]byte, size)
	rand.NewChaCha8([32]byte{1}).Read(file)
	return file
}

// openMedia sends a player's ranged read; the caller reads and closes the body.
func openMedia(t *testing.T, url, rng, device string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/Videos/1/original.mp4?Static=true", nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("X-Emby-Device-Id", device)
	resp, err := http.DefaultTransport.RoundTrip(req) // a fresh connection, as a player opens one per jump
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// jump reads n bytes from first, as one player jump, and checks them.
func jump(t *testing.T, url, device string, file []byte, first, n int) {
	t.Helper()
	resp := openMedia(t, url, fmt.Sprintf("bytes=%d-", first), device)
	defer resp.Body.Close()
	want := fmt.Sprintf("bytes %d-%d/%d", first, len(file)-1, len(file))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != want {
		t.Fatalf("read at %d: %d %q, want 206 %q", first, resp.StatusCode, resp.Header.Get("Content-Range"), want)
	}
	got := make([]byte, n)
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("read at %d: %v", first, err)
	}
	if !bytes.Equal(got, file[first:first+n]) {
		t.Fatalf("read at %d: wrong bytes", first)
	}
}

// heldSpans reports the spans of every file: where each holds bytes from
// and to.
func heldSpans(s *Server) [][2]int64 {
	s.ra.mu.Lock()
	defer s.ra.mu.Unlock()
	var out [][2]int64
	for _, f := range s.ra.files {
		for _, sp := range f.spans {
			out = append(out, [2]int64{sp.from, sp.to})
		}
	}
	return out
}

// TestInterleavedJumpsShareOneFetch replays a player reading a badly
// interleaved file through one connection at a time: it jumps between two
// regions 18 MB apart, dropping the connection at every jump. Every jump is
// answered from the first fetch's read-ahead: one upstream read in all.
func TestInterleavedJumpsShareOneFetch(t *testing.T) {
	file := randomFile(40 << 20)
	upstream, reads := mediaUpstream(t, file)
	px := newTestProxy(t, upstream.URL)
	s := px.Config.Handler.(*Server)

	const audio, video = 2 << 20, 20 << 20
	first := openMedia(t, px.URL, fmt.Sprintf("bytes=%d-", audio), "tv")
	deadline := time.Now().Add(5 * time.Second)
	for spans := heldSpans(s); len(spans) == 0 || spans[0][1] < video+(8<<20); spans = heldSpans(s) {
		if time.Now().After(deadline) {
			t.Fatalf("the read-ahead never reached past the video region: %v", spans)
		}
		time.Sleep(10 * time.Millisecond)
	}
	first.Body.Close()

	for i := range 20 {
		jump(t, px.URL, "tv", file, audio+i*50_000, 30_000)
		jump(t, px.URL, "tv", file, video+i*300_000, 200_000)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("upstream saw %d reads, want 1: every jump should come from the read-ahead", n)
	}
	if spans := heldSpans(s); len(spans) != 1 || spans[0][0] > audio {
		t.Errorf("spans %v: the audio region should still be held, where the player left it", spans)
	}
}

// TestDevicesDoNotShare: a read-ahead belongs to one session.
func TestDevicesDoNotShare(t *testing.T) {
	file := randomFile(1 << 20)
	upstream, reads := mediaUpstream(t, file)
	px := newTestProxy(t, upstream.URL)
	jump(t, px.URL, "tv", file, 0, 1000)
	jump(t, px.URL, "phone", file, 0, 1000)
	if n := reads.Load(); n != 2 {
		t.Errorf("upstream saw %d reads for two devices, want 2", n)
	}
}

// TestWholeFileAndErrors: a read without Range gets the whole file as a 200;
// upstream's errors reach the player as they are; a range past the end is
// answered here once the file's size is known.
func TestWholeFileAndErrors(t *testing.T) {
	file := randomFile(300_000)
	upstream, _ := mediaUpstream(t, file)
	px := newTestProxy(t, upstream.URL)

	resp := openMedia(t, px.URL, "", "tv")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, file) || resp.ContentLength != int64(len(file)) ||
		resp.Header.Get("ETag") != `"v1"` || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Errorf("whole file: %d, %d bytes, length %d, %v", resp.StatusCode, len(body), resp.ContentLength, resp.Header)
	}

	resp = openMedia(t, px.URL, "bytes=1000-1999", "tv")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, file[1000:2000]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1000-1999/%d", len(file)) {
		t.Errorf("bounded range: %d %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
	}

	resp = openMedia(t, px.URL, fmt.Sprintf("bytes=%d-", len(file)), "tv")
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("past the end: %d, want 416", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, px.URL+"/Videos/2/original.mp4?Static=true", nil)
	req.Header.Set("Range", "bytes=0-")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("a missing file: %d, want upstream's 404", r.StatusCode)
	}
}

// TestIfRange: a player resuming a read sends If-Range. When it names the
// version the read-ahead holds, the span answers, and a second read shares
// the first's fetch; when it names another version, the request goes to
// upstream, which answers with the whole file.
func TestIfRange(t *testing.T) {
	file := randomFile(1 << 20)
	upstream, reads := mediaUpstream(t, file)
	px := newTestProxy(t, upstream.URL)
	resume := func(first int, ifRange string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, px.URL+"/Videos/1/original.mp4?Static=true", nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", first))
		req.Header.Set("If-Range", ifRange)
		req.Header.Set("X-Emby-Device-Id", "tv")
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	for _, first := range []int{1000, 2000} { // the first opens the span, the second joins it
		resp, body := resume(first, `"v1"`)
		if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, file[first:]) {
			t.Errorf("If-Range of the held version at %d: %d, %d bytes; want 206 from there", first, resp.StatusCode, len(body))
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("upstream saw %d reads, want 1: the second resume should come from the read-ahead", n)
	}

	resp, body := resume(3000, `"v0"`)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, file) {
		t.Errorf("If-Range of another version: %d, %d bytes; want 200 with the whole file", resp.StatusCode, len(body))
	}
	if n := reads.Load(); n != 2 {
		t.Errorf("upstream saw %d reads, want 2: another version goes to upstream", n)
	}
}

func TestParseRange(t *testing.T) {
	for in, want := range map[string]struct {
		first, last int64
		ok          bool
	}{
		"":              {0, -1, true},
		"bytes=0-":      {0, -1, true},
		"bytes=69-":     {69, -1, true},
		"bytes=0-1":     {0, 1, true},
		"bytes=-500":    {0, 0, false},
		"bytes=0-1,5-9": {0, 0, false},
		"bytes=9-1":     {0, 0, false},
		"items=0-":      {0, 0, false},
	} {
		first, last, ok := parseRange(in)
		if ok != want.ok || ok && (first != want.first || last != want.last) {
			t.Errorf("parseRange(%q) = %d, %d, %v; want %+v", in, first, last, ok, want)
		}
	}
}

// testSpan is a span that answered, holding [from, to) with reach handed to
// players, which delivered recent bytes lately, as of at.
func testSpan(f *file, from, to, reach int64, recent float64, at time.Time, connected bool) *span {
	sp := &span{f: f, ready: true, from: from, to: to, end: -1, reach: reach,
		recent: recent, recentAt: at, opened: at, readers: map[*reader]struct{}{}, wake: make(chan struct{})}
	if connected {
		sp.readers[&reader{pos: reach}] = struct{}{}
	}
	f.spans = append(f.spans, sp)
	return sp
}

// TestReadAheadIsTheShortestRegion: the read-ahead is counted in seconds of
// playback over the regions being played and still fetching, each at its
// share of the bitrate, and is the shortest of them.
func TestReadAheadIsTheShortestRegion(t *testing.T) {
	now := time.Now()
	const mb = 1 << 20
	secs := func(d time.Duration) float64 { return float64(d.Round(10*time.Millisecond)) / float64(time.Second) }
	at := func(bytes, share float64) float64 {
		return secs(time.Duration(bytes * 8 / (share * 8e6) * float64(time.Second)))
	}

	one := &file{bitrate: 8} // 1 MB/s
	only := testSpan(one, 0, 20*mb, 10*mb, 1, now, true)
	if obs, main := one.observe(now); main != only || secs(obs.ReadAhead) != at(10*mb, 1) {
		t.Errorf("one span: %v, main %v; want 10 MB at the full bitrate", obs.ReadAhead, main == only)
	}

	// A badly interleaved file: video plays 95% of the bytes, audio 5%. The
	// audio's 1 MB lasts 1/0.05 times longer than at the full bitrate, but
	// still less than the video's 50 MB.
	mixed := &file{bitrate: 8}
	video := testSpan(mixed, 0, 60*mb, 10*mb, 95, now, true)
	testSpan(mixed, 100*mb, 102*mb, 101*mb, 5, now, false) // read within revisit
	if obs, main := mixed.observe(now); main != video || secs(obs.ReadAhead) != at(mb, 0.05) {
		t.Errorf("interleaved: %v, main is video %v; want the audio's %.2fs", obs.ReadAhead, main == video, at(mb, 0.05))
	}

	// A region read to the end of the file, such as a tail index, holds
	// nothing ahead but cannot run dry; a connected region that has gone
	// quiet holds a vanishing share, which lasts too long to matter. Neither
	// is the shortest.
	tail := &file{bitrate: 8}
	testSpan(tail, 0, 60*mb, 10*mb, 50, now, true)
	index := testSpan(tail, 90*mb, 100*mb, 100*mb, 50, now, true)
	index.end = 100*mb - 1
	testSpan(tail, 70*mb, 71*mb, 70*mb, 1e-12, now, true)
	if obs, _ := tail.observe(now); secs(obs.ReadAhead) != at(50*mb, 0.5) {
		t.Errorf("with a fetched and a quiet region: %v, want the video's %.2fs", obs.ReadAhead, at(50*mb, 0.5))
	}

	// After a seek, the span left behind still holds a big history, but no
	// player has read it within revisit: the new span alone counts, and the
	// bound on the player's buffer starts afresh from it.
	seek := &file{bitrate: 8}
	old := testSpan(seek, 0, 300*mb, 290*mb, 1e9, now.Add(-time.Hour), false)
	old.recentAt, old.sent = now.Add(-2*revisit), 290*mb
	jumped := testSpan(seek, 500*mb, 502*mb, 500*mb, 1, now, true)
	obs, main := seek.observe(now)
	if main != jumped || secs(obs.ReadAhead) != at(2*mb, 1) {
		t.Errorf("after a seek: %v, main is the new span %v; want the new span's 2 MB", obs.ReadAhead, main == jumped)
	}
	if obs.Delivered != 0 || obs.Elapsed != 0 {
		t.Errorf("after a seek: delivered %v over %v, want none: the player's buffer went with the seek", obs.Delivered, obs.Elapsed)
	}
}
