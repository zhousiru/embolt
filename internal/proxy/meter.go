package proxy

import (
	"cmp"
	"sync/atomic"
	"time"

	"github.com/zhousiru/embolt/internal/measure"
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
	kind measure.Kind // of its samples; passive if unset

	// Owned by the reader goroutine.
	warm       time.Duration // reading still to skip
	bytes      int64
	dur        time.Duration
	total      int64 // everything read, ramp included, for end
	totalDur   time.Duration
	sampledAny bool

	filled atomic.Bool  // the read-ahead filled since the last full
	upAt   atomic.Int64 // when the attempt first read past its ramp, Unix ns; 0 before
}

// read counts n bytes read in d and returns a sample when a window completes.
func (m *meter) read(n int, d time.Duration) (measure.Sample, bool) {
	m.total += int64(n)
	m.totalDur += d
	if m.warm > 0 {
		m.warm -= d
		return measure.Sample{}, false
	}
	if m.upAt.Load() == 0 {
		m.upAt.Store(time.Now().UnixNano())
	}
	m.bytes += int64(n)
	if m.dur += d; m.dur < measure.Window {
		return measure.Sample{}, false
	}
	s := measure.Sample{Kind: cmp.Or(m.kind, measure.KindPassive), Bytes: m.bytes, Dur: m.dur}
	m.bytes, m.dur, m.sampledAny = 0, 0, true
	return s, true
}

// end returns the last sample of a reading that is over: the open window if
// it is at least half a window, else, when no window completed (a fast node
// can read a whole test within its ramp), everything read, ramp and all,
// which can only understate the node.
func (m *meter) end() (measure.Sample, bool) {
	s := measure.Sample{Kind: cmp.Or(m.kind, measure.KindPassive)}
	switch {
	case m.warm <= 0 && m.dur >= measure.Window/2:
		s.Bytes, s.Dur = m.bytes, m.dur
	case !m.sampledAny && m.total > 0:
		s.Bytes, s.Dur = m.total, m.totalDur
	default:
		return s, false
	}
	return s, true
}

// paused notes that the read-ahead is full: the node keeps up.
func (m *meter) paused() {
	m.filled.Store(true)
	m.restart()
}

// restart begins a new window after a ramp.
func (m *meter) restart() { m.warm, m.bytes, m.dur = rampTime, 0, 0 }

// begin starts a new upstream attempt: it is not up until it reads past its
// ramp. A pause is no new attempt.
func (m *meter) begin() {
	m.upAt.Store(0)
	m.restart()
}

// upSince reports whether the attempt has been reading past its ramp since
// t: what the feed delivered since then is the node's own doing, not a
// connection's setup.
func (m *meter) upSince(t time.Time) bool {
	at := m.upAt.Load()
	return at != 0 && at <= t.UnixNano()
}

// full reports whether the read-ahead filled since the last call, for the
// controller's step.
func (m *meter) full() bool { return m.filled.Swap(false) }
