// Package proxy looks exactly like the Emby server. It sorts each request
// into the control or media lane and streams media through a read-ahead
// buffer that survives a node failing.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/siruzhou/embolt/internal/cache"
	"github.com/siruzhou/embolt/internal/config"
	"github.com/siruzhou/embolt/internal/control"
	"github.com/siruzhou/embolt/internal/measure"
	"github.com/siruzhou/embolt/internal/nodes"
)

// remotePrefix routes to a separate stream host learned from PlaybackInfo:
// /_embolt/{scheme}/{host}/{path}.
const remotePrefix = "/_embolt/"

type Server struct {
	cfg     *config.Store
	base    *url.URL
	origins []string // spellings of the upstream origin, for stripping
	ctrl    *control.Controller
	stats   *measure.Stats
	cache   *cache.Cache
	catalog *catalog
	links   links
	rp      *httputil.ReverseProxy
}

func New(cfg *config.Store, ctrl *control.Controller, stats *measure.Stats, c *cache.Cache) *Server {
	base := cfg.Load().Upstream.Base()
	s := &Server{
		cfg:     cfg,
		base:    base,
		origins: origins(base),
		ctrl:    ctrl,
		stats:   stats,
		cache:   c,
		catalog: newCatalog(),
		links:   links{m: map[string]link{}},
	}
	s.rp = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		Transport:      routeTransport{s},
		ModifyResponse: s.modifyResponse,
		ErrorHandler:   s.proxyError,
		FlushInterval:  100 * time.Millisecond,
	}
	return s
}

// route travels in the request context from ServeHTTP to the transport.
type route struct {
	lane     lane
	target   *url.URL // upstream base, or a stream host
	node     *nodes.Node
	alt      *nodes.Node // one retry for HLS segments
	cacheKey string
}

type routeKey struct{}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.ctrl.Touch()
	rt := &route{lane: s.classify(r), target: s.base}
	if rest, ok := strings.CutPrefix(r.URL.Path, remotePrefix); ok {
		scheme, rest, _ := strings.Cut(rest, "/")
		host, path, _ := strings.Cut(rest, "/")
		if scheme != "http" && scheme != "https" || !s.catalog.allowsHost(host) {
			http.NotFound(w, r)
			return
		}
		rt.target = &url.URL{Scheme: scheme, Host: host}
		r = r.Clone(r.Context())
		r.URL.Path, r.URL.RawPath = "/"+path, ""
		if rt.lane == laneControl && r.Method == http.MethodGet {
			rt.lane = laneStatic
		}
	}

	if rt.lane == laneStatic {
		s.serveStatic(w, r, rt.target)
		return
	}
	if rt.lane == laneControl {
		n, err := s.ctrl.Primary()
		if err != nil {
			http.Error(w, "embolt: "+err.Error(), http.StatusBadGateway)
			return
		}
		rt.node = n
		if cacheable(r) {
			rt.cacheKey = cache.Key(r)
			if s.cache.Serve(w, r, rt.cacheKey) {
				return
			}
		}
	} else {
		key, mbps := s.catalog.session(r)
		slog.Debug("media request", "path", r.URL.Path, "lane", rt.lane, "session", key, "bitrate_mbps", mbps)
		play, err := s.ctrl.Play(key, mbps)
		if err != nil {
			http.Error(w, "embolt: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer play.Release()
		rt.node = play.Node()
		if rt.lane == laneHLS && !strings.HasSuffix(strings.ToLower(r.URL.Path), ".m3u8") {
			rt.alt = play.Failover(rt.node)
		}
	}
	s.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
}

func routeOf(r *http.Request) *route { return r.Context().Value(routeKey{}).(*route) }

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	rt := routeOf(pr.In)
	pr.SetURL(rt.target)
	s.fixHeaders(pr.Out.Header, rt.target)
	if reInfo.MatchString(pr.In.URL.Path) || reDetails.MatchString(pr.In.URL.Path) || rt.lane == laneHLS {
		pr.Out.Header.Del("Accept-Encoding") // bodies we rewrite must arrive plain
	}
}

// fixHeaders makes a player's request look like it was sent to the upstream:
// Origin and Referer name the upstream, and nothing reveals the client.
func (s *Server) fixHeaders(h http.Header, target *url.URL) {
	origin := target.Scheme + "://" + target.Host
	if h.Get("Origin") != "" {
		h.Set("Origin", origin)
	}
	if ref, err := url.Parse(h.Get("Referer")); err == nil && ref.Host != "" {
		h.Set("Referer", origin+ref.RequestURI())
	}
	for _, k := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		h.Del(k)
	}
}

