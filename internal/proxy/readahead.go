package proxy

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/zhousiru/embolt/internal/control"
)

// The read-ahead is shared by a player's connections. A player may read a
// file through many connections and drop one at every jump: across a badly
// interleaved file, a little way back, or to a seek preview. Each feed reads
// one span of the file ahead of the players reading it. A request that starts
// within a span, or just past the end it has fetched, reads from that span
// instead of opening an upstream request of its own, which through a node
// costs seconds. Where a player left a span stays held for a while, and a
// span that loses its players keeps its bytes and its upstream connection,
// so a player that comes back finds both.

const (
	minAhead   = 8 << 20          // read-ahead of a span that is not the file's main read
	keepBehind = 4 << 20          // bytes kept behind a reader or a mark, for players that step back
	joinReach  = 4 << 20          // a request this far past a span's end waits for it
	keepIdle   = 30 * time.Second // how long a span keeps where a player left it, and its bytes once it has none
	maxSpans   = 4                // per file; an idle span gives way to a new one
	maxHeld    = 512 << 20        // bytes held by every read-ahead together
	mainDecay  = 10 * time.Second // memory of a span's delivery: its share of playback
	revisit    = 10 * time.Second // a span read this recently is still being played
)

var heldBytes = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "embolt_readahead_bytes",
	Help: "Bytes held in read-ahead buffers.",
})

// readAhead holds the spans of every file being played. One lock guards it
// all: spans, their readers and their bytes.
type readAhead struct {
	mu    sync.Mutex
	files map[string]*file
	held  int64
}

// file is one media file of one session. While it has spans it holds the
// session's controller stream: one step per file judges every region the
// player reads, and moves every feed.
type file struct {
	ra        *readAhead
	key       string
	total     int64 // -1 until a feed answers
	validator string
	spans     []*span

	play    *control.Stream // set by the first feed, released with the last span
	bitrate float64         // Mbps
	fetched int64           // bytes read from upstream by every feed
	window  int64           // read-ahead of its main read, in bytes
	stop    context.CancelFunc
}

// span is a contiguous run of a file's bytes, filled by one feed and read
// by any number of the player's connections.
type span struct {
	f      *file
	feed   *feed       // set once its feed is created
	header http.Header // end-to-end headers of the feed's first answer
	total  int64

	ready   bool // the feed answered: header, total and end are known
	dead    bool // no request may join: it failed, its file changed, or it was dropped
	dropped bool // its bytes are released
	done    bool // the feed has ended

	from   int64 // first byte held: the offset of chunks[0]
	chunks []*[chunkSize]byte
	to     int64 // end of the bytes held: the feed's next byte
	end    int64 // last byte the feed reads, -1 until known

	readers  map[*reader]struct{}
	marks    []mark      // where readers left, within keepIdle
	reach    int64       // furthest byte handed to a player
	recent   float64     // bytes handed to players, decaying over mainDecay
	recentAt time.Time   // when bytes were last handed, or the span opened
	idle     *time.Timer // drops the span once it has had no reader for keepIdle

	wake chan struct{} // closed and replaced on every change
}

// reader is one player connection reading a span.
type reader struct{ pos int64 }

// mark is where a reader left a span. A player reading a badly interleaved
// file through one connection leaves one region for the other, and comes
// back a little further on: its bytes are kept until then.
type mark struct {
	pos int64
	at  time.Time
}

func newReadAhead() *readAhead { return &readAhead{files: map[string]*file{}} }

// fileKey names a file of a session: its path on its host, and the media
// source the player chose.
func fileKey(session string, target *url.URL, r *http.Request) string {
	return strings.Join([]string{session, target.Host, strings.ToLower(r.URL.Path), query(r.URL, "MediaSourceId")}, "\x00")
}

// The methods below require ra.mu.

func (ra *readAhead) file(key string) *file {
	f := ra.files[key]
	if f == nil {
		f = &file{ra: ra, key: key, total: -1}
		ra.files[key] = f
	}
	return f
}

