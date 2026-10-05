package nodes

// This is the only file that imports mihomo. Embolt uses its outbounds and
// subscription parsers, and its DNS client for node servers; never its
// rules, inbounds or tunnel.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/common/convert"
	"github.com/metacubex/mihomo/component/resolver"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/dns"
	"go.yaml.in/yaml/v3"

	"github.com/siruzhou/embolt/internal/config"
)

// outbound dials TCP through one mihomo proxy.
type outbound struct{ p C.Proxy }

func newOutbound(mapping map[string]any) (*outbound, error) {
	p, err := adapter.ParseProxy(mapping)
	if err != nil {
		return nil, err
	}
	return &outbound{p}, nil
}

// DialContext passes domains through unresolved, so the node resolves them
// remotely and the route matches what a player on that node would get.
func (o *outbound) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	m := &C.Metadata{NetWork: C.TCP, Type: C.INNER, Host: host, DstPort: uint16(port)}
	if ip, err := netip.ParseAddr(host); err == nil {
		m.Host, m.DstIP = "", ip
	}
	return o.p.DialContext(ctx, m)
}

func (o *outbound) Close() error { return o.p.Close() }

// lookupServer resolves a node's server the way its outbound will when it
// dials, so diversity keys match the addresses actually used.
func lookupServer(ctx context.Context, host string) ([]netip.Addr, error) {
	return resolver.LookupIPWithResolver(ctx, host, resolver.ProxyServerHostResolver)
}

// UseDNS points mihomo's resolver for node servers at c's nameservers; an
// empty c leaves mihomo on the OS's servers.
func UseDNS(c config.DNS) error {
	servers := c.ProxyServerNameserver
	if len(servers) == 0 {
		servers = c.Nameserver
	}
	if len(servers) == 0 {
		resolver.ProxyServerHostResolver = nil
		return nil
	}
	main, err := nameservers(servers)
	if err != nil {
		return err
	}
	bootstrap, err := nameservers(c.DefaultNameserver)
	if err != nil {
		return err
	}
	for _, ns := range append(bootstrap, main...) {
		if host := serverHost(ns); host != "" && !isIP(host) && len(bootstrap) == 0 {
			return fmt.Errorf("dns: nameserver %s is a hostname; add default-nameserver IPs to resolve it", host)
		}
	}
	for _, ns := range bootstrap {
		if !isIP(serverHost(ns)) {
			return fmt.Errorf("dns: default-nameserver %s must be an IP", ns.Addr)
		}
	}
	resolver.ProxyServerHostResolver = dns.NewResolver(dns.Config{Main: main, Default: bootstrap, IPv6: c.IPv6}).Resolver
	return nil
}

// nameservers parses mihomo's nameserver syntax for the plain and encrypted
// schemes: 1.2.3.4, udp://, tcp://, tls://, https:// and quic://.
func nameservers(list []string) ([]dns.NameServer, error) {
	var out []dns.NameServer
	for _, s := range list {
		raw := s
		if ip, err := netip.ParseAddr(s); err == nil {
			raw = "udp://" + netip.AddrPortFrom(ip, 53).String()
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("dns: bad nameserver %q", s)
		}
		ns := dns.NameServer{Net: u.Scheme}
		switch u.Scheme {
		case "udp":
			ns.Net, ns.Addr = "", withPort(u.Host, "53")
		case "tcp":
			ns.Addr = withPort(u.Host, "53")
		case "tls", "quic":
			ns.Addr = withPort(u.Host, "853")
		case "https":
			ns.Addr = (&url.URL{Scheme: "https", Host: withPort(u.Host, "443"), Path: u.Path}).String()
		default:
			return nil, fmt.Errorf("dns: nameserver %q: want udp, tcp, tls, https or quic", s)
		}
		out = append(out, ns)
	}
	return out, nil
}

func withPort(host, port string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

func serverHost(ns dns.NameServer) string {
	hostport := ns.Addr
	if u, err := url.Parse(ns.Addr); err == nil && u.Scheme == "https" {
		hostport = u.Host
	}
	host, _, _ := net.SplitHostPort(hostport)
	return host
}

func isIP(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// parseSubscription accepts a Clash/mihomo YAML document with a proxies
// list, or a base64/plain list of share links.
func parseSubscription(buf []byte) ([]map[string]any, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(buf, &doc); err == nil && doc.Proxies != nil {
		return doc.Proxies, nil
	}
	proxies, err := convert.ConvertsV2Ray(buf)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		return nil, errors.New("subscription has no proxies")
	}
	return proxies, nil
}
