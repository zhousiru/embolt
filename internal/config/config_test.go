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
	if c.Control.StallRisk != 0.01 || c.Control.HalfLife != 2*time.Hour || c.Listen != ":8096" {
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
control: {stall_risk: 2}
`))
	for _, want := range []string{"upstream.url", "upstream.redirect", "no nodes", "stall_risk"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %s", err, want)
		}
	}
}
