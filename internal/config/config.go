// Package config holds Embolt's schema, defaults and validation, and a
// hot-reloading store. Every key has a default; a minimal config is
// upstream.url plus one proxy provider.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen   string   `yaml:"listen"`
	TLS      TLS      `yaml:"tls"`
	Upstream Upstream `yaml:"upstream"`

	// Providers and Proxies use mihomo's own schema, so blocks can be pasted
	// from a mihomo config unchanged.
	Providers map[string]Provider `yaml:"proxy-providers"`
	Proxies   []map[string]any    `yaml:"proxies"`
	DNS       DNS                 `yaml:"dns"`

	Control Control `yaml:"control"`
	Probes  Probes  `yaml:"probes"`
	Pins    Pins    `yaml:"pins"`
	Cache   Cache   `yaml:"cache"`
	Web     Web     `yaml:"web"`
	DataDir string  `yaml:"data_dir"`

	// Profile is where the user's watch state, favorites and preferences
	// live: "upstream" on the server's account, or "local" in
	// data_dir/profile.json, so deployments sharing one account each keep
	// their own.
	Profile string `yaml:"profile"`
}

// LocalProfile reports whether user data stays in this deployment.
func (c *Config) LocalProfile() bool { return c.Profile == "local" }

type TLS struct {
	Listen string `yaml:"listen"`
	Cert   string `yaml:"cert"`
	Key    string `yaml:"key"`
}

type Upstream struct {
	URL      string `yaml:"url"`
	Redirect string `yaml:"redirect"` // follow | pass

	base *url.URL
}

// Base is the parsed upstream URL.
func (u *Upstream) Base() *url.URL { return u.base }

// Provider is a mihomo proxy-provider block. Only http and file are supported.
type Provider struct {
	Type          string `yaml:"type"`
	URL           string `yaml:"url"`
	Path          string `yaml:"path"`
	Interval      int    `yaml:"interval"` // seconds, as in mihomo
	Filter        string `yaml:"filter"`
	ExcludeFilter string `yaml:"exclude-filter"`

	filter, exclude *regexp.Regexp
}

// Keep reports whether a node named name passes the provider's filters.
func (p *Provider) Keep(name string) bool {
	return (p.filter == nil || p.filter.MatchString(name)) &&
		(p.exclude == nil || !p.exclude.MatchString(name))
}

// DNS resolves node servers. It reads the part of mihomo's dns section that
// applies to a client with no inbound or rules, and ignores the rest, so a
// mihomo block pastes unchanged. Encrypted servers keep answers real behind
// a local fake-ip TUN. Without it, the OS's servers are used.
type DNS struct {
	IPv6                  bool     `yaml:"ipv6"`
	DefaultNameserver     []string `yaml:"default-nameserver"`      // IPs; they resolve the others
	ProxyServerNameserver []string `yaml:"proxy-server-nameserver"` // preferred for node servers
	Nameserver            []string `yaml:"nameserver"`              // used if the above is empty
}

func (d DNS) equal(o DNS) bool {
	return d.IPv6 == o.IPv6 && slices.Equal(d.DefaultNameserver, o.DefaultNameserver) &&
		slices.Equal(d.ProxyServerNameserver, o.ProxyServerNameserver) && slices.Equal(d.Nameserver, o.Nameserver)
}

// Control tunes the controller.
type Control struct {
	ReadAhead time.Duration `yaml:"read_ahead"` // media buffered ahead of the player
}

// Probes tune exploration, the only speed test: a session reads a stretch
// of its file through another node, beside its media node, and drops it.
type Probes struct {
	Budget float64 `yaml:"budget"` // share of a session's bitrate spent on tests; 0 turns them off
}

// Pins name a node (by name or ID) that overrides the controller for a role.
type Pins struct {
	Primary string `yaml:"primary"`
	Media   string `yaml:"media"`
}

type Cache struct {
	SizeMB int64 `yaml:"size_mb"` // under data_dir/cache
}

type Web struct {
	Listen    string     `yaml:"listen"`
	BasicAuth *BasicAuth `yaml:"basic_auth"`
}

type BasicAuth struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// Default returns a config with every default filled in.
func Default() *Config {
	return &Config{
		Listen:   ":8096",
		Upstream: Upstream{Redirect: "follow"},
		Control:  Control{ReadAhead: 60 * time.Second},
		Probes:   Probes{Budget: 0.05},
		Cache:    Cache{SizeMB: 2048},
		Web:      Web{Listen: ":9090"},
		DataDir:  "/data",
		Profile:  "upstream",
	}
}

// Load reads and validates the file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes raw YAML over the defaults and validates the result.
func Parse(raw []byte) (*Config, error) {
	c := Default()
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	check := func(ok bool, format string, a ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, a...))
		}
	}
	u := &c.Upstream
	var err error
	if u.base, err = url.Parse(u.URL); err != nil || u.base.Host == "" {
		errs = append(errs, fmt.Errorf("upstream.url: want an absolute URL, got %q", u.URL))
	}
	check(u.Redirect == "follow" || u.Redirect == "pass", "upstream.redirect: want follow or pass, got %q", u.Redirect)
	check(c.Profile == "upstream" || c.Profile == "local", "profile: want upstream or local, got %q", c.Profile)
	check(len(c.Providers)+len(c.Proxies) > 0, "no nodes: set proxy-providers or proxies")
	for name, p := range c.Providers {
		check(p.Type == "http" && p.URL != "" || p.Type == "file" && p.Path != "",
			"proxy-providers.%s: want type http with url, or file with path", name)
		p.filter, err = compileOptional(p.Filter)
		check(err == nil, "proxy-providers.%s.filter: %v", name, err)
		p.exclude, err = compileOptional(p.ExcludeFilter)
		check(err == nil, "proxy-providers.%s.exclude-filter: %v", name, err)
		c.Providers[name] = p
	}
	check(c.Control.ReadAhead > 0, "control.read_ahead: want a positive duration")
	check(c.Probes.Budget >= 0 && c.Probes.Budget < 1, "probes.budget: want [0, 1)")
	check((c.TLS.Listen == "") == (c.TLS.Cert == "") && (c.TLS.Cert == "") == (c.TLS.Key == ""),
		"tls: set listen, cert and key together")
	return errors.Join(errs...)
}

func compileOptional(expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	return regexp.Compile(expr)
}
