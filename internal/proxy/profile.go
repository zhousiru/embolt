package proxy

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhousiru/embolt/internal/profile"
)

// With profile: local, the user's data lives in this deployment. Writes to
// it are answered here and never reach the server. Playback reports are
// recorded and still sent on, since the server runs playback on them. Every
// item the server sends carries the local user data instead of its own, and
// the lists that select by user data (Continue Watching, Next Up, favorites,
// played) are built from the local profile.

const (
	maxOverlay = 64 << 20 // a larger JSON body passes through as it is
	maxBody    = 1 << 20  // most a player's write may carry
	maxIDs     = 500      // most item IDs one rewritten query names
	nextUpScan = 50       // most series Next Up looks into
	nextUpPar  = 6        // series Next Up fetches at once

	embyTime = "2006-01-02T15:04:05.0000000Z07:00"
)

var (
	reMark    = regexp.MustCompile(`(?i)^((?:/emby)?/users/[^/]+)/(favoriteitems|playeditems|playingitems)/([^/]+)(/progress|/delete)?$`)
	reItemOp  = regexp.MustCompile(`(?i)^(?:/emby)?/users/[^/]+/items/([^/]+)/(rating|rating/delete|userdata|hidefromresume)$`)
	reReport  = regexp.MustCompile(`(?i)^(?:/emby)?/sessions/playing(/progress|/stopped)?$`)
	reResume  = regexp.MustCompile(`(?i)^((?:/emby)?/users/[^/]+/items)/resume$`)
	reNextUp  = regexp.MustCompile(`(?i)^((?:/emby)?)/shows/nextup$`)
	reQuery   = regexp.MustCompile(`(?i)^(?:/emby)?(?:/users/[^/]+)?/items$`)
	rePrefs   = regexp.MustCompile(`(?i)^(?:/emby)?/displaypreferences/([^/]+)$`)
	reUserCfg = regexp.MustCompile(`(?i)^(?:/emby)?/users/([^/]+)/configuration$`)
	reLatest  = regexp.MustCompile(`(?i)^(?:/emby)?/users/([^/]+)/items/latest$`)
)

func (s *Server) local() bool { return s.cfg.Load().LocalProfile() }

// The local profile meets the proxy at three points: serveProfile answers
// what the profile owns; for the rest, askPlain shapes the upstream request
// and overlayAnswer edits the answer, wherever overlays says it applies.

// overlays reports whether the local profile edits the answer to a request
// on rt: control traffic other than a cached image.
func (s *Server) overlays(rt *route) bool {
	return s.local() && rt.lane == laneControl && rt.cacheKey == ""
}

// askPlain asks upstream for an answer the profile can edit: a plain body,
// and websocket frames without compression.
func askPlain(h http.Header) {
	h.Del("Accept-Encoding")
	if isWebsocket(h) {
		h.Del("Sec-Websocket-Extensions")
	}
}

// overlayAnswer puts the local profile into the server's answer: its JSON
// body, or the messages of its websocket.
func (s *Server) overlayAnswer(resp *http.Response) error {
	if conn, ok := resp.Body.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = newWSFilter(conn, s.userDataMessage)
		return nil
	}
	return s.overlayResponse(resp)
}

