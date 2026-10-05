package webapi

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/zhousiru/embolt/internal/view"
)

// Journal keeps the last records at Info and above for the Events page.
// Embolt logs at Info only what a person would want to see: refreshes, role
// switches, failovers, breaker trips and reloads.
type Journal struct {
	mu   sync.Mutex
	ring []view.Event
	next int
}

func NewJournal(size int) *Journal { return &Journal{ring: make([]view.Event, 0, size)} }

// Events returns the journal, newest first.
func (j *Journal) Events() []view.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := append(slices.Clone(j.ring[j.next:]), j.ring[:j.next]...)
	slices.Reverse(out)
	return out
}

func (j *Journal) add(e view.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.ring) < cap(j.ring) {
		j.ring = append(j.ring, e)
		return
	}
	j.ring[j.next] = e
	j.next = (j.next + 1) % len(j.ring)
}

// Handler tees records into the journal on their way to next.
func (j *Journal) Handler(next slog.Handler) slog.Handler { return &journalHandler{j: j, next: next} }

type journalHandler struct {
	j     *Journal
	next  slog.Handler
	attrs []slog.Attr
}

func (h *journalHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.next.Enabled(ctx, l)
}

func (h *journalHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		e := view.Event{Time: r.Time, Level: r.Level.String(), Message: r.Message, Attrs: map[string]string{}}
		for _, a := range h.attrs {
			e.Attrs[a.Key] = a.Value.String()
		}
		r.Attrs(func(a slog.Attr) bool {
			e.Attrs[a.Key] = a.Value.String()
			return true
		})
		h.j.add(e)
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *journalHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &journalHandler{j: h.j, next: h.next.WithAttrs(attrs), attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h *journalHandler) WithGroup(name string) slog.Handler {
	return &journalHandler{j: h.j, next: h.next.WithGroup(name), attrs: h.attrs}
}
