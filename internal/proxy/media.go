package proxy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

const (
	chunkSize   = 64 << 10
	maxRing     = 256 << 20
	stallAfter  = 4 * time.Second  // no bytes this long: fail over at once
	pauseLimit  = 30 * time.Second // ring full this long: close the upstream
	stepEvery   = 2 * time.Second  // controller evaluation period
	maxAttempts = 3
)

var (
	errStall   = errors.New("stall")
	errPaused  = errors.New("paused")
	errChanged = errors.New("file changed upstream")

	underrun = promauto.NewCounter(prometheus.CounterOpts{
		Name: "embolt_session_stall_seconds_total",
		Help: "Seconds player connections waited on an empty read-ahead buffer.",
	})
	chunks = sync.Pool{New: func() any { return new([chunkSize]byte) }}
)

// switchTo is the cause given when the controller moves a stream.
type switchTo struct{ n *nodes.Node }

func (switchTo) Error() string { return "risk" }

// stream serves one ranged read. A reader fills a memory ring from the media
// node; the handler drains it to the player. When the node fails, the reader
// resumes at the end of the buffered data on another node, and the player
// sees one unbroken response.
type stream struct {
	s       *Server
	play    *control.Stream
	req     *http.Request // outbound template: target URL and player headers
	bitrate float64

	// Owned by the reader goroutine.
	node      *nodes.Node
	off, end  int64 // next byte to fetch; last byte wanted, -1 if unknown
	total     int64 // full size from Content-Range, -1 if unknown
	validator string

	opened   time.Time
	ring     chan []byte
	buffered atomic.Int64 // bytes in the ring
	sent     atomic.Int64 // bytes handed to the player
	meter    meter

	mu      sync.Mutex
	current context.CancelCauseFunc // the live upstream attempt
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, target *url.URL) {
	key, mbps := s.catalog.session(r)
	play, err := s.ctrl.Play(key, mbps)
	if err != nil {
		http.Error(w, "embolt: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer play.Release()

	out := target.JoinPath(r.URL.EscapedPath())
	out.RawQuery = r.URL.RawQuery
	req, _ := http.NewRequest(http.MethodGet, out.String(), nil)
	req.Header = r.Header.Clone()
	stripHopByHop(req.Header)
	s.fixHeaders(req.Header, target)

	st := &stream{s: s, play: play, req: req, bitrate: play.Bitrate(), end: -1, total: -1, opened: time.Now()}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resp, err := st.start(ctx, r.Header.Get("Range"))
	if err != nil {
		http.Error(w, "embolt: no node could open the stream", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		relay(w, resp) // 401, 404, 416, a passed-on redirect: the player decides
		return
	}
	st.parseRange(resp)
	s.ctrl.SetTarget(measure.Target{URL: req.URL, Header: speedTestHeader(req.Header), Size: st.total})
	writeHeader(w, resp)

	ringBytes := min(int64(s.cfg.Load().Control.ReadAhead.Seconds()*st.bitrate*1e6/8), maxRing)
	st.ring = make(chan []byte, max(ringBytes/chunkSize, 16))
	go st.fill(ctx, resp)
	go st.supervise(ctx)
	st.drain(w)
}

// start opens the player's range on the session's media node, failing over
// if the node cannot connect.
func (st *stream) start(ctx context.Context, rng string) (*http.Response, error) {
	n := st.play.Node()
	for range maxAttempts {
		resp, err := st.open(ctx, n, rng, "")
		if err == nil {
			if n != st.play.Node() {
				st.play.Switched(n, "error")
			}
			st.node = n
			st.meter.restart()
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if n = st.play.Failover(n); n == nil {
			break
		}
	}
	return nil, control.ErrNoNode
}

// open sends one upstream attempt on n under its own cancellable context.
// A redirect-fronted server's 302 is followed on the same node, with the
// player's headers, and the final link is reused for later seeks.
func (st *stream) open(ctx context.Context, n *nodes.Node, rng, ifRange string) (*http.Response, error) {
	actx, cancel := context.WithCancelCause(ctx)
	req := st.req.Clone(actx)
	setOrDel(req.Header, "Range", rng)
	setOrDel(req.Header, "If-Range", ifRange)

	resp, err := st.s.fetch(n, req)
	st.s.observe(ctx, n, err)
	if err != nil {
		cancel(err)
		return nil, err
	}
	st.mu.Lock()
	st.current = cancel
	st.mu.Unlock()
	resp.Body = &attempt{resp.Body, actx, cancel}
	return resp, nil
}

func (s *Server) fetch(n *nodes.Node, req *http.Request) (*http.Response, error) {
	if s.cfg.Load().Upstream.Redirect == "pass" {
		return n.Media().RoundTrip(req)
	}
	key := n.ID + " " + req.URL.String()
	if u, ok := s.links.get(key); ok {
		cdn := req.Clone(req.Context())
		cdn.URL, cdn.Host = u, ""
		resp, err := n.Media().RoundTrip(cdn)
		if err != nil || resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusGone {
			return resp, err
		}
		resp.Body.Close() // the signed link expired: resolve it again
		s.links.drop(key)
	}
	client := &http.Client{Transport: n.Media()}
	resp, err := retry5xx(req.Context(), func() (*http.Response, error) { return client.Do(req) })
	if err == nil && resp.Request.URL.Host != req.URL.Host {
		s.links.put(key, resp.Request.URL)
	}
	return resp, err
}

// fill copies upstream into the ring, resuming after every interruption
// until the range is done, the player leaves, or no node can continue.
func (st *stream) fill(ctx context.Context, resp *http.Response) {
	defer close(st.ring)
	for {
		err := st.pump(ctx, resp.Body.(*attempt))
		resp.Body.Close()
		if err == nil || ctx.Err() != nil {
			return
		}
		if resp, err = st.resume(ctx, err); err != nil {
			if ctx.Err() == nil { // not the player leaving
				slog.Warn("stream ended early; the player will re-request", "err", err)
			}
			return
		}
	}
}

// pump reads one upstream response. Its watchdog fails the attempt when no
// byte arrives for 4 s while the ring has room.
func (st *stream) pump(ctx context.Context, a *attempt) error {
	watchdog := time.AfterFunc(stallAfter, func() { a.cancel(errStall) })
	defer watchdog.Stop()
	for st.end < 0 || st.off <= st.end {
		buf := chunks.Get().(*[chunkSize]byte)[:]
		t0 := time.Now()
		n, err := io.ReadFull(a, buf)
		if s, ok := st.meter.read(n, time.Since(t0)); ok {
			st.s.stats.Record(st.node, s)
		}
		if n > 0 {
			watchdog.Stop()
			st.buffered.Add(int64(n))
			perr := st.push(ctx, buf[:n], a.cancel)
			if perr != nil && !errors.Is(perr, errPaused) {
				return perr
			}
			st.off += int64(n)
			if perr != nil {
				return perr
			}
			watchdog.Reset(stallAfter)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if st.end < 0 || st.off > st.end {
				return nil
			}
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return a.cause(err)
		}
	}
	return nil
}

// push hands a chunk to the drain. When the ring stays full for 30 s the
// player has paused; the upstream closes and reopens at the ring's end later.
func (st *stream) push(ctx context.Context, b []byte, cancel context.CancelCauseFunc) error {
	select {
	case st.ring <- b:
		return nil
	default:
	}
	st.meter.paused()
	idle := time.NewTimer(pauseLimit)
	defer idle.Stop()
	select {
	case st.ring <- b:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-idle.C:
		cancel(errPaused)
	}
	select {
	case st.ring <- b:
		return errPaused
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resume reopens the remaining range after an interruption. A risk switch
// goes to the controller's choice; a stall or error fails over to the
// standby; a pause reopens on the same node once the player has drained half
// the ring. If-Range guards against a file that changed: a 200 instead of 206
// ends the response.
func (st *stream) resume(ctx context.Context, cause error) (*http.Response, error) {
	n, reason := st.node, ""
	var sw switchTo
	switch {
	case errors.As(cause, &sw):
		n, reason = sw.n, "risk"
	case errors.Is(cause, errPaused):
		if err := st.waitForRoom(ctx); err != nil {
			return nil, err
		}
	default:
		reason = "error"
		if errors.Is(cause, errStall) {
			reason = "stall"
		}
		st.s.stats.Record(st.node, measure.Sample{Kind: measure.KindPassive, Err: measure.Redact(cause)})
		n = st.play.Failover(st.node)
	}
	rng := fmt.Sprintf("bytes=%d-", st.off)
	if st.end >= 0 {
		rng += strconv.FormatInt(st.end, 10)
	}
	for range maxAttempts {
		if n == nil {
			break
		}
		resp, err := st.open(ctx, n, rng, st.validator)
		switch {
		case err != nil:
		case resp.StatusCode == http.StatusPartialContent && st.sameFile(resp):
			if n != st.node {
				st.play.Switched(n, reason)
				st.node = n
				st.meter.moved()
			} else {
				st.meter.restart()
			}
			return resp, nil
		case resp.StatusCode == http.StatusOK:
			resp.Body.Close()
			return nil, errChanged
		default:
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reason = cmp.Or(reason, "error")
		n = st.play.Failover(n)
	}
	return nil, control.ErrNoNode
}

func (st *stream) waitForRoom(ctx context.Context) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for len(st.ring) > cap(st.ring)/2 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}

// supervise runs the controller step for this stream every 2 s.
func (st *stream) supervise(ctx context.Context) {
	tick := time.NewTicker(stepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			full, recent := st.meter.take()
			obs := control.Observation{
				ReadAhead: st.media(st.buffered.Load()),
				Delivered: st.media(st.sent.Load()),
				Elapsed:   time.Since(st.opened),
				Full:      full,
				Recent:    recent,
			}
			if n := st.play.Step(obs); n != nil {
				st.mu.Lock()
				st.current(switchTo{n})
				st.mu.Unlock()
			}
		}
	}
}

// drain writes the ring to the player until it ends or the player leaves.
func (st *stream) drain(w io.Writer) {
	for {
		var b []byte
		var ok bool
		select {
		case b, ok = <-st.ring:
		default:
			t0 := time.Now()
			b, ok = <-st.ring
			underrun.Add(time.Since(t0).Seconds())
		}
		if !ok {
			return
		}
		st.buffered.Add(-int64(len(b)))
		n, err := w.Write(b)
		st.sent.Add(int64(n))
		chunks.Put((*[chunkSize]byte)(b[:chunkSize]))
		if err != nil {
			return // the player closed: a seek or a stop
		}
	}
}

// media converts bytes to seconds of media at the session's bitrate.
func (st *stream) media(bytes int64) time.Duration {
	return time.Duration(float64(bytes) * 8 / (st.bitrate * 1e6) * float64(time.Second))
}

func (st *stream) parseRange(resp *http.Response) {
	st.validator = resp.Header.Get("ETag")
	if st.validator == "" || strings.HasPrefix(st.validator, "W/") {
		st.validator = resp.Header.Get("Last-Modified")
	}
	if first, last, total, ok := contentRange(resp.Header.Get("Content-Range")); ok {
		st.off, st.end, st.total = first, last, total
	} else if resp.ContentLength >= 0 {
		st.off, st.end, st.total = 0, resp.ContentLength-1, resp.ContentLength
	}
}

func (st *stream) sameFile(resp *http.Response) bool {
	first, _, total, ok := contentRange(resp.Header.Get("Content-Range"))
	return ok && first == st.off && (st.total < 0 || total == st.total)
}

// contentRange parses "bytes first-last/total"; total is -1 for "*".
func contentRange(v string) (first, last, total int64, ok bool) {
	rng, size, found := strings.Cut(strings.TrimPrefix(v, "bytes "), "/")
	a, b, found2 := strings.Cut(rng, "-")
	if !found || !found2 {
		return 0, 0, 0, false
	}
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	total, err3 := strconv.ParseInt(size, 10, 64)
	if size == "*" {
		total, err3 = -1, nil
	}
	return first, last, total, err1 == nil && err2 == nil && err3 == nil
}

// attempt is one upstream response body under its own cancellable context.
type attempt struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func (a *attempt) Close() error {
	err := a.ReadCloser.Close()
	a.cancel(nil)
	return err
}

// cause prefers why the attempt was cancelled (stall, switch, pause) over
// the read error that the cancellation produced.
func (a *attempt) cause(err error) error {
	if a.ctx.Err() != nil {
		return context.Cause(a.ctx)
	}
	return err
}

func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	writeHeader(w, resp)
	io.Copy(w, resp.Body)
}

// writeHeader sends resp's status and end-to-end headers.
func writeHeader(w http.ResponseWriter, resp *http.Response) {
	maps.Copy(w.Header(), resp.Header)
	stripHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)
}

// speedTestHeader keeps what a speed test needs to read as the player:
// its credentials and identity, without conditionals or ranges.
func speedTestHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		out.Del(k)
	}
	return out
}

var hopByHop = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func stripHopByHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

func setOrDel(h http.Header, k, v string) {
	if v == "" {
		h.Del(k)
	} else {
		h.Set(k, v)
	}
}
