package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/profile"
)

type referenceItem struct {
	ID     string `json:"id"`
	Pos    int64  `json:"pos"`
	Played bool   `json:"played"`
}

type referenceList struct {
	Items []referenceItem `json:"items"`
	Total int             `json:"total"`
}

// The expected responses were recorded from the official Emby binary, not
// from our selection functions. The fake upstream supplies metadata only;
// its user data deliberately contradicts the local viewer's profile.
func TestProfileListsMatchEmby495(t *testing.T) {
	raw, err := os.ReadFile("testdata/emby-4.9.5-nextup.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Queries      map[string]string
		Metadata     []map[string]any
		EpisodeOrder map[string][]string
		Cases        []struct {
			Name    string
			Changes []struct {
				ID     string
				Pos    int64
				Played bool
				Last   string
			}
			Expected map[string]referenceList
		}
	}
	if err := decodeJSON(raw, &ref); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		path := strings.TrimPrefix(r.URL.Path, "/emby")
		list := make([]map[string]any, 0)
		for _, m := range ref.Metadata {
			item := make(map[string]any, len(m)+1)
			for k, v := range m {
				item[k] = v
			}
			if qget(q, "EnableUserData") != "false" {
				item["UserData"] = map[string]any{"Played": true, "PlaybackPositionTicks": 987654321, "PlayCount": 99}
			}
			list = append(list, item)
		}
		if strings.Contains(path, "/Shows/") && strings.HasSuffix(path, "/Episodes") {
			id := strings.Split(path, "/")[2]
			ordered := make([]map[string]any, 0)
			for _, id := range ref.EpisodeOrder[id] {
				for _, m := range list {
					if str(m["Id"]) == id {
						ordered = append(ordered, m)
					}
				}
			}
			list = ordered
		} else if strings.Contains(path, "/Items/") {
			id := path[strings.LastIndex(path, "/")+1:]
			for _, m := range list {
				if str(m["Id"]) == id {
					writeJSON(w, m)
					return
				}
			}
			http.NotFound(w, r)
			return
		} else if !strings.HasSuffix(path, "/Items") {
			t.Errorf("unexpected upstream request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		list = slices.DeleteFunc(list, func(m map[string]any) bool {
			for _, f := range []struct{ param, field string }{{"Ids", "Id"}, {"IncludeItemTypes", "Type"}, {"MediaTypes", "MediaType"}} {
				if v := qget(q, f.param); v != "" && !containsValue(v, str(m[f.field])) {
					return true
				}
			}
			if p := qget(q, "ParentId"); p != "" && str(m["SeriesId"]) != p && str(m["SeasonId"]) != p && str(m["ParentId"]) != p {
				return true
			}
			return false
		})
		writeJSON(w, map[string]any{"Items": list, "TotalRecordCount": len(list)})
	}))
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	px := httptest.NewServer(s)
	defer px.Close()
	for _, c := range ref.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// Reset by replacing only the test store; no old dates survive a case.
			p, _ := profile.Open("")
			s.profile = p
			for _, m := range ref.Metadata {
				b, _ := json.Marshal(m)
				var v any
				decodeJSON(b, &v)
				s.overlay(v)
			}
			for _, change := range c.Changes {
				at, err := time.Parse(time.RFC3339, change.Last)
				if err != nil {
					t.Fatal(err)
				}
				s.profile.Update(change.ID, func(e *profile.Entry) {
					e.Position = change.Pos
					e.Played = change.Played
					e.LastPlayed = at
					e.PlayCount = 1
				})
			}
			for name, path := range ref.Queries {
				t.Run(name, func(t *testing.T) {
					resp, err := http.Get(px.URL + "/emby" + path)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					var v map[string]any
					if err := decodeResponse(resp, &v); err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != 200 {
						t.Fatalf("status %d: %v", resp.StatusCode, v)
					}
					raw, _ := json.Marshal(v)
					var dto struct {
						Items []struct {
							ID       string `json:"Id"`
							UserData struct {
								PlaybackPositionTicks int64
								Played                bool
							}
						}
						TotalRecordCount int
					}
					if err := json.Unmarshal(raw, &dto); err != nil {
						t.Fatal(err)
					}
					if dto.Items == nil {
						t.Fatalf("Items must be an array: %s", raw)
					}
					got := referenceList{Total: dto.TotalRecordCount, Items: make([]referenceItem, 0, len(dto.Items))}
					for _, x := range dto.Items {
						got.Items = append(got.Items, referenceItem{x.ID, x.UserData.PlaybackPositionTicks, x.UserData.Played})
					}
					if want := c.Expected[name]; !reflect.DeepEqual(got, want) {
						t.Errorf("%s: got %+v; official %+v", path, got, want)
					}
				})
			}
		})
	}
}

func decodeResponse(resp *http.Response, v any) error {
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	return nil
}

func TestProfileQueuePagesAfterFetchingAllIDs(t *testing.T) {
	const count = 501 // crosses the upstream Ids batch boundary
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list := make([]map[string]any, 0)
		ids := query(r.URL, "Ids")
		for n := 1; n <= count; n++ {
			id := fmt.Sprint(n)
			if ids != "" && !containsValue(ids, id) {
				continue
			}
			list = append(list, map[string]any{"Id": id, "Type": "Episode", "SeriesId": "show", "ParentIndexNumber": 1, "IndexNumber": n, "UserData": map[string]any{}})
		}
		writeJSON(w, map[string]any{"Items": list, "TotalRecordCount": len(list)})
	}))
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	s.profile.Learn("1", profile.Meta{Type: "Episode", Series: "show"})
	s.profile.Progress("1", 1, time.Now())
	px := httptest.NewServer(s)
	defer px.Close()
	resp, err := http.Get(px.URL + "/emby/Shows/NextUp?UserId=u1&SeriesId=show&StartIndex=500&Limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Items []struct {
			ID string `json:"Id"`
		}
		TotalRecordCount int
	}
	if err := decodeResponse(resp, &got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || got.TotalRecordCount != count || len(got.Items) != 1 || got.Items[0].ID != "501" {
		t.Fatalf("incomplete queue: status %d, %+v", resp.StatusCode, got)
	}
}

func TestProfileListsPreserveUpstreamErrors(t *testing.T) {
	for _, failPath := range []string{"/emby/Shows/show/Episodes", "/emby/Users/u1/Items"} {
		t.Run(failPath, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == failPath {
					http.Error(w, "reference unavailable", http.StatusServiceUnavailable)
					return
				}
				writeJSON(w, map[string]any{"Items": []any{map[string]any{"Id": "1", "Type": "Episode", "SeriesId": "show", "ParentIndexNumber": 1, "IndexNumber": 1}}, "TotalRecordCount": 1})
			}))
			defer upstream.Close()
			s, _ := newTestServer(t, upstream.URL, "profile: local")
			s.profile.Learn("1", profile.Meta{Type: "Episode", Series: "show"})
			s.profile.Progress("1", 1, time.Now())
			px := httptest.NewServer(s)
			defer px.Close()
			resp, err := http.Get(px.URL + "/emby/Shows/NextUp?UserId=u1&SeriesId=show")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want upstream 503", resp.StatusCode)
			}
		})
	}
}