// join attaches rd to the span best placed to serve it: one holding its
// first byte, else one about to fetch it. It returns nil if none is.
func (f *file) join(rd *reader) *span {
	var best *span
	var bestGap int64 = math.MaxInt64
	for _, sp := range f.spans {
		gap := int64(-1)
		switch {
		case sp.dead:
		case sp.ready && sp.from <= rd.pos && rd.pos < sp.to:
			gap = 0
		case !sp.ready && sp.from <= rd.pos && rd.pos < sp.from+joinReach:
			gap = rd.pos - sp.from
		case sp.ready && !sp.done && sp.to <= rd.pos && rd.pos < sp.to+joinReach:
			gap = rd.pos - sp.to
		}
		if gap >= 0 && gap < bestGap {
			best, bestGap = sp, gap
		}
	}
	if best != nil {
		best.attach(rd)
	}
	return best
}

// open starts a span at rd's first byte, making room for it.
func (f *file) open(rd *reader) *span {
	if len(f.spans) >= maxSpans {
		var oldest *span
		for _, sp := range f.spans {
			if len(sp.readers) == 0 && (oldest == nil || sp.recentAt.Before(oldest.recentAt)) {
				oldest = sp
			}
		}
		if oldest != nil {
			oldest.drop()
		}
	}
	now := time.Now()
	sp := &span{f: f, total: -1, from: rd.pos, to: rd.pos, end: -1, reach: rd.pos,
		readers: map[*reader]struct{}{}, recentAt: now, wake: make(chan struct{})}
	f.spans = append(f.spans, sp)
	sp.attach(rd)
	return sp
}

func (sp *span) notify() {
	close(sp.wake)
	sp.wake = make(chan struct{})
}

func (sp *span) attach(rd *reader) {
	sp.readers[rd] = struct{}{}
	sp.reach = max(sp.reach, rd.pos)
	if sp.idle != nil {
		sp.idle.Stop()
	}
	sp.notify()
}

// detach removes rd, marking where it left. A span left without readers
// lingers for keepIdle, or goes at once if no request may join it.
func (sp *span) detach(rd *reader) {
	delete(sp.readers, rd)
	sp.marks = append(sp.marks, mark{rd.pos, time.Now()})
	if len(sp.readers) > 0 {
		sp.trim()
		return
	}
	if sp.dead {
		sp.drop()
		return
	}
	ra := sp.f.ra
	if sp.idle == nil {
		sp.idle = time.AfterFunc(keepIdle, func() {
			ra.mu.Lock()
			defer ra.mu.Unlock()
			if len(sp.readers) == 0 {
				sp.drop()
			}
		})
	} else {
		sp.idle.Reset(keepIdle)
	}
	sp.notify()
}

// answered records the feed's first answer. An answer that shows the file
// changed upstream retires the file's other spans.
func (sp *span) answered(header http.Header, total, end int64, validator string) {
	f := sp.f
	if f.total >= 0 && (f.total != total || f.validator != validator) {
		for _, o := range f.spans {
			if o != sp {
				o.retire()
			}
		}
	}
	f.total, f.validator = total, validator
	sp.header, sp.total, sp.end, sp.ready = header, total, end, true
	sp.notify()
}

// retire stops a span's feed and lets no request join it; its readers
// finish what it holds.
func (sp *span) retire() {
	sp.dead = true
	if sp.feed != nil {
		sp.feed.cancel()
	}
	if len(sp.readers) == 0 {
		sp.drop()
	}
	sp.notify()
}

// drop releases a span that has no readers.
func (sp *span) drop() {
	if sp.dropped {
		return
	}
	sp.dead, sp.dropped, sp.done = true, true, true
	if sp.idle != nil {
		sp.idle.Stop()
	}
	if sp.feed != nil {
		sp.feed.cancel()
	}
	ra := sp.f.ra
	for _, c := range sp.chunks {
		chunks.Put(c)
	}
	ra.hold(-int64(len(sp.chunks)) * chunkSize)
	sp.chunks = nil
	f := sp.f
	for i, o := range f.spans {
		if o == sp {
			f.spans = append(f.spans[:i], f.spans[i+1:]...)
			break
		}
	}
	if len(f.spans) == 0 && ra.files[f.key] == f {
		delete(ra.files, f.key)
		f.close()
	}
	sp.notify()
}

