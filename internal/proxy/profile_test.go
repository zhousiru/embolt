package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhousiru/embolt/internal/profile"
)

// fakeEmby holds one series of three episodes that the shared account has
// already watched, a favorite and all.
type fakeEmby struct {
	mu   sync.Mutex
	seen []string // method, path and query of every request
	body string   // of the last playback report
}

func (f *fakeEmby) episode(id string, n int) map[string]any {
	return map[string]any{
		"Id": id, "Type": "Episode", "SeriesId": "s1", "SeasonId": "se1", "ParentIndexNumber": 1,
		"IndexNumber": n, "RunTimeTicks": 36_000_000_000, // an hour
		"UserData": map[string]any{"Played": true, "PlaybackPositionTicks": 999, "IsFavorite": true, "Key": "k" + id},
	}
}

func (f *fakeEmby) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	if strings.HasPrefix(r.URL.Path, "/emby/Sessions/") {
		raw, _ := io.ReadAll(r.Body)
		f.body = string(raw)
	}
	f.mu.Unlock()
	eps := []map[string]any{f.episode("7", 1), f.episode("8", 2), f.episode("9", 3)}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch r.URL.Path {
	case "/emby/Users/u1/Items/7":
		json.NewEncoder(w).Encode(eps[0])
	case "/emby/Users/u1/Items":
		ids := strings.Split(query(r.URL, "Ids"), ",")
		eps = slices.DeleteFunc(eps, func(ep map[string]any) bool { return !slices.Contains(ids, ep["Id"].(string)) })
		json.NewEncoder(w).Encode(map[string]any{"Items": eps, "TotalRecordCount": len(eps)})
	case "/emby/Shows/s2/Episodes":
		json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
	case "/emby/Shows/s1/Episodes":
		json.NewEncoder(w).Encode(map[string]any{"Items": eps, "TotalRecordCount": len(eps)})
	case "/emby/Sessions/Playing/Stopped":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeEmby) requests(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(f.seen), func(s string) bool { return !strings.HasPrefix(s, prefix) })
}

// TestLocalProfile walks one viewer through an episode on a shared account:
// the server's user data never shows, writes stay here, and the lists that
// select by user data come from the local profile.
func TestLocalProfile(t *testing.T) {
	emby := &fakeEmby{}
	upstream := httptest.NewServer(emby)
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, "profile: local")

	call := func(method, path, body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(method, px.URL+path, strings.NewReader(body))
		req.Header.Set("X-Emby-Token", "tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		var v map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatalf("%s %s: %d, %v", method, path, resp.StatusCode, err)
		}
		return v
	}
	userData := func(v map[string]any) string {
		ud, _ := v["UserData"].(map[string]any)
		return fmt.Sprintf("played=%v pos=%v fav=%v", ud["Played"], ud["PlaybackPositionTicks"], ud["IsFavorite"])
	}
	ids := func(v map[string]any) []string {
		var out []string
		for _, it := range v["Items"].([]any) {
			out = append(out, it.(map[string]any)["Id"].(string))
		}
		return out
	}

	if got := userData(call("GET", "/emby/Users/u1/Items/7", "")); got != "played=false pos=0 fav=false" {
		t.Errorf("details show the shared account's user data: %s", got)
	}

	if v := call("POST", "/emby/Users/u1/FavoriteItems/7", ""); v["IsFavorite"] != true {
		t.Errorf("favorite answered %v", v)
	}
	if got := emby.requests("POST /emby/Users/"); len(got) > 0 {
		t.Errorf("a favorite reached the server: %v", got)
	}

	const stop = `{"ItemId":"7","PositionTicks":18000000000}`
	call("POST", "/emby/Sessions/Playing/Stopped", stop)
	if got := emby.requests("POST /emby/Sessions/Playing/Stopped"); len(got) != 1 || emby.body != stop {
		t.Errorf("the playback report did not reach the server intact: %v %q", got, emby.body)
	}

	resume := call("GET", "/emby/Users/u1/Items/Resume?Limit=12&MediaTypes=Video", "")
	if got := ids(resume); !slices.Equal(got, []string{"7"}) {
		t.Errorf("resume lists %v, want [7]", got)
	} else if got := userData(resume["Items"].([]any)[0].(map[string]any)); got != "played=false pos=1.8e+10 fav=true" {
		t.Errorf("resume item: %s", got)
	}

	favs := call("GET", "/emby/Users/u1/Items?Filters=IsFavorite,IsNotFolder&Recursive=true", "")
	if got := ids(favs); !slices.Equal(got, []string{"7"}) {
		t.Errorf("favorites list %v, want [7]", got)
	}
	last := emby.requests("GET /emby/Users/u1/Items?")
	if q := last[len(last)-1]; !strings.Contains(q, "Ids=7") || !strings.Contains(q, "Filters=IsNotFolder") || strings.Contains(q, "IsFavorite") {
		t.Errorf("favorites query reached the server as %s", q)
	}

	call("POST", "/emby/Users/u1/PlayedItems/7", "")
	next := call("GET", "/emby/Shows/NextUp?UserId=u1&LegacyNextUp=true&Limit=1", "")
	if got := ids(next); !slices.Equal(got, []string{"8"}) {
		t.Errorf("next up %v, want [8]", got)
	}
	if got := ids(call("GET", "/emby/Users/u1/Items/Resume?MediaTypes=Video&IncludeNextUp=false", "")); got != nil {
		t.Errorf("a played item stays in resume: %v", got)
	}
}

