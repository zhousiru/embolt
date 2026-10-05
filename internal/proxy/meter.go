package proxy

import (
	"slices"
	"sync"
	"time"

	"github.com/siruzhou/embolt/internal/control"
	"github.com/siruzhou/embolt/internal/measure"
)

// rampTime is reading skipped after the upstream (re)starts or the reader
// resumes from a pause: it covers the connection's ramp-up and the burst of
// bytes that piled up in socket buffers while the reader waited.
const rampTime = time.Second

// meter turns a stream's reads into rate samples taken only while the network
// is the bottleneck: the read-ahead has room and the ramp is over. A window
// broken by a pause is no sample, since the reader was waiting on the player,
// not the node.
type meter struct {
	// Owned by the reader goroutine.
	warm  time.Duration // reading still to skip
	bytes int64
	dur   time.Duration

	mu     sync.Mutex
	recent []rateAt // this node's samples within control.StreamMemory
	filled bool     // the read-ahead filled since the last take
}

type rateAt struct {
	at   time.Time
	mbps float64
}

// read counts n bytes read in d and returns a sample when a window completes.
func (m *meter) read(n int, d time.Duration) (measure.Sample, bool) {
	if m.warm > 0 {
		m.warm -= d
		return measure.Sample{}, false
	}
	m.bytes += int64(n)
	if m.dur += d; m.dur < measure.Window {
		return measure.Sample{}, false
	}
	s := measure.Sample{Kind: measure.KindPassive, Bytes: m.bytes, Dur: m.dur}
	m.bytes, m.dur = 0, 0
	now := time.Now()
	m.mu.Lock()
	m.recent = append(m.recent, rateAt{now, s.Mbps()})
	m.recent = slices.DeleteFunc(m.recent, func(r rateAt) bool { return now.Sub(r.at) > control.StreamMemory })
	m.mu.Unlock()
	return s, true
}

// paused notes that the read-ahead is full: the node keeps up.
func (m *meter) paused() {
	m.mu.Lock()
	m.filled = true
	m.mu.Unlock()
	m.restart()
}

// restart begins a new window after a ramp, on the same node.
func (m *meter) restart() { m.warm, m.bytes, m.dur = rampTime, 0, 0 }

// moved forgets the old node's samples: they say nothing of the new one.
func (m *meter) moved() {
	m.mu.Lock()
	m.recent = nil
	m.mu.Unlock()
	m.restart()
}

// take reports whether the read-ahead filled since the last take, and the
// node's recent rates, for the controller's step.
func (m *meter) take() (full bool, recent []float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	full, m.filled = m.filled, false
	for _, r := range m.recent {
		recent = append(recent, r.mbps)
	}
	return full, recent
}