// serveProfile answers what the local profile owns and records playback
// reports. It reports whether it wrote the response; if not, the request
// goes on to the server.
func (s *Server) serveProfile(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	post, get := r.Method == http.MethodPost, r.Method == http.MethodGet
	write := post || r.Method == http.MethodDelete
	switch {
	case post && reReport.MatchString(p):
		s.report(r, strings.ToLower(reReport.FindStringSubmatch(p)[1]))
	case write && reMark.MatchString(p):
		return s.mark(w, r, reMark.FindStringSubmatch(p))
	case write && reItemOp.MatchString(p):
		m := reItemOp.FindStringSubmatch(p)
		return s.itemOp(w, r, itemID(m[1]), strings.ToLower(m[2]))
	case get && reResume.MatchString(p):
		s.resume(w, r, reResume.FindStringSubmatch(p)[1])
		return true
	case get && reNextUp.MatchString(p):
		s.nextUp(w, r, reNextUp.FindStringSubmatch(p)[1])
		return true
	case get && reQuery.MatchString(p):
		return s.filtered(w, r)
	case rePrefs.MatchString(p):
		key := strings.ToLower(rePrefs.FindStringSubmatch(p)[1] + "/" + query(r.URL, "Client"))
		switch {
		case get:
			if raw, ok := s.profile.Pref(key); ok {
				writeJSON(w, raw)
				return true
			}
		case post:
			if raw, ok := readJSON(w, r); ok {
				s.profile.SetPref(key, raw)
				w.WriteHeader(http.StatusNoContent)
			}
			return true
		}
	case post && reUserCfg.MatchString(p):
		if raw, ok := readJSON(w, r); ok {
			s.profile.SetConfig(itemID(reUserCfg.FindStringSubmatch(p)[1]), raw)
			w.WriteHeader(http.StatusNoContent)
		}
		return true
	}
	return false
}

// itemID is how the profile keys an item.
func itemID(id string) string { return strings.ToLower(id) }

// report records a playback report, which then goes on to the server.
func (s *Server) report(r *http.Request, kind string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), r.Body), r.Body}
	if err != nil {
		return
	}
	var rep map[string]any
	if decodeJSON(raw, &rep) != nil {
		rep = map[string]any{}
	}
	field := func(k string) string { return cmp.Or(str(rep[k]), query(r.URL, k)) }
	id := itemID(cmp.Or(field("ItemId"), s.catalog.owner(field("MediaSourceId"))))
	if id == "" || kind == "/stopped" && rep["Failed"] == true { // the server keeps no state for a failed play
		return
	}
	s.playState(id, kind, ticks(field("PositionTicks")))
}

// playState records a start ("" kind), "/progress" or "/stopped" report.
func (s *Server) playState(id, kind string, pos *int64) {
	switch {
	case kind == "/stopped":
		s.profile.Stopped(id, pos, time.Now())
	case kind == "/progress" && pos != nil:
		s.profile.Progress(id, *pos, time.Now())
	case kind == "":
		s.profile.Started(id, time.Now())
	}
}

// mark handles /Users/{u}/{FavoriteItems,PlayedItems,PlayingItems}/{id}.
func (s *Server) mark(w http.ResponseWriter, r *http.Request, m []string) bool {
	user, kind, id, suffix := m[1], strings.ToLower(m[2]), itemID(m[3]), strings.ToLower(m[4])
	del := r.Method == http.MethodDelete || suffix == "/delete"
	switch {
	case kind == "playingitems": // the older playback reports, also sent on
		if del {
			suffix = "/stopped"
		}
		s.playState(id, suffix, ticks(query(r.URL, "PositionTicks")))
		return false
	case suffix == "/progress":
		return false
	case kind == "favoriteitems":
		replyUserData(w, id, s.profile.Update(id, func(e *profile.Entry) { e.Favorite = !del }))
	default:
		replyUserData(w, id, s.markTree(r, user, id, !del, datePlayed(query(r.URL, "DatePlayed"))))
	}
	return true
}

