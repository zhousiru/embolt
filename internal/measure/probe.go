package measure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/zhousiru/embolt/internal/nodes"
)

const (
	speedTestTime  = 4 * time.Second
	speedTestBytes = 10 << 20
	speedTestRamp  = 1 << 20 // read before timing starts: the connection's ramp-up
)

// ErrRefused means the server answered a probe with 403 or 429: probing
// must pause, but the node is not at fault.
var ErrRefused = errors.New("server refused the probe")

// Target is what a speed test reads: the file being played, with the
// player's own credentials. It lives in memory only and is never logged.
type Target struct {
	URL    *url.URL
	Header http.Header
	Size   int64
}

// Ping times a request to the server's /System/Ping on a warm connection:
// the first request opens it (and checks liveness), the second is the RTT.
// It never touches sessions or watch history.
func Ping(ctx context.Context, n *nodes.Node, base *url.URL) (Sample, error) {
	u := base.JoinPath("System/Ping").String()
	var ttfb time.Duration
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return Sample{}, err
		}
		start := time.Now()
		resp, err := n.Control().RoundTrip(req)
		if err != nil {
			return fault(ctx, KindPing, err)
		}
		ttfb = time.Since(start)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}
	return Sample{Kind: KindPing, TTFB: ttfb}, nil
}

// SpeedTest reads up to 4 s or 10 MB of the target from a random 1 MB-aligned
// offset, exactly as a player's range request would. The rate is timed from
// the first 1 MB on, so the connection's ramp-up does not count.
func SpeedTest(ctx context.Context, n *nodes.Node, t Target) (Sample, error) {
	off := int64(0)
	if span := (t.Size - speedTestBytes) >> 20; span > 0 {
		off = rand.Int64N(span) << 20
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL.String(), nil)
	if err != nil {
		return Sample{}, err
	}
	req.Header = t.Header.Clone()
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))

	start := time.Now()
	resp, err := (&http.Client{Transport: n.Media()}).Do(req) // follows a 302 on the same node
	if err != nil {
		return fault(ctx, KindSpeed, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return Sample{}, ErrRefused
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		return Sample{}, fmt.Errorf("speed test: status %d", resp.StatusCode)
	}
	ttfb := time.Since(start)
	stop := time.AfterFunc(speedTestTime, func() { resp.Body.Close() })
	defer stop.Stop()
	if ramp, err := io.CopyN(io.Discard, resp.Body, speedTestRamp); err != nil {
		// Not even 1 MB in 4 s: rate the whole read, ramp-up and all.
		return Sample{Kind: KindSpeed, Bytes: ramp, TTFB: ttfb, Dur: time.Since(start) - ttfb}, nil
	}
	timed := time.Now()
	nbytes, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, speedTestBytes-speedTestRamp))
	return Sample{Kind: KindSpeed, Bytes: nbytes, TTFB: ttfb, Dur: time.Since(timed)}, nil
}

// fault turns a transport error into a node-fault sample, unless the probe
// was cancelled from our side.
func fault(ctx context.Context, kind Kind, err error) (Sample, error) {
	if ctx.Err() != nil {
		return Sample{}, ctx.Err()
	}
	return Sample{Kind: kind, Err: Redact(err)}, nil
}

// Redact describes err without the request URL, which may carry a token.
func Redact(err error) string {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	return err.Error()
}