func (ra *readAhead) hold(n int64) {
	ra.held += n
	heldBytes.Set(float64(ra.held))
}

// append stores bytes the feed read at the span's end. It reports false
// once the span is dropped.
func (sp *span) append(b []byte) bool {
	if sp.dropped {
		return false
	}
	for len(b) > 0 {
		i, off := (sp.to-sp.from)/chunkSize, (sp.to-sp.from)%chunkSize
		if int(i) == len(sp.chunks) {
			sp.chunks = append(sp.chunks, chunks.Get().(*[chunkSize]byte))
			sp.f.ra.hold(chunkSize)
		}
		n := copy(sp.chunks[i][off:], b)
		b = b[n:]
		sp.to += int64(n)
	}
	sp.notify()
	return true
}

// at returns the held bytes from pos, within one chunk and up to last.
func (sp *span) at(pos, last int64) []byte {
	i, off := (pos-sp.from)/chunkSize, (pos-sp.from)%chunkSize
	n := min(chunkSize-off, sp.to-pos)
	if last >= 0 {
		n = min(n, last-pos+1)
	}
	return sp.chunks[i][off : off+n]
}

// advance records that rd handed n bytes to its player, and frees what every
// reader has passed.
func (sp *span) advance(rd *reader, n int64) {
	rd.pos += n
	sp.reach = max(sp.reach, rd.pos)
	now := time.Now()
	sp.recent = sp.decayed(now) + float64(n)
	sp.recentAt = now
	sp.trim()
	sp.notify()
}

// trim frees chunks wholly more than keepBehind behind every reader and
// every mark, and forgets marks older than keepIdle.
func (sp *span) trim() {
	now := time.Now()
	sp.marks = slices.DeleteFunc(sp.marks, func(m mark) bool { return now.Sub(m.at) > keepIdle })
	low := sp.reach
	for rd := range sp.readers {
		low = min(low, rd.pos)
	}
	for _, m := range sp.marks {
		low = min(low, m.pos)
	}
	ra := sp.f.ra
	for len(sp.chunks) > 0 && sp.from+chunkSize <= low-keepBehind {
		chunks.Put(sp.chunks[0])
		sp.chunks = sp.chunks[1:]
		sp.from += chunkSize
		ra.hold(-chunkSize)
	}
}

func (sp *span) decayed(now time.Time) float64 {
	return sp.recent * math.Exp(-now.Sub(sp.recentAt).Seconds()/mainDecay.Seconds())
}

// playing reports whether a player reads the span: one is connected, or
// read from it within revisit, as a player does that reads two regions of a
// badly interleaved file in turn.
func (sp *span) playing(now time.Time) bool {
	return sp.ready && !sp.dead && (len(sp.readers) > 0 || now.Sub(sp.recentAt) < revisit)
}

// fetched reports whether the span holds its feed's last byte: with nothing
// left to fetch, it cannot run dry.
func (sp *span) fetched() bool { return sp.end >= 0 && sp.to > sp.end }

// lead returns the file's main read, the span still fetching that delivered
// most lately of those being played, or nil; and what the spans being played
// delivered lately in all, for each one's share of playback.
func (f *file) lead(now time.Time) (main *span, total float64) {
	for _, sp := range f.spans {
		if !sp.playing(now) {
			continue
		}
		d := sp.decayed(now)
		total += d
		if !sp.fetched() && (main == nil || d > main.decayed(now)) {
			main = sp
		}
	}
	return main, total
}

// ahead is the bytes held past the furthest byte handed to a player.
func (sp *span) ahead() int64 { return sp.to - sp.reach }

// target is how far the span reads ahead: the file's window for its main
// read; for another region, as long in playback at its share of the bitrate,
// and never under minAhead.
func (sp *span) target() int64 {
	now, f := time.Now(), sp.f
	main, total := f.lead(now)
	if sp == main {
		return f.window
	}
	share := 0.0
	if total > 0 {
		share = sp.decayed(now) / total
	}
	return min(f.window, max(minAhead, int64(share*float64(f.window))))
}

