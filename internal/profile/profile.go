// Package profile keeps one viewer's Emby user data on local disk: resume
// points, played marks, favorites, likes and preferences. Several Embolt
// deployments can then share one server account and still each keep their
// own history.
package profile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// Emby's resume rules, at its defaults: a position before 5% of the
// runtime keeps no resume point; one after 90%, or within a second of the
// end, marks the item played; and an item under 5 minutes long is played
// rather than resumed.
const (
	minResume   = 0.05
	maxResume   = 0.90
	minDuration = 300 * ticksPerSecond
	endSlack    = ticksPerSecond

	ticksPerSecond = 10_000_000

	metaCap   = 50000
	saveEvery = 10 * time.Second
)

// Entry is one item's user data, plus what is known about the item.
type Entry struct {
	Hidden     bool      `json:"hidden,omitempty"`
	Position   int64     `json:"pos,omitempty"` // ticks
	Played     bool      `json:"played,omitempty"`
	PlayCount  int       `json:"plays,omitempty"`
	Favorite   bool      `json:"fav,omitempty"`
	Likes      *bool     `json:"likes,omitempty"`
	LastPlayed time.Time `json:"last,omitzero"`
	Meta
}

// Meta is what an item's DTO says about it that its user data depends on.
type Meta struct {
	Type    string `json:"type,omitempty"`
	Folder  bool   `json:"folder,omitempty"`
	Runtime int64  `json:"runtime,omitempty"` // ticks
	Series  string `json:"series,omitempty"`
	Season  string `json:"season,omitempty"`
}

// resumable reports whether the item keeps a resume point: music does not.
func (m Meta) resumable() bool { return m.Type != "Audio" }

func (e *Entry) empty() bool {
	return e.LastPlayed.IsZero() && !e.Hidden && e.Position == 0 && !e.Played && e.PlayCount == 0 && !e.Favorite && e.Likes == nil
}

// merge fills m's unknown fields from o.
func (m *Meta) merge(o Meta) {
	m.Type = cmp.Or(o.Type, m.Type)
	m.Folder = m.Folder || o.Folder
	m.Runtime = cmp.Or(o.Runtime, m.Runtime)
	m.Series = cmp.Or(o.Series, m.Series)
	m.Season = cmp.Or(o.Season, m.Season)
}

// Store holds the profile and saves it to one JSON file.
type Store struct {
	path string

	mu      sync.Mutex
	items   map[string]*Entry // by item ID; only entries with user data
	meta    map[string]Meta   // learned from DTOs, for items without an entry yet
	prefs   map[string]json.RawMessage
	configs map[string]json.RawMessage
	dirty   bool
}

type file struct {
	Items   map[string]*Entry          `json:"items"`
	Prefs   map[string]json.RawMessage `json:"prefs,omitempty"`   // display preferences by ID and client
	Configs map[string]json.RawMessage `json:"configs,omitempty"` // user configuration by user ID
}

// Open loads the profile at path; a missing file is an empty profile.
// An empty path keeps it in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, items: map[string]*Entry{}, meta: map[string]Meta{},
		prefs: map[string]json.RawMessage{}, configs: map[string]json.RawMessage{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Items != nil {
		s.items = f.Items
	}
	if f.Prefs != nil {
		s.prefs = f.Prefs
	}
	if f.Configs != nil {
		s.configs = f.Configs
	}
	return s, nil
}

