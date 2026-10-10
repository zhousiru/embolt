package proxy

import (
	"cmp"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhousiru/embolt/internal/profile"
)

// Emby 4.9.5 uses Resume for Continue Watching (IncludeNextUp defaults to
// true), NextUp with LegacyNextUp for the old home row, and NextUp with a
// SeriesId for a playback queue. See testdata/emby-4.9.5-nextup.json.
func (s *Server) resume(w http.ResponseWriter, r *http.Request, path string) {
	q := r.URL.Query()
	video := containsValue(qget(q, "MediaTypes"), "Video") || containsValue(qget(q, "IncludeItemTypes"), "Episode")
	includeNext := video && !strings.EqualFold(qget(q, "IncludeNextUp"), "false")
	var candidates []profileCandidate
	queue := false
	scopedSeries := false
	if parent := itemID(qget(q, "ParentId")); parent != "" {
		typ := s.profile.Get(parent).Type
		if typ == "" {
			v, err := s.get(r, path+"/"+url.PathEscape(parent), authQuery(r))
			if err != nil {
				replyError(w, err)
				return
			}
			if m, ok := v.(map[string]any); ok {
				typ = str(m["Type"])
			}
		}
		if typ == "Series" {
			q.Set("SeriesId", parent)
			scopedSeries = true
			queue = includeNext
		}
	}
	if includeNext {
		var err error
		candidates, err = s.continuations(r, q, continuationMode{queue: queue, resumeOnly: containsValue(qget(q, "ExcludeItemTypes"), "Episode")})
		if err != nil {
			replyError(w, err)
			return
		}
	}
	if !queue {
		for _, id := range s.profile.IDs(profile.Resumable, math.MaxInt) {
			e := s.profile.Get(id)
			if !scopedSeries && s.profile.Hidden(id) || includeNext && e.Series != "" {
				continue
			}
			candidates = append(candidates, profileCandidate{id, e.LastPlayed})
		}
		slices.SortStableFunc(candidates, func(a, b profileCandidate) int { return b.at.Compare(a.at) })
	}
	if qget(q, "MediaTypes") == "" && qget(q, "IncludeItemTypes") == "" {
		candidates = nil
	}
	s.replyProfileList(w, r, path, q, candidates, queue, true)
}

func (s *Server) nextUp(w http.ResponseWriter, r *http.Request, prefix string) {
	q := r.URL.Query()
	queue := qget(q, "SeriesId") != ""
	var candidates []profileCandidate
	if queue || strings.EqualFold(qget(q, "LegacyNextUp"), "true") {
		var err error
		candidates, err = s.continuations(r, q, continuationMode{queue: queue, legacy: !queue})
		if err != nil {
			replyError(w, err)
			return
		}
	}
	path := prefix + "/Items"
	if user := qget(q, "UserId"); user != "" {
		path = prefix + "/Users/" + url.PathEscape(user) + "/Items"
	}
	s.replyProfileList(w, r, path, q, candidates, queue, false)
}

type profileCandidate struct {
	id string
	at time.Time
}

// continuations reads the server's episode metadata and ordering, but selects
// the start entirely from local user data. It never calls upstream NextUp.
func (s *Server) continuations(r *http.Request, q url.Values, mode continuationMode) ([]profileCandidate, error) {
	series := s.profile.Series()
	if !mode.queue {
		series = slices.DeleteFunc(series, s.profile.Hidden)
	}
	if id := itemID(qget(q, "SeriesId")); mode.queue {
		series = []string{id}
	}
	out := make([]profileCandidate, 0)
	for i := 0; i < len(series); i += nextUpPar {
		batch := series[i:min(i+nextUpPar, len(series))]
		results := make([][]profileCandidate, len(batch))
		errs := make([]error, len(batch))
		var wg sync.WaitGroup
		for j, id := range batch {
			wg.Go(func() {
				query := authQuery(r)
				for _, k := range []string{"UserId", "Fields"} {
					if v := qget(q, k); v != "" {
						query.Set(k, v)
					}
				}
				if query.Get("UserId") == "" {
					if m := reResume.FindStringSubmatch(r.URL.Path); m != nil {
						parts := strings.Split(m[1], "/")
						query.Set("UserId", parts[len(parts)-2])
					}
				}
				query.Set("Fields", strings.Trim(query.Get("Fields")+",SpecialEpisodeNumbers", ","))
				query.Set("EnableUserData", "false")
				query.Set("EnableImages", "false")
				prefix := ""
				if strings.HasPrefix(strings.ToLower(r.URL.Path), "/emby/") {
					prefix = "/emby"
				}
				v, err := s.get(r, prefix+"/Shows/"+url.PathEscape(id)+"/Episodes", query)
				if err != nil {
					errs[j] = err
					return
				}
				results[j] = s.seriesContinuation(items(v), mode)
			})
		}
		wg.Wait()
		for j := range batch {
			if errs[j] != nil {
				return nil, errs[j]
			}
			out = append(out, results[j]...)
		}
	}
	if !mode.queue {
		slices.SortStableFunc(out, func(a, b profileCandidate) int { return b.at.Compare(a.at) })
	}
	return out, nil
}

