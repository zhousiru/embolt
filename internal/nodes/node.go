// Package nodes turns subscriptions into nodes: stable IDs, diversity keys,
// and two HTTP transports per node that dial through its mihomo outbound.
package nodes

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Node is one mihomo outbound. Its fields never change after creation; a
// refresh that alters a node yields a new ID.
type Node struct {
	ID       string // hash of protocol, server, port and credentials
	Name     string
	Protocol string
	Provider string
	Subnet   string // server /24 (or /48), else hostname: a diversity key

	out *outbound

	once           sync.Once
	control, media *http.Transport
}

func newNode(provider string, mapping map[string]any) (*Node, error) {
	name, _ := mapping["name"].(string)
	typ, _ := mapping["type"].(string)
	out, err := newOutbound(mapping)
	if err != nil {
		return nil, fmt.Errorf("node %q: %w", name, err)
	}
	host, _ := mapping["server"].(string)
	return &Node{
		ID:       nodeID(mapping),
		Name:     name,
		Protocol: typ,
		Provider: provider,
		Subnet:   subnet(host),
		out:      out,
	}, nil
}

// nodeID hashes everything but the name, so stats survive renames.
// encoding/json sorts map keys, which makes the encoding canonical.
func nodeID(mapping map[string]any) string {
	m := make(map[string]any, len(mapping))
	for k, v := range mapping {
		if k != "name" {
			m[k] = v
		}
	}
	raw, _ := json.Marshal(m)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}

func subnet(host string) string {
	if host == "" { // direct and other serverless outbounds
		return ""
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ips, err := lookupServer(ctx, host)
		if err != nil || len(ips) == 0 {
			return host
		}
		addr = ips[0]
	}
	bits := 24
	if addr.Unmap().Is6() {
		bits = 48
	}
	p, _ := addr.Unmap().Prefix(bits)
	return p.String()
}

func (n *Node) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return n.out.DialContext(ctx, network, addr)
}

func (n *Node) init() {
	n.once.Do(func() {
		n.control = &http.Transport{
			DialContext:           n.dial,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
		n.media = &http.Transport{
			DialContext:           n.dial,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1 only
			DisableCompression:    true,
			ReadBufferSize:        256 << 10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		}
	})
}

// Control is the transport for API traffic: HTTP/2, keep-alive.
func (n *Node) Control() http.RoundTripper { n.init(); return n.control }

// Media is the transport for video: HTTP/1.1, no compression, big reads.
// Each stream gets its own connection, so one stall never blocks another.
func (n *Node) Media() http.RoundTripper { n.init(); return n.media }

func (n *Node) close() {
	n.init()
	n.control.CloseIdleConnections()
	n.media.CloseIdleConnections()
	n.out.Close()
}

func (n *Node) String() string {
	if n == nil {
		return "none"
	}
	return n.Name
}
