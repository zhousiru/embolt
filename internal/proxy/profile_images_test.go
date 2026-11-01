package proxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/profile"
)

func TestResumeSeriesPoster(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "enabled"}[enabled], func(t *testing.T) {
			var imageRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/emby/Items/s1/Images/Primary" {
					imageRequests.Add(1)
					if query(r.URL, "tag") != "poster" || query(r.URL, "maxWidth") != "320" || r.Header.Get("X-Emby-Token") != "player-token" {
						t.Error("poster request lost its original tag, size or authentication")
					}
					w.Header().Set("Content-Type", "image/jpeg")
					w.Write([]byte("poster bytes"))
					return
				}
				if r.URL.Path != "/emby/Users/u1/Items" {
					http.NotFound(w, r)
					return
				}
				var list []map[string]any
				for _, id := range []string{"7", "8", "9"} {
					it := (&fakeEmby{}).episode(id, 1)
					it["ImageTags"] = map[string]any{}
					it["SeriesPrimaryImageTag"] = "poster"
					switch id {
					case "8":
						it["ImageTags"] = map[string]any{"Primary": "episode-still"}
					case "9":
						delete(it, "SeriesPrimaryImageTag")
					}
					list = append(list, it)
				}
				json.NewEncoder(w).Encode(map[string]any{"Items": list})
			}))
			defer upstream.Close()
			extra := "profile: local"
			if enabled {
				extra += "\npersonalization: {resume_series_poster: true}"
			}
			s, _ := newTestServer(t, upstream.URL, extra)
			for _, id := range []string{"7", "8", "9"} {
				s.profile.Update(id, func(e *profile.Entry) { e.Position = 123; e.LastPlayed = time.Now() })
			}
			for _, querySuffix := range []string{"", "&EnableImages=false", "&EnableImageTypes=Thumb", "&ImageTypeLimit=0"} {
				r := httptest.NewRequest("GET", "/emby/Users/u1/Items/Resume?MediaTypes=Video&IncludeNextUp=false"+querySuffix, nil)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("resume: %d %s", w.Code, w.Body)
				}
				var v any
				if err := decodeJSON(w.Body.Bytes(), &v); err != nil {
					t.Fatal(err)
				}
				list := items(v)
				if len(list) != 3 {
					t.Fatalf("got %d items", len(list))
				}
				for _, it := range list {
					id := str(it["Id"])
					tag := str(it["ImageTags"].(map[string]any)["Primary"])
					ud := it["UserData"].(map[string]any)
					if str(ud["PlaybackPositionTicks"]) != "123" || ud["Played"] != false {
						t.Fatalf("local watch state changed: %v", ud)
					}
					switch id {
					case "7":
						want := enabled && querySuffix == ""
						if strings.HasPrefix(tag, seriesPosterTag) != want {
							t.Fatalf("fallback=%q, want enabled=%v", tag, want)
						}
						if want {
							for range 2 {
								r := httptest.NewRequest("GET", "/emby/Items/7/Images/Primary?tag="+url.QueryEscape(tag)+"&maxWidth=320", nil)
								r.Header.Set("X-Emby-Token", "player-token")
								w := httptest.NewRecorder()
								s.ServeHTTP(w, r)
								if w.Code != 200 || w.Body.String() != "poster bytes" {
									t.Fatalf("poster: %d %s", w.Code, w.Body)
								}
							}
						}
					case "8":
						if tag != "episode-still" {
							t.Fatal("replaced an existing episode image")
						}
					case "9":
						if tag != "" {
							t.Fatal("invented a missing series image")
						}
					}
				}
			}
			if enabled && imageRequests.Load() != 1 {
				t.Fatalf("poster fetched %d times, want one then cached", imageRequests.Load())
			}
		})
	}
}

func TestResumePosterRouteBoundaries(t *testing.T) {
	cfg := &config.Config{Profile: "local", Personalization: config.Personalization{ResumeSeriesPoster: true}}
	s := &Server{cfg: config.Static(cfg)}
	tag := seriesPosterTag + base64.RawURLEncoding.EncodeToString([]byte("7:s1:poster"))
	for _, path := range []string{"/Items/7/Images/Primary", "/emby/items/7/images/primary/0"} {
		r := httptest.NewRequest("HEAD", path+"?TAG="+tag+"&api_key=test&quality=90", nil)
		got := s.resumePosterRequest(r)
		if got == r || !strings.Contains(got.URL.Path, "/s1/") || query(got.URL, "tag") != "poster" || query(got.URL, "api_key") != "test" || query(got.URL, "quality") != "90" {
			t.Fatal("failed to route a valid poster request")
		}
		if query(r.URL, "tag") != tag {
			t.Fatal("mutated caller request")
		}
	}
	for _, path := range []string{
		"/Items/8/Images/Primary?tag=" + tag,
		"/Items/7/Images/Thumb?tag=" + tag,
		"/Items/7/Images/Primary?tag=ordinary",
		"/Items/7/Images/Primary?tag=" + seriesPosterTag + "bad!",
		"/Items/7/Images/Primary?tag=" + seriesPosterTag + base64.RawURLEncoding.EncodeToString([]byte("7:../s1:poster")),
	} {
		r := httptest.NewRequest("GET", path, nil)
		if s.resumePosterRequest(r) != r {
			t.Fatal("rewrote unrelated or malformed image request")
		}
	}
	r := httptest.NewRequest("POST", "/Items/7/Images/Primary?tag="+tag, strings.NewReader("upload"))
	if s.resumePosterRequest(r) != r {
		t.Fatal("rewrote image upload")
	}
	cfg.Profile = "upstream"
	r = httptest.NewRequest("GET", "/Items/7/Images/Primary?tag="+tag, nil)
	if s.resumePosterRequest(r) != r {
		t.Fatal("rewrote upstream profile")
	}
	cfg.Profile = "local"
	cfg.Personalization.ResumeSeriesPoster = false
	if s.resumePosterRequest(r) != r {
		t.Fatal("rewrote with option disabled")
	}
}
