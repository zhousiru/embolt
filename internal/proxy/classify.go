package proxy

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type lane int

const (
	laneControl   lane = iota // API, images, websocket: the primary node
	laneStatic                // ranged file reads: read-ahead and failover
	laneHLS                   // playlists and segments: media node, one retry
	laneTranscode             // progressive transcode: media node, as-is
)

func (l lane) String() string { return [...]string{"control", "static", "hls", "transcode"}[l] }

var (
	reStream    = regexp.MustCompile(`(?i)^(?:/emby)?/(videos|audio)/[^/]+/(stream|original)(\.\w+)?$`)
	reDownload  = regexp.MustCompile(`(?i)^(?:/emby)?/items/[^/]+/download$`)
	reHLS       = regexp.MustCompile(`(?i)(\.m3u8$|/hls1?/)`)
	reItemID    = regexp.MustCompile(`(?i)/(?:videos|items|audio)/([^/]+)/`)
	reInfo      = regexp.MustCompile(`(?i)/items/([^/]+)/playbackinfo$`)
	reDetails   = regexp.MustCompile(`(?i)^(?:/emby)?/users/[^/]+/items/([^/]+)$`)
	reCacheable = regexp.MustCompile(`(?i)/items/[^/]+/images/|/videos/[^/]+/[^/]+/subtitles/`)
)

// classify sorts a request into a lane; the first match wins.
func (s *Server) classify(r *http.Request) lane {
	if r.Method != http.MethodGet {
		return laneControl
	}
	p := r.URL.Path
	switch m := reStream.FindStringSubmatch(p); {
	case s.catalog.isStream(p):
		return laneStatic
	case reHLS.MatchString(p):
		return laneHLS
	case reDownload.MatchString(p):
		return laneStatic
	case m == nil:
		return laneControl
	case strings.EqualFold(m[1], "audio"), strings.EqualFold(m[2], "original"),
		strings.EqualFold(query(r.URL, "Static"), "true"):
		return laneStatic
	default:
		return laneTranscode
	}
}

// query reads a parameter of u case-insensitively, as Emby does.
func query(u *url.URL, key string) string { return qget(u.Query(), key) }

// qget reads a parameter case-insensitively, as Emby does.
func qget(q url.Values, key string) string {
	for k, v := range q {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func cacheable(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Range") == "" && reCacheable.MatchString(r.URL.Path)
}
