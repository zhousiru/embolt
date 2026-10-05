package webapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

const marker = "S3CR3T"

// TestNoSecretLeaks loads marker secrets everywhere a secret can live, runs
// the stack long enough to log, probe and fail, then reads every endpoint.
func TestNoSecretLeaks(t *testing.T) {
	cfg, err := config.Parse([]byte(`
upstream: {url: "http://127.0.0.1:1"}
proxy-providers:
  sub: {type: http, url: "http://127.0.0.1:1/sub?token=` + marker + `"}
proxies:
  - {name: s, type: socks5, server: 127.0.0.1, port: 1, username: u, password: ` + marker + `}
web: {basic_auth: {user: admin, password: ` + marker + `}}
data_dir: ` + t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	journal := NewJournal(100)
	slog.SetDefault(slog.New(journal.Handler(slog.DiscardHandler)))
	store := config.Static(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := nodes.NewPool(store)
	stats := measure.NewStats(store, "")
	ctrl := control.New(store, pool, stats)
	go pool.Run(ctx)
	go ctrl.Run(ctx)
	time.Sleep(2 * time.Second) // a failed refresh and a ping round

	h := New(Deps{Cfg: store, Ctrl: ctrl, Journal: journal, UI: fstest.MapFS{"_shell.html": {Data: []byte("shell")}}})
	paths := []string{"/api/v1/status", "/api/v1/nodes", "/api/v1/events", "/metrics", "/"}
	for _, n := range pool.All() {
		paths = append(paths, "/api/v1/nodes/"+n.ID)
	}
	if _, err := ctrl.Play("tv/1", 20); err == nil {
		paths = append(paths, "/api/v1/sessions/tv/1")
	}
	for _, p := range paths {
		body := get(t, h, p, true)
		if strings.Contains(body, marker) {
			t.Errorf("%s leaks a secret:\n%s", p, body)
		}
	}
	if len(journal.Events()) == 0 {
		t.Error("journal is empty; the canary did not exercise logging")
	}
}

func TestReadOnlyAndAuth(t *testing.T) {
	cfg, _ := config.Parse([]byte("upstream: {url: http://e}\nproxies: [{name: d, type: direct}]\nweb: {basic_auth: {user: a, password: b}}"))
	h := guard(config.Static(cfg), http.NotFoundHandler())
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/status", nil)
	r.SetBasicAuth("a", "b")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d, want 405", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET without auth: %d, want 401", w.Code)
	}
}

func get(t *testing.T, h http.Handler, path string, auth bool) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if auth {
		r.SetBasicAuth("admin", marker)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("GET %s: %d", path, w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	return string(body)
}
