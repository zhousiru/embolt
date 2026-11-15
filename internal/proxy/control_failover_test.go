package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A control request whose node drops the connection is sent again, body
// and all, on another node, which takes over as primary.
func TestControlFailsOverOnTransportError(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if requests.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close() // EOF on the first node
			return
		}
		if string(body) != `{"x":1}` {
			t.Errorf("replayed body %q", body)
		}
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL)
	px := httptest.NewServer(s)
	defer px.Close()

	first, err := s.ctrl.Primary()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(px.URL+"/emby/Items/1/PlaybackInfo", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "ok" {
		t.Fatalf("status %d body %q, want 200 ok", resp.StatusCode, got)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("upstream saw %d requests, want 2", n)
	}
	if now, _ := s.ctrl.Primary(); now == first {
		t.Errorf("primary stayed on %s after it failed", first.Name)
	}
}

// When every node fails, the player gets 502 after at most controlAttempts.
func TestControlFailoverGivesUp(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer upstream.Close()
	s, _ := newTestServerNodes(t, upstream.URL, fourDirectNodes)
	px := httptest.NewServer(s)
	defer px.Close()

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(px.URL + "/emby/System/Info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if n := requests.Load(); n != controlAttempts {
		t.Errorf("upstream saw %d requests, want %d", n, controlAttempts)
	}
}