func (s *Server) modifyResponse(resp *http.Response) error {
	rt := routeOf(resp.Request)
	if loc := resp.Header.Get("Location"); loc != "" {
		if rel, ok := s.stripOrigin(loc); ok {
			resp.Header.Set("Location", rel)
		}
	}
	path := resp.Request.URL.Path
	switch {
	case rt.cacheKey != "":
		s.cache.Store(rt.cacheKey, resp, query(resp.Request.URL, "tag") != "")
	case resp.Request.Method == http.MethodPost && reInfo.MatchString(path):
		return s.playbackInfo(resp, reInfo.FindStringSubmatch(path)[1])
	case resp.Request.Method == http.MethodGet && reDetails.MatchString(path):
		return s.itemDetails(resp)
	case rt.lane == laneHLS && strings.HasSuffix(strings.ToLower(path), ".m3u8"):
		return s.rewriteBody(resp)
	}
	return nil
}

// rewriteBody makes absolute upstream URLs in a playlist relative, so
// segments come back through the proxy.
func (s *Server) rewriteBody(resp *http.Response) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	for _, o := range s.origins {
		raw = bytes.ReplaceAll(raw, []byte(o), nil)
	}
	setBody(resp, raw)
	return nil
}

// setBody replaces a response body that was read and rewritten.
func setBody(resp *http.Response, raw []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
}

func (s *Server) stripOrigin(raw string) (string, bool) {
	for _, o := range s.origins {
		if rest, ok := strings.CutPrefix(raw, o); ok && (rest == "" || rest[0] == '/' || rest[0] == '?') {
			return "/" + strings.TrimPrefix(rest, "/"), true
		}
	}
	return "", false
}

// origins lists how the upstream may spell its own origin, longest first.
func origins(u *url.URL) []string {
	o := []string{u.Scheme + "://" + u.Host}
	if p := u.Port(); p == "443" && u.Scheme == "https" || p == "80" && u.Scheme == "http" {
		o = append(o, u.Scheme+"://"+u.Hostname())
	} else if p == "" {
		port := map[string]string{"https": "443", "http": "80"}[u.Scheme]
		o = append([]string{u.Scheme + "://" + u.Host + ":" + port}, o...)
	}
	return o
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		slog.Debug("upstream request failed", "path", r.URL.Path, "err", measure.Redact(err))
	}
	w.WriteHeader(http.StatusBadGateway)
}

// routeTransport sends each request through its route's node: the control
// transport for API traffic, the media transport for video.
type routeTransport struct{ s *Server }

func (t routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := routeOf(req)
	resp, err := t.s.send(rt.node, req, rt.lane == laneControl)
	if err != nil && rt.alt != nil && req.Context().Err() == nil {
		return t.s.send(rt.alt, req, false)
	}
	return resp, err
}

// send performs one request on n. A transport error counts against the node;
// an upstream 5xx does not, and a request without a body retries it.
func (s *Server) send(n *nodes.Node, req *http.Request, control bool) (*http.Response, error) {
	tr := n.Media()
	if control {
		tr = n.Control()
	}
	do := func() (*http.Response, error) { return tr.RoundTrip(req) }
	var resp *http.Response
	var err error
	if (req.Body == nil || req.Body == http.NoBody) && req.Header.Get("Upgrade") == "" {
		resp, err = retry5xx(req.Context(), do)
	} else {
		resp, err = do()
	}
	s.observe(req.Context(), n, err)
	return resp, err
}

// retry5xx sends once more, 2 s later, when the server answers 5xx: the
// server's fault, not the node's.
func retry5xx(ctx context.Context, do func() (*http.Response, error)) (*http.Response, error) {
	resp, err := do()
	if err != nil || resp.StatusCode < 500 {
		return resp, err
	}
	resp.Body.Close()
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return do()
}

func (s *Server) observe(ctx context.Context, n *nodes.Node, err error) {
	switch {
	case err == nil:
		s.stats.Healthy(n)
	case ctx.Err() == nil && !errors.Is(err, context.Canceled):
		s.stats.Record(n, measure.Sample{Kind: measure.KindPassive, Err: measure.Redact(err)})
	}
}

// links caches where a redirect-fronted server sent a node, per node and
// media URL, so seeks skip the front hop. IP-bound links stay with the node
// that resolved them; a failover resolves again on its new node.
type links struct {
	mu sync.Mutex
	m  map[string]link
}

type link struct {
	u       *url.URL
	expires time.Time
}

const linkTTL = 10 * time.Minute

func (l *links) get(key string) (*url.URL, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k, ok := l.m[key]
	if !ok || time.Now().After(k.expires) {
		delete(l.m, key)
		return nil, false
	}
	return k.u, true
}

func (l *links) put(key string, u *url.URL) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > catalogCap {
		clear(l.m)
	}
	l.m[key] = link{u, time.Now().Add(linkTTL)}
}

func (l *links) drop(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}
