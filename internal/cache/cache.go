// Package cache is a disk LRU for images and subtitles, keyed by path and
// full query. Tagged image URLs never expire, since the tag changes with the
// image; untagged entries live 1 h. On a disk error the cache turns itself
// off and requests go upstream.
package cache

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	untaggedTTL = time.Hour
	maxObject   = 16 << 20
)

var lookups = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "embolt_cache_lookups_total", Help: "Image and subtitle cache lookups, by result.",
}, []string{"result"})

// Meta is stored in front of each body.
type Meta struct {
	Type    string    `json:"type"`
	ETag    string    `json:"etag,omitempty"`
	Expires time.Time `json:"expires,omitzero"`
}

type Cache struct {
	dir string
	max int64
	off atomic.Bool

	mu   sync.Mutex
	lru  *list.List // of *item, front = most recent
	idx  map[string]*list.Element
	size int64
}

type item struct {
	name string
	size int64
}

// Open indexes dir, oldest first by mtime, and trims it to max bytes.
func Open(dir string, max int64) *Cache {
	c := &Cache{dir: dir, max: max, lru: list.New(), idx: map[string]*list.Element{}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.fail(err)
		return c
	}
	entries, _ := os.ReadDir(dir)
	type found struct {
		item
		mtime time.Time
	}
	var all []found
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || filepath.Ext(e.Name()) == ".tmp" {
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		all = append(all, found{item{e.Name(), info.Size()}, info.ModTime()})
	}
	slices.SortFunc(all, func(a, b found) int { return a.mtime.Compare(b.mtime) })
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range all {
		c.idx[f.name] = c.lru.PushFront(&item{f.name, f.size})
		c.size += f.size
	}
	c.evict()
	return c
}

// Key names the entry for a request.
func Key(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.URL.Path + "?" + r.URL.RawQuery))
	return hex.EncodeToString(sum[:16])
}

// Serve answers r from the cache and reports whether it did.
func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, key string) bool {
	if c.off.Load() {
		return false
	}
	f, meta, body, err := c.open(key)
	if err != nil {
		lookups.WithLabelValues("miss").Inc()
		return false
	}
	defer f.Close()
	lookups.WithLabelValues("hit").Inc()
	h := w.Header()
	h.Set("Content-Type", meta.Type)
	if meta.ETag != "" {
		h.Set("ETag", meta.ETag)
	}
	h.Set("X-Embolt-Cache", "hit")
	http.ServeContent(w, r, "", time.Time{}, body)
	return true
}

func (c *Cache) open(key string) (*os.File, Meta, *io.SectionReader, error) {
	c.mu.Lock()
	el, ok := c.idx[key]
	if ok {
		c.lru.MoveToFront(el)
	}
	c.mu.Unlock()
	if !ok {
		return nil, Meta{}, nil, fs.ErrNotExist
	}
	f, err := os.Open(filepath.Join(c.dir, key))
	if err != nil {
		c.drop(key)
		return nil, Meta{}, nil, err
	}
	meta, offset, err := readMeta(f)
	if err == nil && !meta.Expires.IsZero() && time.Now().After(meta.Expires) {
		err = errors.New("expired")
	}
	if err != nil {
		f.Close()
		c.drop(key)
		return nil, Meta{}, nil, err
	}
	info, _ := f.Stat()
	return f, meta, io.NewSectionReader(f, offset, info.Size()-offset), nil
}

// Store wraps a 200 response body so that reading it to the end also stores
// it. Bodies over 16 MB, or responses that fail midway, are not stored.
func (c *Cache) Store(key string, resp *http.Response, tagged bool) {
	if c.off.Load() || resp.StatusCode != http.StatusOK || resp.ContentLength > maxObject {
		return
	}
	meta := Meta{Type: resp.Header.Get("Content-Type"), ETag: resp.Header.Get("ETag")}
	if !tagged {
		meta.Expires = time.Now().Add(untaggedTTL)
	}
	f, err := os.CreateTemp(c.dir, "*.tmp")
	if err == nil {
		err = writeMeta(f, meta)
	}
	if err != nil {
		c.fail(err)
		return
	}
	resp.Body = &tee{c: c, key: key, src: resp.Body, f: f}
}

type tee struct {
	c   *Cache
	key string
	src io.ReadCloser
	f   *os.File
	n   int64
}

func (t *tee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if t.f != nil && n > 0 {
		if t.n += int64(n); t.n > maxObject {
			t.abort()
		} else if _, werr := t.f.Write(p[:n]); werr != nil {
			t.c.fail(werr)
			t.abort()
		}
	}
	if err == io.EOF && t.f != nil {
		t.commit()
	}
	return n, err
}

func (t *tee) Close() error {
	if t.f != nil {
		t.abort() // closed before EOF: the copy is incomplete
	}
	return t.src.Close()
}

func (t *tee) commit() {
	tmp := t.f.Name()
	info, _ := t.f.Stat()
	err := t.f.Close()
	t.f = nil
	if err == nil {
		err = os.Rename(tmp, filepath.Join(t.c.dir, t.key))
	}
	if err != nil {
		os.Remove(tmp)
		t.c.fail(err)
		return
	}
	t.c.add(t.key, info.Size())
}

func (t *tee) abort() {
	t.f.Close()
	os.Remove(t.f.Name())
	t.f = nil
}

func (c *Cache) add(key string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		c.size -= el.Value.(*item).size
		c.lru.Remove(el)
	}
	c.idx[key] = c.lru.PushFront(&item{key, size})
	c.size += size
	c.evict()
}

func (c *Cache) evict() {
	for c.size > c.max && c.lru.Len() > 0 {
		it := c.lru.Remove(c.lru.Back()).(*item)
		delete(c.idx, it.name)
		c.size -= it.size
		os.Remove(filepath.Join(c.dir, it.name))
	}
}

func (c *Cache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		c.size -= el.Value.(*item).size
		c.lru.Remove(el)
		delete(c.idx, key)
	}
	os.Remove(filepath.Join(c.dir, key))
}

func (c *Cache) fail(err error) {
	if !c.off.Swap(true) {
		slog.Error("image cache turned off after a disk error", "err", err)
	}
}

// File layout: 4-byte big-endian header length, JSON Meta, body.
func writeMeta(w io.Writer, m Meta) error {
	raw, _ := json.Marshal(m)
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(len(raw)))
	buf.Write(raw)
	_, err := w.Write(buf.Bytes())
	return err
}

func readMeta(f *os.File) (Meta, int64, error) {
	var n uint32
	if err := binary.Read(f, binary.BigEndian, &n); err != nil || n > 64<<10 {
		return Meta{}, 0, errors.New("corrupt cache entry")
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		return Meta{}, 0, err
	}
	var m Meta
	return m, 4 + int64(n), json.Unmarshal(raw, &m)
}
