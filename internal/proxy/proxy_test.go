package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/cache"
	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
	"github.com/zhousiru/embolt/internal/profile"
)

func TestClassify(t *testing.T) {
	s := &Server{cfg: config.Static(mustParse(t, "http://emby", "")), catalog: newCatalog()}
	for path, want := range map[string]lane{
		"/Videos/1/stream?Static=true":          laneStatic,
		"/emby/videos/1/stream.mkv?static=true": laneStatic,
		"/Videos/1/original.mp4":                laneStatic,
		"/Items/1/Download?api_key=x":           laneStatic,
		"/Audio/1/stream.flac":                  laneStatic,
		"/Videos/1/stream.mp4?VideoCodec=h264":  laneTranscode,
		"/videos/1/master.m3u8":                 laneHLS,
		"/videos/1/hls1/main/0.ts":              laneHLS,
		"/Items/1/Images/Primary?tag=abc":       laneControl,
		"/Users/1/Items?ParentId=2":             laneControl,
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if got := s.classify(r); got != want {
			t.Errorf("%s: lane %d, want %d", path, got, want)
		}
	}
}

func TestStripOrigin(t *testing.T) {
	s := &Server{origins: origins(mustURL(t, "https://emby.example:443"))}
	for in, want := range map[string]string{
		"https://emby.example:443/web/index.html": "/web/index.html",
		"https://emby.example/videos/1?x=1":       "/videos/1?x=1",
		"https://cdn.example/videos/1":            "",
		"https://emby.example.evil/x":             "",
	} {
		if got, _ := s.stripOrigin(in); got != want {
			t.Errorf("stripOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFailoverMidStream stalls the first node after 1 MB and checks that the
// player still receives every byte, in order, in one response.
func TestFailoverMidStream(t *testing.T) {
	file := make([]byte, 6<<20)
	rand.NewChaCha8([32]byte{}).Read(file)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/Videos/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		if requests.Add(1) == 1 { // the first node delivers 1 MB, then stalls
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(file)-1, len(file)))
			w.Header().Set("Content-Length", fmt.Sprint(len(file)-1000))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(file[1000 : 1000+1<<20])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
	}))
	defer upstream.Close()

	px := newTestProxy(t, upstream.URL)
	req, _ := http.NewRequest(http.MethodGet, px.URL+"/Videos/42/stream?Static=true&PlaySessionId=p1", nil)
	req.Header.Set("Range", "bytes=1000-")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, file[1000:]) {
		t.Fatalf("status %d, %d bytes; want 206 and the file from byte 1000 intact", resp.StatusCode, len(got))
	}
	if d := time.Since(start); d > stallAfter+5*time.Second {
		t.Errorf("failover took %v", d)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("upstream saw %d requests, want 2", n)
	}
}

func newTestProxy(t *testing.T, upstream string, extra ...string) *httptest.Server {
	t.Helper()
	s, _ := newTestServer(t, upstream, extra...)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// newTestServer has two direct nodes, a and b; extra lines add to its config.
func newTestServer(t *testing.T, upstream string, extra ...string) (*Server, *nodes.Pool) {
	t.Helper()
	// Two direct nodes; udp differs only to give them distinct IDs. No test
	// starts but the ones a test runs itself.
	cfg := mustParse(t, upstream, `
proxies:
  - {name: a, type: direct, udp: false}
  - {name: b, type: direct, udp: true}
probes: {budget: 0}
data_dir: `+t.TempDir()+"\n"+strings.Join(extra, "\n"))
	store := config.Static(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := nodes.NewPool(store)
	go pool.Run(ctx)
	select {
	case <-pool.Updated():
	case <-time.After(5 * time.Second):
		t.Fatal("nodes never loaded")
	}
	stats := measure.NewStats(store)
	ctrl := control.New(store, pool, stats)
	p, _ := profile.Open("")
	s := New(store, ctrl, stats, cache.Open(t.TempDir(), 1<<20), p)
	t.Cleanup(s.Close)
	return s, pool
}

func mustParse(t *testing.T, upstream, extra string) *config.Config {
	t.Helper()
	if extra == "" {
		extra = "proxies: [{name: d, type: direct}]"
	}
	c, err := config.Parse([]byte("upstream: {url: " + upstream + "}\n" + extra))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPlaybackInfo(t *testing.T) {
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"PlaySessionId":"p9","MediaSources":[{"Id":"ms1","Bitrate":25000000,"Size":1000,
			"DirectStreamUrl":"%s/videos/7/stream.mkv?Static=true&MediaSourceId=ms1",
			"Path":"https://cdn.example/file.mkv","Protocol":"Http"}]}`, upstreamURL)
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL

	px := newTestProxy(t, upstream.URL)
	resp, err := http.Post(px.URL+"/emby/Items/7/PlaybackInfo", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`"DirectStreamUrl":"/videos/7/stream.mkv?Static=true\u0026MediaSourceId=ms1"`,
		`"Path":"/_embolt/https/cdn.example/file.mkv"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("response lacks %s:\n%s", want, body)
		}
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d for a %d-byte body", resp.ContentLength, len(body))
	}
}