// datePlayed reads Emby's yyyyMMddHHmmss, or RFC 3339; else it is zero.
func datePlayed(v string) time.Time {
	for _, layout := range []string{"20060102150405", time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// markTree marks an item played or unplayed, and every item under it, as
// the server does for a series or season. user is the /Users/{u} prefix.
func (s *Server) markTree(r *http.Request, user, id string, played bool, at time.Time) profile.Entry {
	ids := []string{id}
	if e := s.profile.Get(id); e.Type == "" || e.Folder {
		q := authQuery(r)
		q.Set("ParentId", id)
		q.Set("Recursive", "true")
		q.Set("IsFolder", "false")
		q.Set("EnableImages", "false")
		q.Set("EnableUserData", "false")
		if v, err := s.get(r, user+"/Items", q); err == nil {
			for _, c := range items(v) {
				if cid, _ := c["Id"].(string); cid != "" {
					ids = append(ids, itemID(cid))
				}
			}
		} else {
			slog.Debug("could not list an item's children", "item", id, "err", err)
		}
	}
	for _, c := range slices.Backward(ids) { // children first, so a folder's own mark is last
		if played {
			s.profile.MarkPlayed(c, at)
		} else {
			s.profile.MarkUnplayed(c)
		}
	}
	return s.profile.Get(id)
}

// itemOp handles /Users/{u}/Items/{id}/{Rating,UserData,HideFromResume}.
func (s *Server) itemOp(w http.ResponseWriter, r *http.Request, id, op string) bool {
	var f func(*profile.Entry)
	switch {
	case op == "rating" && r.Method == http.MethodPost:
		likes := strings.EqualFold(query(r.URL, "Likes"), "true")
		f = func(e *profile.Entry) { e.Likes = &likes }
	case op == "rating", op == "rating/delete":
		f = func(e *profile.Entry) { e.Likes = nil }
	case op == "hidefromresume":
		hide := !strings.EqualFold(query(r.URL, "Hide"), "false")
		f = func(e *profile.Entry) {
			if hide {
				e.Position = 0
			}
		}
	default: // userdata: a partial UserItemDataDto
		raw, ok := readJSON(w, r)
		if !ok {
			return true
		}
		var d struct {
			PlaybackPositionTicks *int64
			PlayCount             *int
			IsFavorite, Played    *bool
			Likes                 *bool
			LastPlayedDate        *time.Time
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		f = func(e *profile.Entry) {
			setIf(&e.Position, d.PlaybackPositionTicks)
			setIf(&e.PlayCount, d.PlayCount)
			setIf(&e.Favorite, d.IsFavorite)
			setIf(&e.Played, d.Played)
			setIf(&e.LastPlayed, d.LastPlayedDate)
			e.Likes = d.Likes
		}
	}
	replyUserData(w, id, s.profile.Update(id, f))
	return true
}

func setIf[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// resume answers /Users/{u}/Items/Resume from the local resume points.
func (s *Server) resume(w http.ResponseWriter, r *http.Request, itemsPath string) {
	q := r.URL.Query()
	start, limit := page(q)
	q.Set("Recursive", "true")
	s.listIDs(w, r, itemsPath, q, s.profile.IDs(profile.Resumable, maxIDs), true, start, limit)
}

// filtered answers an item query that filters on user data from the local
// profile: the filters become Ids and ExcludeItemIds. It reports whether
// the query had such a filter.
func (s *Server) filtered(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	var keep, drop []func(profile.Entry) bool
	var rest []string
	for f := range strings.SplitSeq(qget(q, "Filters"), ",") {
		switch strings.ToLower(strings.TrimSpace(f)) {
		case "":
		case "isfavorite":
			keep = append(keep, favorite)
		case "isfavoriteorlikes":
			keep = append(keep, func(e profile.Entry) bool { return e.Favorite || liked(e) })
		case "isliked":
			keep = append(keep, liked)
		case "isdisliked":
			keep = append(keep, func(e profile.Entry) bool { return e.Likes != nil && !*e.Likes })
		case "isplayed":
			keep = append(keep, played)
		case "isunplayed":
			drop = append(drop, played)
		case "isresumable":
			keep = append(keep, profile.Resumable)
		default:
			rest = append(rest, f)
		}
	}
	for param, is := range map[string]func(profile.Entry) bool{"IsFavorite": favorite, "IsPlayed": played} {
		switch strings.ToLower(qget(q, param)) {
		case "true":
			keep = append(keep, is)
		case "false":
			drop = append(drop, is)
		}
		qdel(q, param)
	}
	if keep == nil && drop == nil {
		return false
	}
	qdel(q, "Filters")
	if rest != nil {
		q.Set("Filters", strings.Join(rest, ","))
	}
	if ex := s.profile.IDs(anyOf(drop), maxIDs); len(ex) > 0 {
		if old := qget(q, "ExcludeItemIds"); old != "" {
			ex = append(ex, old)
		}
		qdel(q, "ExcludeItemIds")
		q.Set("ExcludeItemIds", strings.Join(ex, ","))
	}
	if keep == nil {
		s.reply(w, r, r.URL.Path, q)
		return true
	}
	ids := s.profile.IDs(allOf(keep), maxIDs)
	if old := qget(q, "Ids"); old != "" {
		named := strings.Split(strings.ToLower(old), ",")
		ids = slices.DeleteFunc(ids, func(id string) bool { return !slices.Contains(named, id) })
	}
	if !strings.Contains(strings.ToLower(qget(q, "SortBy")), "dateplayed") {
		if len(ids) == 0 {
			writeJSON(w, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			return true
		}
		qdel(q, "Ids")
		q.Set("Ids", strings.Join(ids, ","))
		s.reply(w, r, r.URL.Path, q)
		return true
	}
	// Sorted by date played: the server's dates are not ours, so sort and
	// page here.
	start, limit := page(q)
	desc := strings.Contains(strings.ToLower(qget(q, "SortOrder")), "desc")
	if !desc {
		slices.Reverse(ids)
	}
	s.listIDs(w, r, r.URL.Path, q, ids, false, start, limit)
	return true
}

func favorite(e profile.Entry) bool { return e.Favorite }
func played(e profile.Entry) bool   { return e.Played }
func liked(e profile.Entry) bool    { return e.Likes != nil && *e.Likes }

func allOf(fs []func(profile.Entry) bool) func(profile.Entry) bool {
	return func(e profile.Entry) bool {
		for _, f := range fs {
			if !f(e) {
				return false
			}
		}
		return true
	}
}

func anyOf(fs []func(profile.Entry) bool) func(profile.Entry) bool {
	return func(e profile.Entry) bool {
		return slices.ContainsFunc(fs, func(f func(profile.Entry) bool) bool { return f(e) })
	}
}

// listIDs asks the server for the items ids names, and answers with them in
// that order, paged here. A resume list keeps only items with a resume point.
func (s *Server) listIDs(w http.ResponseWriter, r *http.Request, path string, q url.Values, ids []string, resume bool, start, limit int) {
	if len(ids) == 0 {
		writeJSON(w, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		return
	}
	qdel(q, "Ids")
	q.Set("Ids", strings.Join(ids, ","))
	v, err := s.get(r, path, q)
	if err != nil {
		replyError(w, err)
		return
	}
	rank := make(map[string]int, len(ids))
	for i, id := range ids {
		rank[id] = i
	}
	list := items(v)
	list = slices.DeleteFunc(list, func(it map[string]any) bool {
		_, ok := rank[itemID(str(it["Id"]))]
		return !ok || resume && !profile.Resumable(s.profile.Get(itemID(str(it["Id"]))))
	})
	slices.SortStableFunc(list, func(a, b map[string]any) int {
		return cmp.Compare(rank[itemID(str(a["Id"]))], rank[itemID(str(b["Id"]))])
	})
	total := len(list)
	list = list[min(start, total):]
	if limit > 0 {
		list = list[:min(limit, len(list))]
	}
	writeJSON(w, map[string]any{"Items": list, "TotalRecordCount": total})
}

// nextUp answers /Shows/NextUp from the local played marks, for each series
// watched here, most recent first.
func (s *Server) nextUp(w http.ResponseWriter, r *http.Request, prefix string) {
	q := r.URL.Query()
	start, limit := page(q)
	series := s.profile.Series()
	if id := itemID(qget(q, "SeriesId")); id != "" {
		series = slices.DeleteFunc(series, func(s string) bool { return s != id })
	}
	series = series[:min(len(series), nextUpScan)]
	resumable := !strings.EqualFold(qget(q, "EnableResumable"), "false")
	cutoff := datePlayed(qget(q, "NextUpDateCutoff"))
	fields := strings.Trim(qget(q, "Fields")+",SpecialEpisodeNumbers", ",") // where specials aired
	qdel(q, "SeriesId", "ParentId", "NextUpDateCutoff", "EnableResumable", "EnableRewatching", "DisableFirstEpisode", "Legacy", "Fields")
	q.Set("Fields", fields)

	var found []map[string]any
	var firstErr error
	for i := 0; i < len(series) && (limit == 0 || len(found) < start+limit); i += nextUpPar {
		batch := series[i:min(i+nextUpPar, len(series))]
		next := make([]map[string]any, len(batch))
		errs := make([]error, len(batch))
		var wg sync.WaitGroup
		for j, id := range batch {
			wg.Go(func() {
				v, err := s.get(r, prefix+"/Shows/"+url.PathEscape(id)+"/Episodes", q)
				if err != nil {
					errs[j] = err
					return
				}
				next[j] = s.nextEpisode(items(v), resumable, cutoff)
			})
		}
		wg.Wait()
		for j := range batch {
			firstErr = cmp.Or(firstErr, errs[j])
			if next[j] != nil {
				found = append(found, next[j])
			}
		}
	}
	if len(found) == 0 && firstErr != nil {
		replyError(w, firstErr)
		return
	}
	total := len(found)
	found = found[min(start, total):]
	if limit > 0 {
		found = found[:min(limit, len(found))]
	}
	writeJSON(w, map[string]any{"Items": found, "TotalRecordCount": total})
}

// nextEpisode picks Next Up from a series' episodes, as Emby does: the
// first unplayed episode, in the order they aired, after the most recently
// played one. Of episodes played at once, as when a season is marked, the
// last in order counts. A series last played before cutoff has none, and
// so does one whose next episode is in progress, unless resumable.
func (s *Server) nextEpisode(eps []map[string]any, resumable bool, cutoff time.Time) map[string]any {
	eps = airedOrder(eps)
	last, lastAt := -1, time.Time{}
	for i, ep := range eps {
		if e := s.profile.Get(itemID(str(ep["Id"]))); e.Played && !e.LastPlayed.Before(lastAt) {
			last, lastAt = i, e.LastPlayed
		}
	}
	if last < 0 || lastAt.Before(cutoff) {
		return nil
	}
	for _, ep := range eps[last+1:] {
		switch e := s.profile.Get(itemID(str(ep["Id"]))); {
		case e.Played:
		case e.Position > 0 && !resumable:
			return nil
		default:
			return ep
		}
	}
	return nil
}

// airedOrder sorts episodes as they aired. A special goes where its
// AirsBefore or AirsAfter numbers place it; one without them is left out.
func airedOrder(eps []map[string]any) []map[string]any {
	type placed struct {
		ep                   map[string]any
		season, episode, tie int
	}
	var out []placed
	for _, ep := range eps {
		season, okS := num(ep["ParentIndexNumber"])
		episode, okE := num(ep["IndexNumber"])
		switch {
		case !okS || season != 0:
			if !okS || !okE { // unnumbered: keep the server's order, without specials
				return slices.DeleteFunc(slices.Clone(eps), func(ep map[string]any) bool { return str(ep["ParentIndexNumber"]) == "0" })
			}
			out = append(out, placed{ep, season, episode, 0})
		default:
			if before, ok := num(ep["AirsBeforeSeasonNumber"]); ok {
				at, ok := num(ep["AirsBeforeEpisodeNumber"])
				if !ok {
					at = math.MinInt
				}
				out = append(out, placed{ep, before, at, -1})
			} else if after, ok := num(ep["AirsAfterSeasonNumber"]); ok {
				out = append(out, placed{ep, after, math.MaxInt, 0})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b placed) int {
		return cmp.Or(cmp.Compare(a.season, b.season), cmp.Compare(a.episode, b.episode), cmp.Compare(a.tie, b.tie))
	})
	sorted := make([]map[string]any, len(out))
	for i, p := range out {
		sorted[i] = p.ep
	}
	return sorted
}

func num(v any) (int, bool) {
	n, err := strconv.Atoi(str(v))
	return n, err == nil
}

// overlayResponse puts the local profile into a JSON answer from the server.
func (s *Server) overlayResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" ||
		!strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil
	}
	raw, ok, err := peek(resp, maxOverlay)
	if !ok || !bytes.Contains(raw, []byte(`"UserData"`)) && !bytes.Contains(raw, []byte(`"Configuration"`)) {
		return err
	}
	var v any
	if decodeJSON(raw, &v) != nil {
		return nil
	}
	changed := s.overlay(v)
	if list, ok := v.([]any); ok && reLatest.MatchString(resp.Request.URL.Path) && s.hidePlayedInLatest(resp.Request) {
		v = slices.DeleteFunc(list, func(it any) bool {
			m, _ := it.(map[string]any)
			ud, _ := m["UserData"].(map[string]any)
			return ud["Played"] == true
		})
		changed = true
	}
	if changed {
		raw, _ = json.Marshal(v)
		setBody(resp, raw)
	}
	return nil
}

// hidePlayedInLatest reports whether Latest should leave out played items:
// asked for, or the user's setting, which Emby defaults to true.
func (s *Server) hidePlayedInLatest(r *http.Request) bool {
	if v := query(r.URL, "IsPlayed"); v != "" {
		return strings.EqualFold(v, "false")
	}
	var c struct{ HidePlayedInLatest *bool }
	json.Unmarshal(s.profile.Config(itemID(reLatest.FindStringSubmatch(r.URL.Path)[1])), &c)
	return c.HidePlayedInLatest == nil || *c.HidePlayedInLatest
}

// overlay puts the local user data into every item in v, learns what each
// item is, and puts a user's local configuration into it. It reports whether
// it changed v.
func (s *Server) overlay(v any) bool {
	changed := false
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			changed = s.overlay(x) || changed
		}
	case map[string]any:
		for _, x := range v {
			changed = s.overlay(x) || changed
		}
		id, _ := v["Id"].(string)
		if id == "" {
			break
		}
		id = itemID(id)
		if typ, ok := v["Type"].(string); ok {
			m := profile.Meta{Type: typ, Series: itemID(str(v["SeriesId"])), Season: itemID(str(v["SeasonId"]))}
			m.Folder, _ = v["IsFolder"].(bool)
			if t := ticks(str(v["RunTimeTicks"])); t != nil {
				m.Runtime = *t
			}
			s.profile.Learn(id, m)
		}
		if old, ok := v["UserData"].(map[string]any); ok {
			v["UserData"] = userData(id, s.profile.Get(id), old)
			changed = true
		}
		if cfg := s.profile.Config(id); cfg != nil && v["Policy"] != nil && v["Configuration"] != nil {
			v["Configuration"] = cfg
			changed = true
		}
	}
	return changed
}

// userData is an entry as Emby's UserItemDataDto, keeping the server's own
// keys for the item from old.
func userData(id string, e profile.Entry, old map[string]any) map[string]any {
	d := map[string]any{
		"PlaybackPositionTicks": e.Position,
		"PlayCount":             e.PlayCount,
		"IsFavorite":            e.Favorite,
		"Played":                e.Played,
		"Key":                   id,
		"ItemId":                id,
	}
	for _, k := range []string{"Key", "ItemId", "ServerId"} {
		if v, ok := old[k]; ok {
			d[k] = v
		}
	}
	if e.Likes != nil {
		d["Likes"] = *e.Likes
	}
	if !e.LastPlayed.IsZero() {
		d["LastPlayedDate"] = e.LastPlayed.UTC().Format(embyTime)
	}
	if e.Position > 0 && e.Runtime > 0 {
		d["PlayedPercentage"] = 100 * float64(e.Position) / float64(e.Runtime)
	}
	if e.Folder && e.Played {
		d["UnplayedItemCount"] = 0
	}
	return d
}

func replyUserData(w http.ResponseWriter, id string, e profile.Entry) {
	writeJSON(w, userData(id, e, nil))
}

// get asks the server for path?q on the primary node, as the player that
// sent r, and returns its JSON answer with the local profile in it.
func (s *Server) get(r *http.Request, path string, q url.Values) (any, error) {
	u := s.base.JoinPath(path)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	stripHopByHop(req.Header)
	for _, k := range []string{"Accept-Encoding", "Content-Length", "Content-Type", "Range",
		"If-Range", "If-None-Match", "If-Modified-Since"} {
		req.Header.Del(k)
	}
	s.fixHeaders(req.Header, s.base)
	n, err := s.ctrl.Primary()
	if err != nil {
		return nil, err
	}
	resp, err := s.send(n, req, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOverlay))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{resp.StatusCode, resp.Header.Get("Content-Type"), raw}
	}
	var v any
	if err := decodeJSON(raw, &v); err != nil {
		return nil, err
	}
	s.overlay(v)
	return v, nil
}

// reply answers with the server's answer to path?q.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, path string, q url.Values) {
	v, err := s.get(r, path, q)
	if err != nil {
		replyError(w, err)
		return
	}
	writeJSON(w, v)
}

