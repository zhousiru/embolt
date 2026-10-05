package measure

import (
	"bufio"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const keepSamples = 30 * 24 * time.Hour

// sampleLog appends every sample to <dir>/<date>.jsonl for offline replay,
// and drops files older than 30 days when it opens a new day.
type sampleLog struct {
	dir string

	mu   sync.Mutex
	day  string
	file *os.File
}

func (l *sampleLog) write(s Sample) {
	line, _ := json.Marshal(s)
	l.mu.Lock()
	defer l.mu.Unlock()
	if day := s.Time.UTC().Format(time.DateOnly); day != l.day {
		l.rotate(day)
	}
	if l.file != nil {
		l.file.Write(append(line, '\n'))
	}
}

func (l *sampleLog) rotate(day string) {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	l.day = day
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		slog.Error("sample log disabled", "err", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(l.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Error("sample log disabled", "err", err)
		return
	}
	l.file = f
	cutoff := time.Now().Add(-keepSamples).UTC().Format(time.DateOnly)
	old, _ := filepath.Glob(filepath.Join(l.dir, "*.jsonl"))
	for _, p := range old {
		if strings.TrimSuffix(filepath.Base(p), ".jsonl") < cutoff {
			os.Remove(p)
		}
	}
}

// ReadSamples yields every sample under dir in time order of files.
func ReadSamples(dir string) iter.Seq2[Sample, error] {
	return func(yield func(Sample, error) bool) {
		files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if err != nil {
			yield(Sample{}, err)
			return
		}
		sort.Strings(files)
		for _, p := range files {
			f, err := os.Open(p)
			if err != nil {
				yield(Sample{}, err)
				return
			}
			ok := decodeLines(f, yield)
			f.Close()
			if !ok {
				return
			}
		}
	}
}

func decodeLines(r io.Reader, yield func(Sample, error) bool) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			continue // a torn last line after a crash
		}
		if !yield(s, nil) {
			return false
		}
	}
	return sc.Err() == nil || yield(Sample{}, sc.Err())
}
