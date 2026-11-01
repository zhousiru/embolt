package proxy

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
)

const seriesPosterTag = "embolt-series-v1."

var (
	primaryImagePath = regexp.MustCompile(`(?i)^((?:/emby)?/items/)([^/]+)(/images/primary(?:/0)?)$`)
	imagePart        = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// resumeSeriesPoster advertises a series poster only when an episode has no
// primary image. The tag carries the route, so a client can load it after an
// Embolt restart without a per-client or persistent image mapping.
func resumeSeriesPoster(it map[string]any) {
	if str(it["Type"]) != "Episode" {
		return
	}
	tags, _ := it["ImageTags"].(map[string]any)
	if str(tags["Primary"]) != "" {
		return
	}
	id, series, tag := str(it["Id"]), str(it["SeriesId"]), str(it["SeriesPrimaryImageTag"])
	if !imagePart.MatchString(id) || !imagePart.MatchString(series) || !imagePart.MatchString(tag) {
		return
	}
	if tags == nil {
		tags = make(map[string]any)
		it["ImageTags"] = tags
	}
	tags["Primary"] = seriesPosterTag + base64.RawURLEncoding.EncodeToString([]byte(id+":"+series+":"+tag))
	// An episode's still aspect ratio does not describe its series poster.
	delete(it, "PrimaryImageAspectRatio")
}

// resumePosterRequest resolves only tags emitted by the opt-in resume list.
// Keep authentication, sizing and all other query parameters, and let the
// usual image proxy/cache serve the series image under its real cache key.
func (s *Server) resumePosterRequest(r *http.Request) *http.Request {
	if !s.local() || !s.cfg.Load().Personalization.ResumeSeriesPoster ||
		(r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return r
	}
	m := primaryImagePath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return r
	}
	encoded, ok := strings.CutPrefix(query(r.URL, "tag"), seriesPosterTag)
	if !ok || len(encoded) > 520 {
		return r
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	parts := strings.Split(string(raw), ":")
	if err != nil || len(parts) != 3 || !strings.EqualFold(parts[0], m[2]) {
		return r
	}
	for _, part := range parts {
		if !imagePart.MatchString(part) {
			return r
		}
	}
	r = r.Clone(r.Context())
	r.URL.Path = m[1] + parts[1] + m[3]
	r.URL.RawPath = ""
	q := r.URL.Query()
	qdel(q, "tag")
	q.Set("tag", parts[2])
	r.URL.RawQuery = q.Encode()
	return r
}
