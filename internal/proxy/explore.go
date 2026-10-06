package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// A test reads a stretch of a file through an explored node, beside the
// main read's feed, which keeps reading on the media node: the bytes just
// past its read-ahead window, up to control.ProbeBytes for up to
// control.ProbeTime. It drops what it reads, so it shares nothing with the
// read-ahead, and a node that fails it costs the session nothing.

var errUnserved = errors.New("not served") // the node answered, but not with the stretch

// test is one speed test of a file's stretch. Its bytes are dropped, so it
// never checks that the file is unchanged.
type test struct {
	s         *Server
	req       *http.Request // the main read's outbound template
	from, end int64         // first and last byte
}

// test plans a test of the stretch past main's window, or of the file's
// last bytes when the window reaches its end. It requires ra.mu.
func (f *file) test(main *span) test {
	from := max(0, min(main.reach+f.window, f.total-control.ProbeBytes))
	return test{s: main.feed.s, req: main.feed.req, from: from, end: min(from+control.ProbeBytes, f.total) - 1}
}

// run reads the test through n and records what it says of n: its rate, or
// a failure, which never counts against the session's media node.
func (t test) run(ctx context.Context, play *control.Stream, n *nodes.Node) {
	m := meter{kind: measure.KindExplore, warm: rampTime}
	got, err := t.read(ctx, n, &m)
	if smp, ok := m.end(); ok {
		t.s.stats.Record(n, smp)
	}
	if err != nil && !errors.Is(err, errUnserved) && !errors.Is(err, context.Canceled) {
		t.s.stats.Record(n, measure.Sample{Kind: measure.KindExplore, Err: measure.Redact(err)})
	}
	play.Probed(got)
	mbps := 0.0
	if m.totalDur > 0 {
		mbps = math.Round(float64(m.total)*8/m.totalDur.Seconds()/1e5) / 10
	}
	errMsg := ""
	if err != nil {
		errMsg = measure.Redact(err)
	}
	st := t.s.stats.State(n)
	slog.Info("explored", "session", play.Key(), "node", n.Name, "mb", got>>20, "mbps", mbps,
		"node_mbps", math.Round(st.Rate.Typical()*10)/10, "node_known", st.Rate.Known(), "err", errMsg)
}

// read fetches the stretch until it is done, its time is up, or the node
// fails: no answer within 2·stallAfter, or no byte for stallAfter. It returns
// the bytes read.
func (t test) read(ctx context.Context, n *nodes.Node, m *meter) (int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(2*stallAfter, func() { cancel(errStall) })
	defer watchdog.Stop()
	req := t.req.Clone(ctx)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", t.from, t.end))
	req.Header.Del("If-Range")
	resp, err := t.s.fetch(n, req)
	t.s.observe(ctx, n, err)
	if err != nil {
		return 0, causeOf(ctx, err)
	}
	defer resp.Body.Close()
	if first, _, _, ok := contentRange(resp.Header.Get("Content-Range")); resp.StatusCode != http.StatusPartialContent ||
		!ok || first != t.from {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			t.s.ctrl.Refused()
		}
		return 0, errUnserved
	}
	buf := chunks.Get().(*[chunkSize]byte)
	defer chunks.Put(buf)
	var got int64
	until := time.Now().Add(control.ProbeTime)
	for t.from+got <= t.end && time.Now().Before(until) {
		watchdog.Reset(stallAfter)
		t0 := time.Now()
		k, err := io.ReadFull(resp.Body, buf[:min(chunkSize, t.end+1-t.from-got)])
		if smp, ok := m.read(k, time.Since(t0)); ok {
			t.s.stats.Record(n, smp)
		}
		got += int64(k)
		exploreBytes.Add(float64(k))
		if err != nil {
			return got, causeOf(ctx, err)
		}
	}
	return got, nil
}

// causeOf prefers why ctx was cancelled (a stall, the file closing) over the
// error the cancellation produced.
func causeOf(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}
