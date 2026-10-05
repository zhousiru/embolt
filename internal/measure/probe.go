package measure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/zhousiru/embolt/internal/nodes"
)

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
