package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fourDirectNodes = `
  - {name: a, type: direct, udp: false}
  - {name: b, type: direct, udp: true}
  - {name: c, type: direct, udp: false, ip-version: ipv4}
  - {name: d, type: direct, udp: true, ip-version: ipv4}
`

// A truncated body followed by a denied exit must still arrive intact in
// one player response, using the original byte offset and validator.
func TestForbiddenFailoverPreservesStream(t *testing.T) {
	for _, midstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("midstream=%v", midstream), func(t *testing.T) {
			file := bytes.Repeat([]byte("0123456789abcdef"), 1<<17)
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := requests.Add(1)
				w.Header().Set("ETag", `"v1"`)
				if midstream && call == 1 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(file)-1, len(file)))
					w.Header().Set("Content-Length", fmt.Sprint(len(file)-1000))
					w.WriteHeader(http.StatusPartialContent)
					w.Write(file[1000 : 1000+chunkSize])
					return // unexpected EOF
				}
				if (!midstream && call == 1) || (midstream && call == 2) {
					http.Error(w, "exit forbidden", http.StatusForbidden)
					return
				}
				if midstream && (r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", 1000+chunkSize, len(file)-1) || r.Header.Get("If-Range") != `"v1"`) {
					t.Errorf("resume headers: Range=%q If-Range=%q", r.Header.Get("Range"), r.Header.Get("If-Range"))
				}
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
			}))
			defer upstream.Close()
			s, _ := newTestServerNodes(t, upstream.URL, fourDirectNodes)
			px := httptest.NewServer(s)
			defer px.Close()
			req, _ := http.NewRequest(http.MethodGet, px.URL+"/Videos/42/stream?Static=true&PlaySessionId=forbidden", nil)
			req.Header.Set("Range", "bytes=1000-")
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusPartialContent || !bytes.Equal(got, file[1000:]) {
				t.Fatalf("status=%d bytes=%d err=%v; want intact 206", resp.StatusCode, len(got), err)
			}
			want := int32(2)
			if midstream {
				want = 3
			}
			if requests.Load() != want {
				t.Fatalf("requests=%d want=%d", requests.Load(), want)
			}
		})
	}
}

// Denials are bounded even with more nodes available; other client errors
// retain their original response instead of cycling through exits.
func TestMediaHTTPErrorPolicy(t *testing.T) {
	for _, code := range []int{401, 403, 404, 410, 416, 429} {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/passthrough=%v", code, passthrough), func(t *testing.T) {
				var mu sync.Mutex
				peers := map[string]int{}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					peers[r.RemoteAddr]++
					mu.Unlock()
					http.Error(w, "denied", code)
				}))
				defer upstream.Close()
				s, _ := newTestServerNodes(t, upstream.URL, fourDirectNodes)
				req := httptest.NewRequest(http.MethodGet, "/Videos/42/stream?Static=true&PlaySessionId=denied", nil)
				req.Header.Set("Range", "bytes=0-")
				if passthrough {
					req.Header.Set("Range", "bytes=-100")
				}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, req)
				if w.Code != code {
					t.Fatalf("status=%d want=%d", w.Code, code)
				}
				want := 1
				if code == 403 {
					want = maxAttempts
				}
				mu.Lock()
				defer mu.Unlock()
				if len(peers) != want {
					t.Fatalf("peers=%v want %d distinct exits", peers, want)
				}
				for peer, count := range peers {
					if count != 1 {
						t.Errorf("exit %s retried %d times", peer, count)
					}
				}
			})
		}
	}
}

func TestResumeHTTPErrorPolicy(t *testing.T) {
	for _, code := range []int{200, 206, 401, 403, 404, 410, 416, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("ETag", `"changed"`)
				if code == 206 {
					w.Header().Set("Content-Range", "bytes 100-1024/1025")
				}
				w.WriteHeader(code)
			}))
			defer upstream.Close()
			s, _ := newTestServerNodes(t, upstream.URL, fourDirectNodes)
			play, err := s.ctrl.Play("tv/1", 20)
			if err != nil {
				t.Fatal(err)
			}
			defer play.Release()
			req, _ := http.NewRequest(http.MethodGet, upstream.URL+"/Videos/42/stream", nil)
			fd := &feed{s: s, play: play, req: req, node: play.Node(), off: 100, end: 1023, total: 1024, validator: `"original"`}
			resp, err := fd.resume(t.Context(), switchTo{play.Node(), "risk"})
			if resp != nil {
				resp.Body.Close()
				t.Fatal("unexpected successful resume")
			}
			if err == nil {
				t.Fatal("missing error")
			}
			if (code == 200 || code == 206) && err != errChanged {
				t.Fatalf("got %v, want file changed", err)
			}
			want := int32(1)
			if code == 403 {
				want = maxAttempts
			}
			if code == 503 {
				want = 2
			}
			if requests.Load() != want {
				t.Fatalf("requests=%d want=%d", requests.Load(), want)
			}
			if s.ctrl.Sessions()[0].Failovers != 0 {
				t.Fatal("failed resume counted as successful switch")
			}
		})
	}
}
