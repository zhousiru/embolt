package nodes

import (
	"testing"

	"github.com/zhousiru/embolt/internal/config"
)

func TestNameservers(t *testing.T) {
	got, err := nameservers([]string{"223.5.5.5", "tls://223.5.5.5", "https://dns.alidns.com/dns-query", "quic://[2400:3200::1]"})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ net, addr string }{
		{"", "223.5.5.5:53"},
		{"tls", "223.5.5.5:853"},
		{"https", "https://dns.alidns.com:443/dns-query"},
		{"quic", "[2400:3200::1]:853"},
	}
	for i, w := range want {
		if got[i].Net != w.net || got[i].Addr != w.addr {
			t.Errorf("%d: got %s %s, want %s %s", i, got[i].Net, got[i].Addr, w.net, w.addr)
		}
	}
	if _, err := nameservers([]string{"dhcp://en0"}); err == nil {
		t.Error("unsupported scheme accepted")
	}
}

func TestUseDNSNeedsBootstrapForHostnames(t *testing.T) {
	if err := UseDNS(config.DNS{ProxyServerNameserver: []string{"https://doh.pub/dns-query"}}); err == nil {
		t.Error("a DoH hostname without default-nameserver was accepted")
	}
	if err := UseDNS(config.DNS{DefaultNameserver: []string{"https://doh.pub/dns-query"}, Nameserver: []string{"1.1.1.1"}}); err == nil {
		t.Error("a hostname in default-nameserver was accepted")
	}
	if err := UseDNS(config.DNS{DefaultNameserver: []string{"tls://223.5.5.5"}, ProxyServerNameserver: []string{"https://doh.pub/dns-query"}}); err != nil {
		t.Error(err)
	}
	UseDNS(config.DNS{})
}
