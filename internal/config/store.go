package config

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Store holds the live config and reloads it on file change or SIGHUP.
// Readers call Load each time they need a value, so changes apply at once.
type Store struct {
	path string
	cur  atomic.Pointer[Config]

	mu    sync.Mutex
	mtime time.Time
	subs  []func(*Config)
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	c, err := Load(path)
	if err != nil {
		return nil, err
	}
	s.cur.Store(c)
	s.mtime = modTime(path)
	return s, nil
}

// Static wraps a fixed config; used by tests.
func Static(c *Config) *Store {
	s := &Store{}
	s.cur.Store(c)
	return s
}

func (s *Store) Load() *Config { return s.cur.Load() }

// OnChange registers f to run after every successful reload.
func (s *Store) OnChange(f func(*Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, f)
}

// Watch reloads on SIGHUP and when the file's mtime changes. Polling, unlike
// inotify, survives editors that replace the file and bind-mounted volumes.
func (s *Store) Watch(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			s.reload(true)
		case <-tick.C:
			s.reload(false)
		}
	}
}

func (s *Store) reload(force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mt := modTime(s.path)
	if !force && mt.Equal(s.mtime) {
		return
	}
	s.mtime = mt
	next, err := Load(s.path)
	if err != nil {
		slog.Error("config reload failed; keeping the current config", "err", err)
		return
	}
	prev := s.cur.Swap(next)
	if prev.Listen != next.Listen || prev.Upstream.URL != next.Upstream.URL || prev.Web.Listen != next.Web.Listen ||
		prev.TLS != next.TLS || prev.DataDir != next.DataDir || !prev.DNS.equal(next.DNS) {
		slog.Warn("config reloaded; listener, upstream, dns and data_dir changes apply after a restart")
	} else {
		slog.Info("config reloaded")
	}
	for _, f := range s.subs {
		f(next)
	}
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
