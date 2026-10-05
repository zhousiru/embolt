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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

const (
	chunkSize   = 64 << 10
	maxWindow   = 256 << 20        // most read-ahead of one file's main read
	stallAfter  = 4 * time.Second  // no bytes this long: fail over at once
	pauseLimit  = 30 * time.Second // read-ahead full this long: close the upstream
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
	exploreBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "embolt_explore_bytes_total",
		Help: "Bytes read by speed tests through explored nodes; none of it is played.",
	})
	chunks = sync.Pool{New: func() any { return new([chunkSize]byte) }}
)

// switchTo is the cause given when the controller moves a feed.
type switchTo struct{ n *nodes.Node }

func (switchTo) Error() string { return "risk" }

// feed fills one span from upstream. It opens the span's range on the
// session's media node, and when the node fails, resumes at the span's end
// on another node, so the players reading the span see one unbroken stream.
type feed struct {
	s    *Server
	sp   *span
	play *control.Stream // the file's
	req  *http.Request   // outbound template: target URL and the first player's headers

	ctx    context.Context // ends when the span goes
	cancel context.CancelFunc

	// Owned by the feed's goroutine once it runs.
	node      *nodes.Node
	off, end  int64 // next byte to fetch; last byte wanted, -1 if unknown
	total     int64 // full size from Content-Range, -1 if unknown
	validator string
	meter     meter

	mu   sync.Mutex
	live *attempt // the latest upstream attempt
}

// openSpan creates sp's feed and opens its range upstream. On an answer the
// span can serve, the feed runs and openSpan returns nil, nil. Any other
// answer is returned for the caller to relay; the span takes no more
// requests, and its feed ends when the caller detaches.
func (s *Server) openSpan(sp *span, req *http.Request, key string, mbps float64) (*http.Response, error) {
	ra := s.ra
	ra.mu.Lock()
	if err := sp.f.acquire(s, key, mbps); err != nil {
		sp.retire()
		ra.mu.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	fd := &feed{s: s, sp: sp, play: sp.f.play, req: req.WithContext(ctx), ctx: ctx, cancel: cancel, end: -1, total: -1}
	sp.feed = fd
	ra.mu.Unlock()

	resp, err := fd.start(fmt.Sprintf("bytes=%d-", sp.from))
	ra.mu.Lock()
	defer ra.mu.Unlock()
	switch {
	case err != nil:
		sp.retire()
		return nil, err
	case !fd.serves(resp, sp.from):
		sp.dead = true // not retire: that would end the answer's body
		sp.notify()
		return resp, nil
	case sp.dead: // its file changed while it opened
		resp.Body.Close()
		return nil, errChanged
	}
	header := resp.Header.Clone()
	stripHopByHop(header)
	header.Del("Content-Length")
	header.Del("Content-Range")
	sp.answered(header, fd.total, fd.end, fd.validator)
	go fd.run(resp)
	return nil, nil
}

// serves reports whether resp can fill a span from first: a 206 from that
// byte, or a 200 of the whole file, of a known size.
func (fd *feed) serves(resp *http.Response, first int64) bool {
	switch resp.StatusCode {
	case http.StatusPartialContent:
		fd.parseRange(resp)
		return fd.off == first && fd.total >= 0
	case http.StatusOK:
		fd.parseRange(resp)
		return first == 0 && fd.total >= 0
	}
	return false
}

// redirect ends the feed's live attempt with cause, for resume to act on. A
// feed that is paused or already resuming follows the session by itself.
func (fd *feed) redirect(cause error) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if a := fd.live; a != nil {
		a.cancel(cause)
	}
}

// run fills the span until the file ends, the span goes, or no node can
// continue.
func (fd *feed) run(resp *http.Response) {
	fd.fill(fd.ctx, resp)
	ra := fd.s.ra
	ra.mu.Lock()
	defer ra.mu.Unlock()
	fd.sp.done = true
	if fd.sp.dropped {
		return
	}
	fd.sp.notify()
}

// start opens the span's range on the session's media node, failing over if
// the node cannot connect.
func (fd *feed) start(rng string) (*http.Response, error) {
	n := fd.play.Node()
	for range maxAttempts {
		resp, err := fd.open(fd.ctx, n, rng, "")
		if err == nil {
			if n != fd.play.Node() {
				fd.play.Switched(n, "error")
			}
			fd.node = n
			fd.meter.restart()
			return resp, nil
		}
		if fd.ctx.Err() != nil {
			return nil, fd.ctx.Err()
		}
		if n = fd.play.Failover(n); n == nil {
			break
		}
	}
	return nil, control.ErrNoNode
}

