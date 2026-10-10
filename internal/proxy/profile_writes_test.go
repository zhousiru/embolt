package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/profile"
)

// Contracts measured against an isolated Emby 4.9.5.0 with synthetic media.
func TestLocalProfileWriteContracts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/Sessions/") {
			if query(r.URL, "reject") == "true" {
				w.WriteHeader(400)
			} else {
				w.WriteHeader(204)
			}
			return
		}
		t.Errorf("local write reached upstream: %s", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	px := httptest.NewServer(s)
	defer px.Close()
	post := func(path, body string, status int) {
		t.Helper()
		req, _ := http.NewRequest("POST", px.URL+"/emby"+path, strings.NewReader(body))
		req.Header.Set("X-Emby-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != status {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, raw)
		}
	}
	s.profile.Learn("7", profile.Meta{Type: "Episode", Series: "s1", Runtime: 36_000_000_000})
	date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.profile.Update("7", func(e *profile.Entry) {
		e.Favorite = true
		e.Position = 100
		e.Played = true
		e.PlayCount = 4
		e.LastPlayed = date
	})
	post("/Users/u1/Items/7/UserData", `{"PlayCount":2,"IsFavorite":false}`, 204)
	e := s.profile.Get("7")
	if e.Position != 0 || e.Played || e.PlayCount != 2 || !e.Favorite || !e.LastPlayed.Equal(date) {
		t.Fatalf("UserData patch: %+v", e)
	}
	post("/Users/u1/Items/7/UserData", `{"PlaybackPositionTicks":18000000000}`, 204)
	post("/Users/u1/Items/8/UserData", `{"LastPlayedDate":"2026-01-01T00:00:00Z"}`, 204)
	if !s.profile.Get("8").LastPlayed.Equal(date) {
		t.Fatal("date-only UserData was dropped")
	}
	post("/Users/u1/Items/7/HideFromResume?Hide=true", `{}`, 200)
	if !s.profile.Hidden("7") || !s.profile.Hidden("s1") || s.profile.Get("7").Position != 18_000_000_000 {
		t.Fatal("hide must preserve progress and hide series")
	}
	post("/Sessions/Playing?reject=true", `{"ItemId":"7"}`, 400)
	if !s.profile.Hidden("7") || s.profile.Get("7").PlayCount != 2 {
		t.Fatal("rejected start mutated profile")
	}
	post("/Sessions/Playing", `{"ItemId":"7"}`, 204)
	if s.profile.Hidden("7") || s.profile.Get("7").PlayCount != 3 {
		t.Fatal("accepted start must unhide series and count play")
	}
	post("/Users/u1/Configuration", `{"SubtitleLanguagePreference":"chi","HidePlayedInLatest":false}`, 204)
	post("/Users/u1/Configuration/Partial", `{"ResumeRewindSeconds":10}`, 204)
	cfg := s.userConfiguration("u1")
	if string(cfg["SubtitleLanguagePreference"]) != `"chi"` || string(cfg["HidePlayedInLatest"]) != "false" || string(cfg["ResumeRewindSeconds"]) != "10" || string(cfg["EnableNextEpisodeAutoPlay"]) != "true" {
		t.Fatalf("partial config: %s", cfg)
	}
	post("/Users/u1/Configuration", `{}`, 204)
	if _, ok := s.userConfiguration("u1")["SubtitleLanguagePreference"]; ok {
		t.Fatal("full configuration must replace optional fields")
	}
	v := map[string]any{"Id": "new-user", "Policy": map[string]any{}, "Configuration": map[string]any{"SubtitleMode": "None", "ResumeRewindSeconds": 999}}
	s.overlay(v)
	raw, _ := json.Marshal(v["Configuration"])
	if strings.Contains(string(raw), "999") || !strings.Contains(string(raw), `"SubtitleMode":"Smart"`) {
		t.Fatalf("shared configuration leaked: %s", raw)
	}
}

func TestDisplayPreferencesIsolation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("preference write reached shared account")
		}
		writeJSON(w, map[string]any{"Id": "official-id", "Client": "emby", "SortOrder": "Descending", "CustomPrefs": map[string]string{"shared": "secret"}})
	}))
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	for _, tc := range []struct{ method, body, want string }{
		{"GET", "", `{}`},
		{"POST", `{"CustomPrefs":{"one":"1","two":"2"},"SortOrder":"Descending"}`, `{"one":"1","two":"2"}`},
		{"POST", `{"CustomPrefs":{"one":"3"}}`, `{"one":"3"}`},
		{"POST", `{}`, `{}`},
	} {
		r := httptest.NewRequest(tc.method, "/emby/DisplayPreferences/test?Client=emby", strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		if !s.serveProfile(w, r) {
			t.Fatal("preferences not handled")
		}
		if tc.method == "POST" && w.Code != 204 {
			t.Fatalf("post status: %d %s", w.Code, w.Body.String())
		}
		raw, _ := s.profile.Pref("test/emby")
		var got map[string]json.RawMessage
		json.Unmarshal(raw, &got)
		if string(got["CustomPrefs"]) != tc.want || string(got["Id"]) != `"official-id"` || string(got["SortOrder"]) != `"Ascending"` {
			t.Fatalf("preferences: %s", raw)
		}
	}
}

func TestHiddenSeriesLists(t *testing.T) {
	upstream := httptest.NewServer(&fakeEmby{})
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	s.profile.Learn("s1", profile.Meta{Type: "Series", Folder: true})
	s.profile.Learn("7", profile.Meta{Type: "Episode", Series: "s1", Runtime: 36_000_000_000})
	s.profile.Progress("7", 18_000_000_000, time.Now())
	s.profile.SetHidden("7", true)
	for _, tc := range []struct {
		path  string
		count int
	}{
		{"/Users/u1/Items/Resume?MediaTypes=Video", 0},
		{"/Users/u1/Items/Resume?MediaTypes=Video&IncludeNextUp=false", 0},
		{"/Shows/NextUp?UserId=u1&LegacyNextUp=true", 0},
		{"/Users/u1/Items/Resume?MediaTypes=Video&ParentId=s1&IncludeNextUp=false", 1},
		{"/Users/u1/Items/Resume?MediaTypes=Video&ParentId=s1", 3},
		{"/Shows/NextUp?UserId=u1&SeriesId=s1", 3},
	} {
		w := httptest.NewRecorder()
		s.serveProfile(w, httptest.NewRequest("GET", "/emby"+tc.path, nil))
		var got struct{ Items []any }
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != tc.count {
			t.Errorf("%s: %s", tc.path, w.Body.String())
		}
	}
}

func TestMarkTreeFailureDoesNotMutate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	s.profile.Learn("series", profile.Meta{Type: "Series", Folder: true})
	r := httptest.NewRequest("POST", "/emby/Users/u1/PlayedItems/series", nil)
	w := httptest.NewRecorder()
	s.serveProfile(w, r)
	if w.Code != 503 || s.profile.Get("series").Played {
		t.Fatalf("failed cascade: status=%d state=%+v", w.Code, s.profile.Get("series"))
	}
}