// Persist saves the profile every 10 s while it changes, and once more when
// ctx ends.
func (s *Store) Persist(ctx context.Context) {
	tick := time.NewTicker(saveEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
		if err := s.Save(); err != nil {
			slog.Error("could not save the profile", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Save writes the profile if it changed since the last save.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(file{Items: s.items, Prefs: s.prefs, Configs: s.configs})
	s.dirty = false
	s.mu.Unlock()
	defer func() {
		if err != nil {
			s.mu.Lock()
			s.dirty = true
			s.mu.Unlock()
		}
	}()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	err = os.Rename(tmp, s.path)
	return err
}

// Learn records what an item is, from a DTO the server sent.
func (s *Store) Learn(id string, m Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[id]; ok {
		before := e.Meta
		e.Meta.merge(m)
		s.dirty = s.dirty || e.Meta != before
		return
	}
	old := s.meta[id]
	old.merge(m)
	if len(s.meta) > metaCap {
		clear(s.meta)
	}
	s.meta[id] = old
}

// Get returns an item's entry; an item without one has no user data.
func (s *Store) Get(id string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

func (s *Store) get(id string) Entry {
	if e, ok := s.items[id]; ok {
		return *e
	}
	return Entry{Meta: s.meta[id]}
}

// Update applies f to an item's entry and returns the result.
func (s *Store) Update(id string, f func(*Entry)) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(id, f)
}

func (s *Store) update(id string, f func(*Entry)) Entry {
	e := s.get(id)
	f(&e)
	if e.empty() {
		s.meta[id] = e.Meta
		delete(s.items, id)
	} else {
		s.items[id] = &e
	}
	s.dirty = true
	return e
}

// Started records that playback began: every start counts as a play.
func (s *Store) Started(id string, now time.Time) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.update(cmp.Or(s.get(id).Series, id), func(e *Entry) { e.Hidden = false })
	return s.update(id, func(e *Entry) {
		e.PlayCount++
		e.LastPlayed = now
		if !e.resumable() {
			e.Played = true
		}
	})
}

// Progress records a position a player reported while playing. Without
// the runtime, which Embolt may not have seen yet, the position is kept as
// it is.
func (s *Store) Progress(id string, pos int64, now time.Time) Entry {
	return s.Update(id, func(e *Entry) {
		e.seen(now)
		if e.Runtime > 0 {
			e.playState(pos)
		} else if e.resumable() {
			e.Position = pos
		}
	})
}

// Stopped records where playback stopped. A player that reports no
// position is taken to have played to the end.
func (s *Store) Stopped(id string, pos *int64, now time.Time) Entry {
	return s.Update(id, func(e *Entry) {
		e.seen(now)
		if pos != nil {
			e.playState(*pos)
			return
		}
		e.PlayCount++
		e.Played, e.Position = true, 0
	})
}

// seen dates a play whose start Embolt missed, so it still sorts as recent.
func (e *Entry) seen(now time.Time) {
	if e.LastPlayed.IsZero() {
		e.LastPlayed = now
	}
}

// playState applies Emby's resume rules to a reported position. An item of
// unknown runtime is taken to have played to the end.
func (e *Entry) playState(pos int64) {
	switch {
	case e.Runtime <= 0:
		e.Played, pos = true, 0
	case pos <= 0:
		pos = 0
	case float64(pos) < minResume*float64(e.Runtime):
		pos = 0
	case float64(pos) > maxResume*float64(e.Runtime), pos >= e.Runtime-endSlack, e.Runtime < minDuration:
		e.Played, pos = true, 0
	}
	if !e.resumable() {
		pos = 0
	}
	e.Position = pos
}

// MarkPlayed marks an item played. A date counts as one more play and
// becomes the last played date; without one, the last played date stays.
func (s *Store) MarkPlayed(id string, at time.Time) Entry {
	return s.Update(id, func(e *Entry) {
		if !at.IsZero() {
			e.PlayCount++
			e.LastPlayed = at
		}
		e.PlayCount = max(e.PlayCount, 1)
		e.Played, e.Position = true, 0
		e.seen(time.Now())
	})
}

// MarkUnplayed clears an item's played mark and resume point, and the
// played mark of the season and series that hold it.
func (s *Store) MarkUnplayed(id string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.update(id, func(e *Entry) {
		e.Played, e.Position, e.PlayCount, e.LastPlayed = false, 0, 0, time.Time{}
	})
	for _, parent := range []string{e.Season, e.Series} {
		if p, ok := s.items[parent]; ok && p.Played {
			s.update(parent, func(p *Entry) { p.Played = false })
		}
	}
	return e
}

// IDs lists the items whose entries satisfy keep, most recently played
// first, at most limit of them.
func (s *Store) IDs(keep func(Entry) bool, limit int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, e := range s.items {
		if keep(*e) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Or(s.items[b].LastPlayed.Compare(s.items[a].LastPlayed), cmp.Compare(a, b))
	})
	return ids[:min(len(ids), limit)]
}

// Resumable is an item with a resume point.
func Resumable(e Entry) bool { return e.Position > 0 }

// Series lists the series with an episode played or in progress, most
// recently watched first.
func (s *Store) Series() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := map[string]time.Time{}
	for _, e := range s.items {
		if e.Series != "" && (e.Played || e.Position > 0) && e.LastPlayed.After(last[e.Series]) {
			last[e.Series] = e.LastPlayed
		}
	}
	ids := slices.Collect(maps.Keys(last))
	slices.SortFunc(ids, func(a, b string) int { return cmp.Or(last[b].Compare(last[a]), cmp.Compare(a, b)) })
	return ids
}

// Pref returns display preferences saved under key.
func (s *Store) Pref(key string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.prefs[key]
	return raw, ok
}

func (s *Store) SetPref(key string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefs[key] = raw
	s.dirty = true
}

// Config returns a user's configuration, if a player saved one here.
func (s *Store) Config(user string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configs[user]
}

func (s *Store) SetConfig(user string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[user] = raw
	s.dirty = true
}

// SetHidden hides a series (or a standalone item) without deleting progress.
func (s *Store) SetHidden(id string, hidden bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.update(cmp.Or(s.get(id).Series, id), func(e *Entry) { e.Hidden = hidden })
}

func (s *Store) Hidden(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(cmp.Or(s.get(id).Series, id)).Hidden
}
