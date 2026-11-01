package config

import (
	"strings"
	"testing"
	"time"
)

func TestMinimalConfigGetsDefaults(t *testing.T) {
	c, err := Parse([]byte(`
upstream: {url: "https://emby.example:443"}
proxy-providers:
  data:
    type: http
    url: https://sub.example/api
    exclude-filter: '(?:流量|到期)'
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Control.ReadAhead != time.Minute || c.Listen != ":8096" {
		t.Errorf("defaults not applied: %+v", c.Control)
	}
	p := c.Providers["data"]
	if p.Keep("剩余流量：985 GB") || !p.Keep("🇯🇵日本高速01") {
		t.Error("exclude-filter not applied")
	}
}

func TestValidation(t *testing.T) {
	_, err := Parse([]byte(`
upstream: {url: "not a url", redirect: maybe}
control: {read_ahead: -1s}
`))
	for _, want := range []string{"upstream.url", "upstream.redirect", "no nodes", "read_ahead"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %s", err, want)
		}
	}
}

func TestExcludeJapaneseNodeNames(t *testing.T) {
	c, err := Parse([]byte(`
upstream: {url: "https://emby.example"}
proxy-providers:
  data:
    type: http
    url: https://sub.example/api
    exclude-filter: '(?i)(?:流量|到期|官网|客服|公告|重置|日本|🇯🇵|japan|\bjp\b|\bjp[0-9]+\b)'
`))
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers["data"]
	for _, name := range []string{"🇯🇵日本专线02|BGP|流媒体", "日本高速04", "🇯🇵 Tokyo", "Japan 01", "JAPAN-02", "JP-01", "jp02", "剩余流量：985 GB"} {
		if p.Keep(name) {
			t.Errorf("prohibited node retained: %s", name)
		}
	}
	for _, name := range []string{"🇭🇰香港专线03|BGP|流媒体", "🇸🇬新加坡01", "🇨🇳台湾01", "US-01", "UK-01"} {
		if !p.Keep(name) {
			t.Errorf("allowed node excluded: %s", name)
		}
	}
}