// statusError is an answer from the server other than 200, relayed as is.
type statusError struct {
	code  int
	ctype string
	body  []byte
}

func (e *statusError) Error() string { return "upstream " + strconv.Itoa(e.code) }

func replyError(w http.ResponseWriter, err error) {
	var se *statusError
	if errors.As(err, &se) {
		setOrDel(w.Header(), "Content-Type", se.ctype)
		w.WriteHeader(se.code)
		w.Write(se.body)
		return
	}
	http.Error(w, "embolt: "+err.Error(), http.StatusBadGateway)
}

func writeJSON(w http.ResponseWriter, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(raw)
}

// readJSON reads a player's JSON body, or answers 400.
func readJSON(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err == nil && !json.Valid(raw) {
		err = errors.New("want a JSON body")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return raw, true
}

// decodeJSON keeps numbers exact, so a body re-encodes as it came.
func decodeJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(v)
}

// items is the Items of a query result, or the elements of a bare list.
func items(v any) []map[string]any {
	list, ok := v.([]any)
	if m, isMap := v.(map[string]any); isMap {
		list, ok = m["Items"].([]any)
	}
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// str is a JSON string or number as a string.
func str(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}

func ticks(s string) *int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return &n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		n := int64(f)
		return &n
	}
	return nil
}

// page takes StartIndex and Limit out of q; a limit of 0 is none.
func page(q url.Values) (start, limit int) {
	start, _ = strconv.Atoi(qget(q, "StartIndex"))
	limit, _ = strconv.Atoi(qget(q, "Limit"))
	qdel(q, "StartIndex", "Limit")
	return max(start, 0), max(limit, 0)
}

func qdel(q url.Values, keys ...string) {
	for k := range q {
		if slices.ContainsFunc(keys, func(key string) bool { return strings.EqualFold(k, key) }) {
			delete(q, k)
		}
	}
}

// authQuery is the credentials and client names a player put in its query.
func authQuery(r *http.Request) url.Values {
	q := url.Values{}
	for k, v := range r.URL.Query() {
		if lk := strings.ToLower(k); lk == "api_key" || strings.HasPrefix(lk, "x-emby-") {
			q[k] = v
		}
	}
	return q
}

// peek reads a response body of at most limit bytes and puts it back. A
// larger one is put back unread past the limit, and ok is false.
func peek(resp *http.Response, limit int64) (raw []byte, ok bool, err error) {
	raw, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		resp.Body.Close()
		return nil, false, err
	}
	if int64(len(raw)) > limit {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}
		return nil, false, nil
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return raw, true, nil
}