// room reports whether the feed may read on: the span is being played, it
// holds less than its target, and the read-aheads together are within
// budget, which never starves a span below minAhead.
func (sp *span) room() bool {
	a := sp.ahead()
	return sp.playing(time.Now()) && a < sp.target() &&
		(a < minAhead || sp.f.ra.held < maxHeld)
}

// acquire gives the file its session's controller stream, and starts its
// supervisor, when its first feed opens.
func (f *file) acquire(s *Server, key string, mbps float64) error {
	if f.play != nil {
		return nil
	}
	play, err := s.ctrl.Play(key, mbps)
	if err != nil {
		return err
	}
	f.play, f.bitrate = play, play.Bitrate()
	f.window = min(int64(s.cfg.Load().Control.ReadAhead.Seconds()*f.bitrate*1e6/8), maxWindow)
	ctx, cancel := context.WithCancel(context.Background())
	f.stop = cancel
	go f.supervise(ctx)
	return nil
}

// close releases the file's controller stream once it has no span.
func (f *file) close() {
	if f.play != nil {
		f.stop()
		f.play.Release()
		f.play = nil
	}
}

// maxAhead is the most read-ahead a file reports.
const maxAhead = 2 * time.Minute

// observe is what the controller's step judges, and the file's main read;
// nil while no region still fetching is being played.
//
// The read-ahead is in seconds of playback. Each region holds its bytes ahead
// at its share of the bitrate, the share it delivered lately: a file's audio,
// stored apart from its video, plays a small share of its bytes, and its few
// megabytes last minutes. It is the shortest of the regions still fetching,
// since the player stalls when any runs dry, and at most maxAhead, which
// is plenty: a file fully fetched reports that. Played is what the regions
// being played delivered lately, which picks the lead among a session's files.
func (f *file) observe(now time.Time) (control.Observation, *span) {
	main, total := f.lead(now)
	if main == nil {
		return control.Observation{}, nil
	}
	secs := maxAhead.Seconds()
	for _, sp := range f.spans {
		if !sp.playing(now) || sp.fetched() {
			continue
		}
		share := 1.0
		if total > 0 {
			share = sp.decayed(now) / total
		}
		if share > 0 { // else opened, not read yet: no share to judge it at
			secs = min(secs, float64(sp.ahead())*8/(share*f.bitrate*1e6))
		}
	}
	return control.Observation{
		ReadAhead: time.Duration(secs * float64(time.Second)),
		Played:    total,
	}, main
}

// supervise runs the controller step for the file every 2 s while a region
// of it is being played. A switch moves every feed; a test reads past the
// main read's window, beside it.
func (f *file) supervise(ctx context.Context) {
	tick := time.NewTicker(stepEvery)
	defer tick.Stop()
	ra := f.ra
	ra.mu.Lock()
	fetched, at := f.fetched, time.Now()
	ra.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ra.mu.Lock()
		now := time.Now()
		obs, main := f.observe(now)
		obs.Fetched = float64(f.fetched-fetched) * 8 / now.Sub(at).Seconds() / 1e6
		fetched, at = f.fetched, now
		var feeds []*feed
		for _, sp := range f.spans {
			if sp.feed != nil && sp.ready {
				feeds = append(feeds, sp.feed)
			}
		}
		var mf *feed
		if main != nil {
			mf = main.feed
		}
		play := f.play
		ra.mu.Unlock()
		for _, fd := range feeds {
			if full := fd.meter.full(); fd == mf {
				obs.Full = full
			}
		}
		if mf == nil || play == nil {
			continue // nothing played: nothing to judge
		}
		if n, why := play.Step(obs); n != nil {
			for _, fd := range feeds {
				fd.redirect(switchTo{n, why})
			}
			continue
		}
		if n := play.Explore(); n != nil {
			ra.mu.Lock()
			t := f.test(main)
			ra.mu.Unlock()
			go t.run(ctx, play, n)
		}
	}
}

// Close ends every read-ahead's upstream reads; players still reading get
// what is held.
func (s *Server) Close() {
	ra := s.ra
	ra.mu.Lock()
	defer ra.mu.Unlock()
	for _, f := range ra.files {
		for _, sp := range slices.Clone(f.spans) {
			sp.retire()
		}
	}
}

