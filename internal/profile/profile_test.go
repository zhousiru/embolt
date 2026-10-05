package profile

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const hour = 3600 * ticksPerSecond

func TestStoppedFollowsEmbysResumeRules(t *testing.T) {
	s, _ := Open("")
	for _, c := range []struct {
		runtime     int64
		pos         *int64
		wantPos     int64
		wantPlayed  bool
		description string
	}{
		{hour, ptr(hour / 100), 0, false, "under 5%: no resume point"},
		{hour, ptr(hour / 2), hour / 2, false, "midway: a resume point"},
		{hour, ptr(hour * 95 / 100), 0, true, "past 90%: played"},
		{hour, nil, 0, true, "no position: played to the end"},
		{120 * ticksPerSecond, ptr(60 * ticksPerSecond), 0, true, "under 5 minutes long: played, not resumed"},
		{0, ptr(hour / 2), 0, true, "unknown runtime: played"},
	} {
		s.MarkUnplayed("1")
		s.Learn("1", Meta{Type: "Movie", Runtime: c.runtime})
		e := s.Stopped("1", c.pos, time.Now())
		if e.Position != c.wantPos || e.Played != c.wantPlayed {
			t.Errorf("%s: position %d played %v", c.description, e.Position, e.Played)
		}
	}
}

func TestPlaybackCountsAndProgress(t *testing.T) {
	s, _ := Open("")
	s.Learn("m", Meta{Type: "Movie", Runtime: hour})
	s.Started("m", time.Now())
	s.Started("m", time.Now())
	if e := s.Progress("m", hour/50, time.Now()); e.PlayCount != 2 || e.Position != 0 {
		t.Errorf("two starts and progress at 2%%: plays %d position %d", e.PlayCount, e.Position)
	}
	if e := s.Progress("m", hour/2, time.Now()); e.Position != hour/2 || e.Played {
		t.Errorf("progress midway: position %d played %v", e.Position, e.Played)
	}
	if e := s.Progress("m", hour*95/100, time.Now()); e.Position != 0 || !e.Played {
		t.Errorf("progress past 90%%: position %d played %v", e.Position, e.Played)
	}
	if e := s.Progress("unknown", 1234, time.Now()); e.Position != 1234 || e.Played {
		t.Errorf("progress of an item of unseen runtime: position %d played %v", e.Position, e.Played)
	}

	s.Learn("song", Meta{Type: "Audio", Runtime: hour / 15})
	if e := s.Started("song", time.Now()); !e.Played {
		t.Error("a song is not played once it starts")
	}

	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s.Started("ep", t0)
	if e := s.MarkPlayed("ep", time.Time{}); e.PlayCount != 1 || !e.LastPlayed.Equal(t0) {
		t.Errorf("marked played without a date: plays %d, last played %v; want 1 and %v", e.PlayCount, e.LastPlayed, t0)
	}
	if e := s.MarkPlayed("ep", t0.Add(time.Hour)); e.PlayCount != 2 || !e.LastPlayed.Equal(t0.Add(time.Hour)) {
		t.Errorf("marked played with a date: plays %d, last played %v", e.PlayCount, e.LastPlayed)
	}
}

func TestUnplayedEpisodeUnmarksItsSeries(t *testing.T) {
	s, _ := Open("")
	s.Learn("ep", Meta{Type: "Episode", Series: "show", Season: "s1"})
	s.MarkPlayed("ep", time.Now())
	s.MarkPlayed("s1", time.Now())
	s.MarkPlayed("show", time.Now())
	s.MarkUnplayed("ep")
	if s.Get("show").Played || s.Get("s1").Played {
		t.Error("the series and season stay played after one of their episodes was unplayed")
	}
}

func TestOrderAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	s.Learn("a", Meta{Series: "x"})
	s.Learn("b", Meta{Series: "y"})
	s.Progress("a", 10, t0)
	s.Progress("b", 10, t0.Add(time.Minute))
	s.Update("c", func(e *Entry) { e.Favorite = true })
	s.SetConfig("u1", []byte(`{"HidePlayedInLatest":false}`))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.IDs(Resumable, 10); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("resumable %v, want most recent first", got)
	}
	if got := s.Series(); !slices.Equal(got, []string{"y", "x"}) {
		t.Errorf("series %v, want most recent first", got)
	}
	if !s.Get("c").Favorite || string(s.Config("u1")) != `{"HidePlayedInLatest":false}` {
		t.Error("a favorite or the configuration did not survive a restart")
	}
	s.Update("c", func(e *Entry) { e.Favorite = false })
	if _, kept := s.items["c"]; kept {
		t.Error("an entry with no user data left is kept")
	}
}

func ptr(n int64) *int64 { return &n }