// episodeOrder uses Emby's own special-episode sort coordinates. A special
// without a positive sort season is not eligible for Continue Watching.
func episodeOrder(ep map[string]any) (int, int) {
	season, _ := strconv.Atoi(str(ep["ParentIndexNumber"]))
	number, _ := strconv.Atoi(str(ep["IndexNumber"]))
	if v, ok := ep["SortParentIndexNumber"]; ok {
		season, _ = strconv.Atoi(str(v))
	}
	if v, ok := ep["SortIndexNumber"]; ok {
		number, _ = strconv.Atoi(str(v))
	}
	return season, number
}

type continuationMode struct {
	queue      bool
	legacy     bool
	resumeOnly bool
}

func (s *Server) seriesContinuation(eps []map[string]any, mode continuationMode) []profileCandidate {
	// Excluding Episode disables next-up expansion in Resume. The official
	// server falls back to in-progress episodes in library order, one per
	// series on the home row, all of them when scoped to that series.
	if mode.resumeOnly {
		out := make([]profileCandidate, 0)
		for _, ep := range eps {
			id := itemID(str(ep["Id"]))
			e := s.profile.Get(id)
			if e.Position > 0 {
				out = append(out, profileCandidate{id, e.LastPlayed})
				if !mode.queue {
					break
				}
			}
		}
		return out
	}
	last := -1
	var at time.Time
	for i, ep := range eps {
		season, _ := episodeOrder(ep)
		e := s.profile.Get(itemID(str(ep["Id"])))
		if season > 0 && (e.Played || e.Position > 0) && e.LastPlayed.After(at) {
			last, at = i, e.LastPlayed
		}
	}
	if last < 0 {
		return nil
	}
	anchor := eps[last]
	e := s.profile.Get(itemID(str(anchor["Id"])))
	if !mode.queue && e.Position > 0 && (!mode.legacy || !e.Played) {
		return []profileCandidate{{itemID(str(anchor["Id"])), at}}
	}
	season, number := episodeOrder(anchor)
	inclusive := e.Position > 0 && (!mode.legacy || !e.Played)
	out := make([]profileCandidate, 0)
	for _, ep := range eps {
		sn, en := episodeOrder(ep)
		order := cmp.Or(cmp.Compare(sn, season), cmp.Compare(en, number))
		if sn <= 0 || order < 0 || order == 0 && !inclusive {
			continue
		}
		id := itemID(str(ep["Id"]))
		state := s.profile.Get(id)
		if len(out) == 0 && state.Played && (mode.legacy || state.Position == 0) {
			continue
		}
		out = append(out, profileCandidate{id, at})
		if !mode.queue {
			break
		}
	}
	return out
}

// replyProfileList fetches the selected DTOs with the caller's library/type
// filters and fields. Pagination follows selection, not the metadata fetch.
func (s *Server) replyProfileList(w http.ResponseWriter, r *http.Request, path string, q url.Values, candidates []profileCandidate, queue, resume bool) {
	start, limit := page(q)
	explicitZero := qget(r.URL.Query(), "Limit") == "0"
	count := !strings.EqualFold(qget(q, "EnableTotalRecordCount"), "false")
	qdel(q, "SeriesId", "IncludeNextUp", "LegacyNextUp", "EnableTotalRecordCount", "SortBy", "SortOrder")
	// These endpoints ignore generic Items user-data filters in Emby 4.9.5.
	// Passing them to Items here would filter on the shared account instead.
	qdel(q, "Filters", "IsPlayed", "IsFavorite", "ExcludeItemIds", "ExcludeItemTypes")
	if !resume {
		qdel(q, "MediaTypes", "IncludeItemTypes", "SearchTerm")
		if queue {
			qdel(q, "ParentId")
		}
	}
	if qget(q, "Recursive") == "" {
		q.Set("Recursive", "true")
	}
	// Resume scoped to a series omits completed episodes, unlike NextUp's
	// playback queue; an explicitly resumed, played episode remains eligible.
	if queue && resume {
		candidates = slices.DeleteFunc(candidates, func(c profileCandidate) bool { e := s.profile.Get(c.id); return e.Played && e.Position == 0 })
	}
	byID := make(map[string]map[string]any)
	for i := 0; i < len(candidates); i += maxIDs {
		ids := make([]string, 0, maxIDs)
		for _, c := range candidates[i:min(i+maxIDs, len(candidates))] {
			ids = append(ids, c.id)
		}
		qdel(q, "Ids")
		q.Set("Ids", strings.Join(ids, ","))
		v, err := s.get(r, path, q)
		if err != nil {
			replyError(w, err)
			return
		}
		for _, it := range items(v) {
			byID[itemID(str(it["Id"]))] = it
		}
	}
	list := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		if it := byID[c.id]; it != nil {
			list = append(list, it)
		}
	}
	total := len(list)
	list = list[min(start, len(list)):]
	// The home rows report zero when the requested offset exhausts them.
	if !queue && len(list) == 0 {
		total = 0
	}
	if limit > 0 {
		list = list[:min(limit, len(list))]
	}
	if explicitZero {
		list = list[:0]
	}
	if !count {
		total = 0
	}
	writeJSON(w, map[string]any{"Items": list, "TotalRecordCount": total})
}

func containsValue(value, want string) bool {
	return slices.ContainsFunc(strings.Split(value, ","), func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), want) })
}