func TestSessionIsDevicePlusItem(t *testing.T) {
	c := newCatalog()
	key := func(target string) string {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("X-Emby-Device-Id", "tv")
		k, _ := c.session(r)
		return k
	}
	play := key("/emby/videos/593931/original.mkv?PlaySessionId=a&MediaSourceId=m1")
	scan := key("/emby/videos/593931/original.mkv?PlaySessionId=b")
	other := key("/emby/videos/567817/original.mkv?PlaySessionId=a")
	if play != scan {
		t.Errorf("a scan of the same item is a separate session: %q vs %q", play, scan)
	}
	if play == other {
		t.Errorf("two items share a session: %q", play)
	}
}

func TestItemDetailsNameTheSession(t *testing.T) {
	const details = `{"Id":"7","Type":"Episode","Name":"Pilot","SeriesName":"Show","SeriesId":"3",
		"SeriesPrimaryImageTag":"s1","ParentIndexNumber":1,"IndexNumber":2,"ImageTags":{"Primary":"t1"}}`
	var images atomic.Int32
	var imageQuery, imageAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/emby/Users/u1/Items/7":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, details)
		case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			io.WriteString(w, `{"MediaSources":[{"Id":"ms1","Bitrate":8000000}]}`)
		case r.URL.Path == "/emby/Items/7/Images/Primary":
			images.Add(1)
			imageQuery, imageAuth = r.URL.RawQuery, r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "image/jpeg")
			io.WriteString(w, "jpeg")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL)
	s := px.Config.Handler.(*Server)

	if s.Item("tv/7") != nil {
		t.Fatal("an item is named before its details were fetched")
	}
	resp, err := http.Get(px.URL + "/emby/Users/u1/Items/7")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != details {
		t.Errorf("the player got a changed body:\n%s", body)
	}
	it := s.Item("tv/7")
	if it == nil || it.Name != "Pilot" || it.SeriesName != "Show" || *it.Season != 1 || *it.Episode != 2 || !it.Image {
		t.Fatalf("Item = %+v", it)
	}

	// A session keyed by a MediaSourceId finds the item through PlaybackInfo.
	resp, err = http.Post(px.URL+"/emby/Items/7/PlaybackInfo", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if it := s.Item("tv/ms1"); it == nil || it.ID != "7" {
		t.Errorf("Item by MediaSourceId = %+v", it)
	}

	for range 2 {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/items/7/image", nil)
		r.SetBasicAuth("admin", "pane-password")
		s.ServeImage(w, r, "7")
		if w.Code != http.StatusOK || w.Body.String() != "jpeg" {
			t.Fatalf("image: %d %q", w.Code, w.Body)
		}
	}
	if n := images.Load(); n != 1 {
		t.Errorf("upstream served the image %d times, want 1 then the cache", n)
	}
	if !strings.Contains(imageQuery, "tag=t1") {
		t.Errorf("image query %q lacks the item's tag", imageQuery)
	}
	if imageAuth != "" {
		t.Errorf("the pane's credentials went upstream: %q", imageAuth)
	}
	w := httptest.NewRecorder()
	s.ServeImage(w, httptest.NewRequest(http.MethodGet, "/", nil), "9")
	if w.Code != http.StatusNotFound {
		t.Errorf("an unknown item's image: %d, want 404", w.Code)
	}
}
