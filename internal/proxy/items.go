package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/zhousiru/embolt/internal/cache"
	"github.com/zhousiru/embolt/internal/view"
)

const (
	maxDetails  = 4 << 20 // a larger details body passes through unread
	thumbHeight = "360"
)

// item is what the pane shows for a session, and where its image lives:
// the item's own Primary image, else its series'.
type item struct {
	view.Item
	imageID, imageTag string
}

// itemDetails records the name, episode and image of an item whose details
// a player fetched (GET /Users/{u}/Items/{id}), before it plays. The body
// reaches the player unchanged.
func (s *Server) itemDetails(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	raw, ok, err := peek(resp, maxDetails)
	if !ok {
		return err
	}

	var d struct {
		ID                    string `json:"Id"`
		Type                  string
		Name                  string
		SeriesName            string
		SeriesID              string `json:"SeriesId"`
		SeriesPrimaryImageTag string
		ParentIndexNumber     *int
		IndexNumber           *int
		ProductionYear        int
		ImageTags             map[string]string
	}
	if json.Unmarshal(raw, &d) != nil || d.ID == "" || d.Name == "" {
		return nil
	}
	it := item{Item: view.Item{ID: d.ID, Type: d.Type, Name: d.Name, SeriesName: d.SeriesName, Year: d.ProductionYear}}
	if d.Type == "Episode" {
		it.Season, it.Episode = d.ParentIndexNumber, d.IndexNumber
	}
	switch {
	case d.ImageTags["Primary"] != "":
		it.imageID, it.imageTag = d.ID, d.ImageTags["Primary"]
	case d.SeriesID != "" && d.SeriesPrimaryImageTag != "":
		it.imageID, it.imageTag = d.SeriesID, d.SeriesPrimaryImageTag
	}
	it.Image = it.imageID != ""

	c := s.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.details) > catalogCap {
		clear(c.details)
	}
	c.details[d.ID] = it
	return nil
}

// lookup finds an item by its ID or one of its MediaSourceIds.
func (c *catalog) lookup(id string) (item, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if it, ok := c.details[id]; ok {
		return it, true
	}
	it, ok := c.details[c.owners[id]]
	return it, ok
}

// Item describes what a session plays, if a player fetched the item's
// details through Embolt. A session key ends in the item's ID.
func (s *Server) Item(sessionKey string) *view.Item {
	id := sessionKey[strings.LastIndexByte(sessionKey, '/')+1:]
	if it, ok := s.catalog.lookup(id); ok {
		return &it.Item
	}
	return nil
}

// ServeImage serves the thumbnail of an item the catalog knows, through the
// image cache and the primary node, so the pane never reaches Emby itself.
// The request is built fresh: none of the pane's headers go upstream.
func (s *Server) ServeImage(w http.ResponseWriter, r *http.Request, id string) {
	it, ok := s.catalog.lookup(id)
	if !ok || it.imageID == "" {
		http.NotFound(w, r)
		return
	}
	q := url.Values{"tag": {it.imageTag}, "maxHeight": {thumbHeight}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		"/emby/Items/"+url.PathEscape(it.imageID)+"/Images/Primary?"+q.Encode(), nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := s.ctrl.Primary()
	if err != nil {
		http.Error(w, "embolt: "+err.Error(), http.StatusBadGateway)
		return
	}
	rt := &route{lane: laneControl, target: s.base, node: n, cacheKey: cache.Key(req)}
	if s.cache.Serve(w, req, rt.cacheKey) {
		return
	}
	s.rp.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), routeKey{}, rt)))
}
