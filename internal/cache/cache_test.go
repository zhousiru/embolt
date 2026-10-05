package cache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStoreServeEvict(t *testing.T) {
	c := Open(t.TempDir(), 300) // room for two 100-byte bodies with headers
	put := func(path, body string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/jpeg"}},
			Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
		key := Key(r)
		c.Store(key, resp, true)
		io.ReadAll(resp.Body) // the player reads it through the tee
		resp.Body.Close()
		return key
	}
	serve := func(key string) (string, bool) {
		w := httptest.NewRecorder()
		ok := c.Serve(w, httptest.NewRequest(http.MethodGet, "/", nil), key)
		return w.Body.String(), ok
	}

	img := strings.Repeat("x", 100)
	a := put("/Items/1/Images/Primary?tag=a", img)
	if body, ok := serve(a); !ok || body != img {
		t.Fatalf("serve = %q, %v", body, ok)
	}
	put("/Items/2/Images/Primary?tag=b", img)
	put("/Items/3/Images/Primary?tag=c", img) // a third entry evicts the oldest
	if _, ok := serve(a); ok {
		t.Error("least recently used entry survived eviction")
	}
	if reopened := Open(c.dir, 1<<20); reopened.lru.Len() != 2 {
		t.Errorf("reopened index has %d entries, want 2", reopened.lru.Len())
	}
}