// open sends one upstream attempt on n under its own cancellable context.
// A redirect-fronted server's 302 is followed on the same node, with the
// player's headers, and the final link is reused for later seeks.
func (fd *feed) open(ctx context.Context, n *nodes.Node, rng, ifRange string) (*http.Response, error) {
	actx, cancel := context.WithCancelCause(ctx)
	req := fd.req.Clone(actx)
	setOrDel(req.Header, "Range", rng)
	setOrDel(req.Header, "If-Range", ifRange)

	resp, err := fd.s.fetch(n, req)
	fd.s.observe(ctx, n, err)
	if err != nil {
		cancel(err)
		return nil, err
	}
	a := &attempt{resp.Body, actx, cancel}
	fd.mu.Lock()
	fd.live = a
	fd.mu.Unlock()
	resp.Body = a
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

// fill copies upstream into the span, resuming after every interruption
// until the range is done, the span goes, or no node can continue.
func (fd *feed) fill(ctx context.Context, resp *http.Response) error {
	for {
		err := fd.pump(resp.Body.(*attempt))
		resp.Body.Close()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if resp, err = fd.resume(ctx, err); err != nil {
			if ctx.Err() == nil { // not the span going
				slog.Warn("stream ended early; the player will re-request", "err", err)
			}
			return err
		}
	}
}

// pump reads one upstream response while the span has room. Its watchdog
// fails the attempt when no byte arrives for 4 s while it has room.
func (fd *feed) pump(a *attempt) error {
	watchdog := time.AfterFunc(stallAfter, func() { a.cancel(errStall) })
	defer watchdog.Stop()
	buf := chunks.Get().(*[chunkSize]byte)
	defer chunks.Put(buf)
	ra := fd.s.ra
	for fd.end < 0 || fd.off <= fd.end {
		watchdog.Stop()
		if err := fd.awaitRoom(a); err != nil {
			return err
		}
		watchdog.Reset(stallAfter)
		t0 := time.Now()
		n, err := io.ReadFull(a, buf[:])
		if s, ok := fd.meter.read(n, time.Since(t0)); ok {
			fd.s.stats.Record(fd.node, s)
		}
		if n > 0 {
			ra.mu.Lock()
			kept := fd.sp.append(buf[:n])
			ra.mu.Unlock()
			if !kept {
				return context.Canceled
			}
			fd.off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if fd.end < 0 || fd.off > fd.end {
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

// awaitRoom waits until the span has room for more. When it stays full for
// 30 s its players have paused or left: the upstream closes, and reopens at
// the span's end once there is room. A redirect ends the wait at once.
func (fd *feed) awaitRoom(a *attempt) error {
	ra := fd.s.ra
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if fd.sp.room() {
		return nil
	}
	fd.meter.paused()
	idle := time.NewTimer(pauseLimit)
	defer idle.Stop()
	for !fd.sp.room() {
		switch fd.sp.waitOr(a.ctx, idle.C) {
		case cancelled:
			return context.Cause(a.ctx)
		case alarmed:
			a.cancel(errPaused)
			return errPaused
		}
	}
	return nil
}

// resume reopens the remaining range after an interruption. A risk switch
// goes to the controller's choice; a stall or error fails over to the best
// other node, or follows the session if another feed already moved it; a
// pause reopens on the session's media node once the players have drained
// half the read-ahead. Only a node that cannot connect is failed over: any
// answer from the server ends the feed, since another node would get the
// same. If-Range guards against a file that changed: a 200 instead of 206.
func (fd *feed) resume(ctx context.Context, cause error) (*http.Response, error) {
	rng := fmt.Sprintf("bytes=%d-", fd.off)
	if fd.end >= 0 {
		rng += strconv.FormatInt(fd.end, 10)
	}
	n, reason := fd.node, ""
	var sw switchTo
	switch {
	case errors.As(cause, &sw):
		n, reason = sw.n, "risk"
	case errors.Is(cause, errPaused):
		if err := fd.waitForRoom(ctx); err != nil {
			return nil, err
		}
		n = fd.play.Node()
	default:
		reason = "error"
		if errors.Is(cause, errStall) {
			reason = "stall"
		}
		fd.s.stats.Record(fd.node, measure.Sample{Kind: measure.KindPassive, Err: measure.Redact(cause)})
		if n = fd.play.Node(); n == fd.node {
			n = fd.play.Failover(fd.node)
		}
	}
	for range maxAttempts {
		if n == nil {
			break
		}
		resp, err := fd.open(ctx, n, rng, fd.validator)
		switch {
		case err != nil:
		case resp.StatusCode == http.StatusPartialContent && fd.sameFile(resp):
			fd.node = n
			fd.meter.restart()
			if n != fd.play.Node() {
				fd.play.Switched(n, reason)
			}
			return resp, nil
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
			resp.Body.Close()
			return nil, errChanged
		default: // the server's fault, not the node's
			resp.Body.Close()
			return nil, fmt.Errorf("upstream answered %s", resp.Status)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reason = cmp.Or(reason, "error")
		n = fd.play.Failover(n)
	}
	return nil, control.ErrNoNode
}

// waitForRoom waits until the span's players have drained half its
// read-ahead.
func (fd *feed) waitForRoom(ctx context.Context) error {
	ra := fd.s.ra
	ra.mu.Lock()
	defer ra.mu.Unlock()
	for !fd.sp.room() || fd.sp.ahead() > fd.sp.target()/2 {
		if !fd.sp.wait(ctx) {
			return ctx.Err()
		}
	}
	return nil
}

func (fd *feed) parseRange(resp *http.Response) {
	fd.validator = resp.Header.Get("ETag")
	if fd.validator == "" || strings.HasPrefix(fd.validator, "W/") {
		fd.validator = resp.Header.Get("Last-Modified")
	}
	if first, last, total, ok := contentRange(resp.Header.Get("Content-Range")); ok {
		fd.off, fd.end, fd.total = first, last, total
	} else if resp.ContentLength >= 0 {
		fd.off, fd.end, fd.total = 0, resp.ContentLength-1, resp.ContentLength
	}
}

func (fd *feed) sameFile(resp *http.Response) bool {
	first, _, total, ok := contentRange(resp.Header.Get("Content-Range"))
	return ok && first == fd.off && (fd.total < 0 || total == fd.total)
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