// serveStatic answers a ranged read of a media file from a shared span,
// opening one when no span can serve it. A span checks a request's If-Match
// and If-Range itself, and answers when it holds the version they name. A
// request Embolt cannot answer from a span (several ranges, another
// conditional request, a version the span does not hold) goes to upstream as
// is.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, target *url.URL) {
	key, mbps := s.catalog.session(r)
	req := s.outbound(r, target)
	first, last, ok := parseRange(r.Header.Get("Range"))
	if !ok || conditional(r.Header) {
		s.passThrough(w, req, key, mbps)
		return
	}
	want := versions{ifMatch: r.Header.Get("If-Match")}
	if r.Header.Get("Range") != "" { // without a Range, If-Range is ignored
		want.ifRange = r.Header.Get("If-Range")
	}
	fk := fileKey(key, target, r)
	for range 2 { // a span that fails to open sends the requests that joined it to open their own
		rd := &reader{pos: first}
		ra := s.ra
		ra.mu.Lock()
		f := ra.file(fk)
		if f.total >= 0 && first >= f.total {
			total := f.total
			ra.mu.Unlock()
			unsatisfiable(w, total)
			return
		}
		sp := f.join(rd)
		if sp == nil {
			sp = f.open(rd)
			ra.mu.Unlock()
			resp, err := s.openSpan(sp, req, key, mbps)
			if err != nil || resp != nil {
				if err != nil {
					http.Error(w, "embolt: no node could open the stream", http.StatusBadGateway)
				} else {
					relay(w, resp) // 401, 404, 416, a passed-on redirect: the player decides
				}
				ra.mu.Lock()
				sp.detach(rd) // the span is dead: this drops it, and ends its feed
				ra.mu.Unlock()
				return
			}
		} else {
			ra.mu.Unlock()
		}
		switch s.read(w, r, sp, rd, last, want) {
		case served:
			return
		case stale:
			s.passThrough(w, req, key, mbps) // upstream answers: 412, or the whole file
			return
		}
	}
	http.Error(w, "embolt: no node could open the stream", http.StatusBadGateway)
}

// answer says how read answered a request.
type answer int

const (
	served   answer = iota // it wrote a response, or the player left
	unopened               // sp failed to open; nothing was written
	stale                  // sp holds another version than the request names; nothing was written
)

// versions are the preconditions on a request a span checks itself: the
// If-Match that VLC sends on every read once it knows the file's ETag, and
// the If-Range a player sends resuming a read. Empty means none.
type versions struct{ ifMatch, ifRange string }

// read waits for sp to open and answers rd's request from it, if sp holds
// the version the request wants.
func (s *Server) read(w http.ResponseWriter, r *http.Request, sp *span, rd *reader, last int64, want versions) answer {
	ra, ctx := s.ra, r.Context()
	ra.mu.Lock()
	defer func() {
		sp.detach(rd)
		ra.mu.Unlock()
	}()
	for !sp.ready && !sp.dead {
		if !sp.wait(ctx) {
			return served
		}
	}
	if !sp.ready {
		return unopened
	}
	if !sp.holds(want) {
		return stale
	}
	if rd.pos >= sp.total {
		ra.mu.Unlock()
		unsatisfiable(w, sp.total)
		ra.mu.Lock()
		return served
	}
	if last < 0 || last >= sp.total {
		last = sp.total - 1
	}
	header, total, first := sp.header, sp.total, rd.pos
	done := sp.f.play.Watch()
	defer done()
	ra.mu.Unlock()

	h := w.Header()
	maps.Copy(h, header)
	h.Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	if r.Header.Get("Range") == "" {
		w.WriteHeader(http.StatusOK)
	} else {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, total))
		w.WriteHeader(http.StatusPartialContent)
	}

	ra.mu.Lock()
	for rd.pos <= last {
		for rd.pos >= sp.to && !sp.done {
			t0 := time.Now()
			ok := sp.wait(ctx)
			underrun.Add(time.Since(t0).Seconds())
			if !ok {
				return served
			}
		}
		if rd.pos >= sp.to {
			return served // the feed ended early: the player re-requests
		}
		b := sp.at(rd.pos, last)
		ra.mu.Unlock()
		n, err := w.Write(b)
		ra.mu.Lock()
		sp.advance(rd, int64(n))
		if err != nil {
			return served // the player closed: a seek or a stop
		}
	}
	return served
}

