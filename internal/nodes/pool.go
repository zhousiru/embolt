package nodes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/zhousiru/embolt/internal/config"
)

const defaultInterval = 6 * time.Hour

// Pool is the current node list, refreshed from subscriptions. Readers get an
// immutable snapshot; a refresh swaps it whole.
type Pool struct {
	cfg  *config.Store
	dir  string // last good subscription bodies
	snap atomic.Pointer[snapshot]

	sets    map[string][]*Node // by provider, "" for inline proxies; owned by Run
	updated chan struct{}
	reload  chan struct{}
}

type snapshot struct {
	list []*Node
	byID map[string]*Node
}

func NewPool(cfg *config.Store) *Pool {
	p := &Pool{
		cfg:     cfg,
		dir:     filepath.Join(cfg.Load().DataDir, "providers"),
		sets:    map[string][]*Node{},
		updated: make(chan struct{}, 1),
		reload:  make(chan struct{}, 1),
	}
	p.snap.Store(&snapshot{byID: map[string]*Node{}})
	cfg.OnChange(func(*config.Config) { notify(p.reload) })
	return p
}

// All returns every node, sorted by name.
func (p *Pool) All() []*Node { return p.snap.Load().list }

func (p *Pool) Get(id string) *Node { return p.snap.Load().byID[id] }

// Find resolves a pin: a node ID or an exact name.
func (p *Pool) Find(ref string) *Node {
	if ref == "" {
		return nil
	}
	s := p.snap.Load()
	if n := s.byID[ref]; n != nil {
		return n
	}
	for _, n := range s.list {
		if n.Name == ref {
			return n
		}
	}
	return nil
}

// Updated fires after each refresh that changed the node list.
func (p *Pool) Updated() <-chan struct{} { return p.updated }

// Run refreshes every provider now, then on its interval and on config reload.
func (p *Pool) Run(ctx context.Context) {
	due := map[string]time.Time{}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-p.reload:
			clear(due)
		case <-timer.C:
		}
		next := p.refreshDue(ctx, due)
		timer.Reset(time.Until(next))
	}
}

func (p *Pool) refreshDue(ctx context.Context, due map[string]time.Time) time.Time {
	cfg := p.cfg.Load()
	now := time.Now()
	changed := false
	if _, ok := due[""]; !ok {
		changed = p.replace("", p.build("", cfg.Proxies, func(string) bool { return true })) || changed
		due[""] = now.Add(100 * 365 * 24 * time.Hour) // inline proxies change only on reload
	}
	for name, prov := range cfg.Providers {
		if now.Before(due[name]) {
			continue
		}
		interval := time.Duration(prov.Interval) * time.Second
		if interval <= 0 {
			interval = defaultInterval
		}
		due[name] = now.Add(interval)
		mappings, err := p.fetch(ctx, name, prov)
		if err != nil {
			slog.Error("subscription refresh failed; keeping the last good list", "provider", name, "err", err)
			continue
		}
		changed = p.replace(name, p.build(name, mappings, prov.Keep)) || changed
	}
	for name := range p.sets {
		if _, ok := cfg.Providers[name]; !ok && name != "" {
			changed = p.replace(name, nil) || changed
			delete(due, name)
		}
	}
	if changed {
		notify(p.updated)
	}
	return slices.MinFunc(slices.Collect(maps.Values(due)), func(a, b time.Time) int { return a.Compare(b) })
}

// fetch downloads a provider, falling back to its last good copy on disk.
func (p *Pool) fetch(ctx context.Context, name string, prov config.Provider) ([]map[string]any, error) {
	if prov.Type == "file" {
		buf, err := os.ReadFile(prov.Path)
		if err != nil {
			return nil, err
		}
		return parseSubscription(buf)
	}
	cache := filepath.Join(p.dir, name+".yaml")
	buf, err := download(ctx, prov.URL)
	if err == nil {
		var m []map[string]any
		if m, err = parseSubscription(buf); err == nil {
			_ = os.MkdirAll(p.dir, 0o700)
			_ = os.WriteFile(cache, buf, 0o600)
			return m, nil
		}
	}
	if len(p.sets[name]) == 0 { // first start: use the copy from the last run
		if old, rerr := os.ReadFile(cache); rerr == nil {
			slog.Warn("using cached subscription", "provider", name, "err", err)
			return parseSubscription(old)
		}
	}
	return nil, err
}

func download(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "clash.meta") // providers serve mihomo YAML to this agent
	resp, err := http.DefaultClient.Do(req)
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return nil, ue.Err // the URL carries the subscription token
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// build parses mappings into nodes, reusing live nodes with the same ID so
// their warm connections survive a refresh. Pseudo-nodes drop out by filter.
func (p *Pool) build(provider string, mappings []map[string]any, keep func(string) bool) []*Node {
	old := p.snap.Load().byID
	var out []*Node
	seen := map[string]bool{}
	for _, m := range mappings {
		name, _ := m["name"].(string)
		id := nodeID(m)
		if !keep(name) || seen[id] {
			continue
		}
		seen[id] = true
		if n := old[id]; n != nil && n.Name == name {
			out = append(out, n)
			continue
		}
		n, err := newNode(provider, m)
		if err != nil {
			slog.Warn("skipping node", "provider", provider, "err", err)
			continue
		}
		out = append(out, n)
	}
	return out
}

// replace installs a provider's new node set and reports whether IDs changed.
func (p *Pool) replace(provider string, set []*Node) bool {
	keep := map[*Node]bool{}
	for _, n := range set {
		keep[n] = true
	}
	changed := len(set) != len(p.sets[provider])
	for _, n := range p.sets[provider] {
		if !keep[n] {
			changed = true
			n.close()
		}
	}
	if set == nil {
		delete(p.sets, provider)
	} else {
		p.sets[provider] = set
	}
	if !changed {
		return false
	}
	s := &snapshot{byID: map[string]*Node{}}
	for _, ns := range p.sets {
		for _, n := range ns {
			if _, dup := s.byID[n.ID]; !dup {
				s.byID[n.ID] = n
				s.list = append(s.list, n)
			}
		}
	}
	slices.SortFunc(s.list, func(a, b *Node) int { return cmp.Compare(a.Name, b.Name) })
	p.snap.Store(s)
	slog.Info("nodes refreshed", "provider", cmp.Or(provider, "inline"), "nodes", len(set), "total", len(s.list))
	return true
}

func (p *Pool) closeAll() {
	for _, ns := range p.sets {
		for _, n := range ns {
			n.close()
		}
	}
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