func TestLocalProfileNextUpEmptyArray(t *testing.T) {
	upstream := httptest.NewServer(&fakeEmby{})
	defer upstream.Close()
	s, _ := newTestServer(t, upstream.URL, "profile: local")
	px := httptest.NewServer(s)
	defer px.Close()

	check := func(t *testing.T, query string) {
		t.Helper()
		resp, err := http.Get(px.URL + "/emby/Shows/NextUp?UserId=u1" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || string(raw) != `{"Items":[],"TotalRecordCount":0}` {
			t.Errorf("empty NextUp %s: status=%d body=%s, want an empty Items array", query, resp.StatusCode, raw)
		}
	}

	t.Run("new profile", func(t *testing.T) { check(t, "") })
	for _, id := range []string{"7", "8", "9"} {
		s.profile.Learn(id, profile.Meta{Type: "Episode", Series: "s1"})
		s.profile.MarkPlayed(id, time.Now())
	}
	t.Run("unwatched series", func(t *testing.T) { check(t, "&SeriesId=s2") })
	t.Run("completed series", func(t *testing.T) { check(t, "&SeriesId=s1") })
	t.Run("empty page", func(t *testing.T) { check(t, "&StartIndex=10&Limit=1") })
}

func TestWSFilterRewritesOnlyUserData(t *testing.T) {
	p, _ := profile.Open("")
	p.Update("7", func(e *profile.Entry) { e.Favorite = true })
	s := &Server{profile: p}

	push := []byte(`{"MessageType":"UserDataChanged","Data":{"UserId":"u1","UserDataList":[{"ItemId":"7","Played":true,"IsFavorite":false}]}}`)
	other := []byte(`{"MessageType":"Sessions","Data":[]}`)
	binary := bytes.Repeat([]byte{0xab}, 300)
	var in bytes.Buffer
	in.Write(append(wsHeader(len(push)), push...))
	in.Write(append([]byte{0x82, 126, 1, 44}, binary...)) // 300 bytes
	in.Write(append(wsHeader(len(other)), other...))

	f := newWSFilter(struct {
		io.Reader
		io.Writer
		io.Closer
	}{&in, io.Discard, io.NopCloser(nil)}, s.userDataMessage)
	var out bytes.Buffer
	buf := make([]byte, 7) // small reads cross every frame boundary
	for {
		n, err := f.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}

	got := out.Bytes()
	hdr, size := 2, int(got[1])
	if size == 126 {
		hdr, size = 4, int(got[2])<<8|int(got[3])
	}
	var msg struct {
		Data struct{ UserDataList []map[string]any }
	}
	if err := json.Unmarshal(got[hdr:hdr+size], &msg); err != nil {
		t.Fatal(err)
	}
	if ud := msg.Data.UserDataList[0]; ud["IsFavorite"] != true || ud["Played"] != false {
		t.Errorf("pushed user data %v, want the local profile's", ud)
	}
	rest := got[hdr+size:]
	want := append(append([]byte{0x82, 126, 1, 44}, binary...), append(wsHeader(len(other)), other...)...)
	if !bytes.Equal(rest, want) {
		t.Error("other frames did not pass through byte for byte")
	}
}

// TestWebsocketPushThroughProxy upgrades through the proxy, and checks the
// server's push arrives with the local user data and uncompressed.
func TestWebsocketPushThroughProxy(t *testing.T) {
	push := []byte(`{"MessageType":"UserDataChanged","Data":{"UserId":"u1","UserDataList":[{"ItemId":"7","Played":true}]}}`)
	var extensions string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extensions = r.Header.Get("Sec-Websocket-Extensions")
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Write(append(wsHeader(len(push)), push...))
		brw.Flush()
		io.Copy(io.Discard, conn)
	}))
	defer upstream.Close()
	px := newTestProxy(t, upstream.URL, "profile: local")

	conn, err := net.Dial("tcp", strings.TrimPrefix(px.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET /embywebsocket?api_key=tok HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	f := newWSFilter(struct {
		io.Reader
		io.Writer
		io.Closer
	}{br, io.Discard, conn}, func(p []byte) []byte { push = p; return nil })
	if _, err := f.Read(make([]byte, 1<<16)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(push), `"Played":false`) {
		t.Errorf("the push reached the player as %s", push)
	}
	if extensions != "" {
		t.Errorf("the server was offered %q", extensions)
	}
}
