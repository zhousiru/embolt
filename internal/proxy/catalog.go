package proxy

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/zhousiru/embolt/internal/profile"
)

const catalogCap = 4096

var reDeviceID = regexp.MustCompile(`(?i)DeviceId="([^"]*)"`)

// catalog remembers what PlaybackInfo told the player: each media source's
// bitrate and size, its stream paths, and any separate stream hosts.
type catalog struct {
	mu      sync.Mutex
	sources map[string]source // by MediaSourceId
	items   map[string]source // by item ID, the first source
	streams map[string]bool   // lower-cased stream paths
	hosts   map[string]bool   // stream hosts the proxy may reach
	details map[string]item   // by item ID, from the item's details
	owners  map[string]string // item ID by MediaSourceId
}

type source struct {
	mbps float64
	size int64
}

func newCatalog() *catalog {
	return &catalog{
		sources: map[string]source{},
		items:   map[string]source{},
		streams: map[string]bool{},
		hosts:   map[string]bool{},
		details: map[string]item{},
		owners:  map[string]string{},
	}
}

func (c *catalog) isStream(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[strings.ToLower(path)]
}

// owner is the item a MediaSourceId belongs to, if PlaybackInfo named it.
func (c *catalog) owner(src string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owners[src]
}

func (c *catalog) allowsHost(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hosts[strings.ToLower(host)]
}

// session keys a stream by device + item: one viewer watching one thing.
// Not by PlaySessionId: players open extra ones for side work such as
// scanning the file for seek thumbnails, and those must share the viewer's
// node and exit IP. It also finds the bitrate in Mbps (0 if unknown).
func (c *catalog) session(r *http.Request) (key string, mbps float64) {
	src := query(r.URL, "MediaSourceId")
	item := ""
	if m := reItemID.FindStringSubmatch(r.URL.Path); m != nil {
		item = m[1]
	}
	key = deviceID(r) + "/" + cmp.Or(item, src)
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sources[src]; ok {
		return key, s.mbps
	}
	return key, c.items[item].mbps
}

func deviceID(r *http.Request) string {
	if id := cmp.Or(query(r.URL, "DeviceId"), r.Header.Get("X-Emby-Device-Id")); id != "" {
		return id
	}
	for _, h := range []string{"X-Emby-Authorization", "Authorization"} {
		if m := reDeviceID.FindStringSubmatch(r.Header.Get(h)); m != nil {
			return m[1]
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// playbackInfo records each media source, and rewrites absolute stream URLs
// so the player keeps talking to the proxy. It runs on the response to
// POST /Items/{id}/PlaybackInfo, which the proxy asked for uncompressed.
func (s *Server) playbackInfo(resp *http.Response, item string) error {
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var info map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &info) != nil {
		return nil
	}
	sources, _ := info["MediaSources"].([]any)
	changed := false
	c := s.catalog
	c.mu.Lock()
	if len(c.sources) > catalogCap {
		clear(c.sources)
		clear(c.items)
		clear(c.streams)
		clear(c.owners)
	}
	for i, v := range sources {
		ms, _ := v.(map[string]any)
		if ms == nil {
			continue
		}
		bitrate, _ := ms["Bitrate"].(float64)
		size, _ := ms["Size"].(float64)
		src := source{mbps: bitrate / 1e6, size: int64(size)}
		if id, _ := ms["Id"].(string); id != "" {
			c.sources[id] = src
			c.owners[id] = item
		}
		if t, _ := ms["RunTimeTicks"].(float64); t > 0 {
			s.profile.Learn(itemID(item), profile.Meta{Runtime: int64(t)})
		}
		if i == 0 {
			c.items[item] = src
		}
		for _, field := range []string{"DirectStreamUrl", "TranscodingUrl", "Path"} {
			if u, _ := ms[field].(string); u != "" {
				if rel, ok := s.localize(u); ok {
					ms[field], changed = rel, true
					u = rel
				}
				if field == "DirectStreamUrl" && strings.HasPrefix(u, "/") {
					p, _, _ := strings.Cut(u, "?")
					c.streams[strings.ToLower(p)] = true
				}
			}
		}
	}
	c.mu.Unlock()
	if changed {
		raw, _ = json.Marshal(info)
		setBody(resp, raw)
	}
	return nil
}

// localize maps an absolute URL to a proxy-relative one: the upstream's own
// origin is stripped, and another host is routed through /_embolt/ and
// allowed from then on.
func (s *Server) localize(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	if rel, ok := s.stripOrigin(raw); ok {
		return rel, true
	}
	s.catalog.hosts[strings.ToLower(u.Host)] = true // caller holds catalog.mu
	return remotePrefix + u.Scheme + "/" + u.Host + u.RequestURI(), true
}
