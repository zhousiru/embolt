// Package webapi serves the read-only pane: the embedded UI, /api/v1/*,
// /metrics and /healthz. It accepts only GET and HEAD and serializes only
// view types, so it can neither change routing nor leak a secret.
package webapi

import (
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/view"
)

type Deps struct {
	Cfg     *config.Store
	Ctrl    *control.Controller
	Journal *Journal
	UI      fs.FS // nil in -tags noui builds
	Version string
	Started time.Time
	Items   Items // nil leaves sessions unnamed
}

// Items names what sessions play and serves their thumbnails.
type Items interface {
	Item(sessionKey string) *view.Item
	ServeImage(w http.ResponseWriter, r *http.Request, id string)
}

func New(d Deps) http.Handler {
	gauges := prometheus.NewRegistry()
	gauges.MustRegister(collector{d.Ctrl})
	metrics := promhttp.HandlerFor(prometheus.Gatherers{prometheus.DefaultGatherer, gauges}, promhttp.HandlerOpts{})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		nodes := d.Ctrl.Nodes()
		usable, measured := 0, 0
		for _, n := range nodes {
			if !n.BreakerOpen {
				usable++
				if n.RateMbps.Measured {
					measured++
				}
			}
		}
		testing, paused := d.Ctrl.Testing()
		reply(w, view.Status{
			Version:     d.Version,
			Started:     d.Started,
			Upstream:    d.Cfg.Load().Upstream.Base().Host,
			Primary:     d.Ctrl.PrimaryRef(),
			Nodes:       len(nodes),
			Usable:      usable,
			Measured:    measured,
			Testing:     testing,
			TestsPaused: paused,
			Sessions:    d.named(d.Ctrl.Sessions()),
			Recent:      d.named(d.Ctrl.Recent()),
			Limits:      d.Ctrl.Limits(),
		})
	})
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		reply(w, d.Ctrl.Nodes())
	})
	mux.HandleFunc("GET /api/v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if n, ok := d.Ctrl.Node(r.PathValue("id")); ok {
			reply(w, n)
		} else {
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /api/v1/sessions/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		s := d.Ctrl.Session(key)
		for _, e := range d.Journal.Events() {
			if e.Attrs["session"] == key {
				s.Events = append(s.Events, e)
			}
		}
		if s.Session == nil && len(s.Events) == 0 {
			http.NotFound(w, r)
			return
		}
		if s.Session != nil {
			s.Session = &d.named([]view.Session{*s.Session})[0]
		}
		reply(w, s)
	})
	mux.HandleFunc("GET /api/v1/items/{id}/image", func(w http.ResponseWriter, r *http.Request) {
		if d.Items == nil {
			http.NotFound(w, r)
			return
		}
		d.Items.ServeImage(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		reply(w, d.Journal.Events())
	})
	mux.HandleFunc("GET /api/", http.NotFound)
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /", spa(d.UI))
	return guard(d.Cfg, mux)
}

// named fills in what each session plays, where it is known.
func (d Deps) named(ss []view.Session) []view.Session {
	if d.Items != nil {
		for i := range ss {
			ss[i].Item = d.Items.Item(ss[i].Key)
		}
	}
	return ss
}

// guard allows only reads, adds security headers, and applies the optional
// basic auth, read per request so a config reload takes effect at once.
func guard(cfg *config.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		if ba := cfg.Load().Web.BasicAuth; ba != nil && r.URL.Path != "/healthz" {
			user, pass, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(ba.User))&
				subtle.ConstantTimeCompare([]byte(pass), []byte(ba.Password)) != 1 {
				h.Set("WWW-Authenticate", `Basic realm="embolt"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// spa serves built files, with hashed assets cached forever; any other path
// gets the shell, so client-side routes survive a reload.
func spa(ui fs.FS) http.Handler {
	if ui == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "this build has no UI (-tags noui); the API is under /api/v1/", http.StatusNotFound)
		})
	}
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		isAsset := strings.HasPrefix(name, "assets/")
		if fi, err := fs.Stat(ui, name); err == nil && !fi.IsDir() {
			if isAsset {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		if isAsset {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, ui, "_shell.html")
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