// holds reports whether sp holds the version of the file a request wants.
// If-Match names it by "*" or a list of strong ETags; If-Range by its strong
// ETag or its Last-Modified date (RFC 9110, 13.1). Requires ra.mu.
func (sp *span) holds(want versions) bool {
	etag := sp.header.Get("ETag")
	strong := etag != "" && !strings.HasPrefix(etag, "W/")
	if want.ifMatch != "" && want.ifMatch != "*" &&
		!(strong && slices.ContainsFunc(strings.Split(want.ifMatch, ","), func(t string) bool { return strings.TrimSpace(t) == etag })) {
		return false
	}
	return want.ifRange == "" || strong && want.ifRange == etag || want.ifRange == sp.header.Get("Last-Modified")
}

// woke says why a wait on a span returned.
type woke int

const (
	changed   woke = iota // the span changed
	cancelled             // ctx ended
	alarmed               // the alarm fired
)

// waitOr releases ra.mu until the span changes, ctx ends or alarm fires.
func (sp *span) waitOr(ctx context.Context, alarm <-chan time.Time) woke {
	wake, ra := sp.wake, sp.f.ra
	ra.mu.Unlock()
	defer ra.mu.Lock()
	select {
	case <-wake:
		return changed
	case <-ctx.Done():
		return cancelled
	case <-alarm:
		return alarmed
	}
}

// wait releases ra.mu until sp changes; it reports false if ctx ends first.
func (sp *span) wait(ctx context.Context) bool { return sp.waitOr(ctx, nil) == changed }

// outbound is the upstream request template for a player's request.
func (s *Server) outbound(r *http.Request, target *url.URL) *http.Request {
	out := target.JoinPath(r.URL.EscapedPath())
	out.RawQuery = r.URL.RawQuery
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, out.String(), nil)
	req.Header = r.Header.Clone()
	stripHopByHop(req.Header)
	s.fixHeaders(req.Header, target)
	return req
}

// passThrough sends a request Embolt cannot answer from a span to upstream
// as it is, on the session's media node, and relays the answer. A node that
// cannot connect is failed over, at most maxAttempts tries in all.
func (s *Server) passThrough(w http.ResponseWriter, req *http.Request, key string, mbps float64) {
	play, err := s.ctrl.Play(key, mbps)
	if err != nil {
		http.Error(w, "embolt: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer play.Release()
	defer play.Watch()()
	ctx := req.Context()
	n := play.Node()
	for range maxAttempts {
		if n == nil {
			break
		}
		resp, err := s.fetch(n, req.Clone(ctx))
		s.observe(ctx, n, err)
		if err == nil {
			relay(w, resp)
			return
		}
		if ctx.Err() != nil {
			return
		}
		n = play.Failover(n)
	}
	http.Error(w, "embolt: no node could open the stream", http.StatusBadGateway)
}

// parseRange reads a request's single byte range; no Range is the whole
// file. last is -1 for an open range. A suffix range or several ranges are
// not ok.
func parseRange(v string) (first, last int64, ok bool) {
	if v == "" {
		return 0, -1, true
	}
	spec, found := strings.CutPrefix(v, "bytes=")
	a, b, found2 := strings.Cut(spec, "-")
	if !found || !found2 || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	if err != nil || first < 0 {
		return 0, 0, false
	}
	if b = strings.TrimSpace(b); b == "" {
		return first, -1, true
	}
	last, err = strconv.ParseInt(b, 10, 64)
	return first, last, err == nil && last >= first
}

// conditional reports whether a request carries a precondition other than
// If-Match and If-Range, which a span checks itself.
func conditional(h http.Header) bool {
	for _, k := range []string{"If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if h.Get(k) != "" {
			return true
		}
	}
	return false
}

func unsatisfiable(w http.ResponseWriter, total int64) {
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(total, 10))
	http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
}
