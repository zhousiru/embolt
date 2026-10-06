// Package view holds the pane's DTOs and is the source of web/src/api/types.gen.ts.
// Secrets stay out by construction: no type here has a field for a
// subscription URL, credential, server address or token.
package view

import "time"

type Status struct {
	Version     string    `json:"version"`
	Started     time.Time `json:"started"`
	Upstream    string    `json:"upstream"` // host only
	Primary     *NodeRef  `json:"primary,omitempty"`
	Nodes       int       `json:"nodes"`
	Usable      int       `json:"usable"`   // breaker closed
	Measured    int       `json:"measured"` // usable, with a rate measured
	Testing     *NodeRef  `json:"testing,omitempty"`
	TestsPaused time.Time `json:"testsPaused,omitzero"` // until, after the server refused a test
	Sessions    []Session `json:"sessions"`             // playing, oldest first
	Recent      []Session `json:"recent"`               // ended within a day, newest first
	Limits      Limits    `json:"limits"`
}

// Limits are the control settings the pane draws against.
type Limits struct {
	BufferMinSeconds float64 `json:"bufferMinSeconds"`
	ReadAheadSeconds float64 `json:"readAheadSeconds"`
}

type NodeRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Session is one playback as of its controller's last step, or as it ended.
type Session struct {
	Key           string    `json:"key"`
	State         string    `json:"state"` // starting, ok, risk, low, idle or ended
	Media         NodeRef   `json:"media"`
	Players       int       `json:"players"` // open player connections
	BitrateMbps   float64   `json:"bitrateMbps"`
	BufferSeconds float64   `json:"bufferSeconds"` // read-ahead plus a lower bound on the player's own
	FetchedMbps   float64   `json:"fetchedMbps"`   // read from upstream over the last step
	NodeMbps      float64   `json:"nodeMbps"`      // the media node's typical rate, 0 if not measured
	Failovers     int       `json:"failovers"`
	Tests         int       `json:"tests"`
	LowSeconds    float64   `json:"lowSeconds"` // under the low mark, once first over it
	Started       time.Time `json:"started"`
	Ended         time.Time `json:"ended,omitzero"`
	Item          *Item     `json:"item,omitempty"` // unknown until a player fetches the item's details
}

// Item is what a session plays, as the player's own request for the item's
// details showed it.
type Item struct {
	ID         string `json:"id"`
	Type       string `json:"type"` // Movie, Episode, Audio, ...
	Name       string `json:"name"`
	SeriesName string `json:"seriesName,omitempty"`
	Season     *int   `json:"season,omitempty"`
	Episode    *int   `json:"episode,omitempty"`
	Year       int    `json:"year,omitempty"`
	Image      bool   `json:"image"` // served at /api/v1/items/{id}/image
}

// SessionDetail is a session with its recent steps. Once the session has
// been forgotten, only its events remain.
type SessionDetail struct {
	Session *Session `json:"session,omitempty"`
	History []Point  `json:"history"` // oldest first, up to 10 min
	Events  []Event  `json:"events"`
}

// Point is one step of a session.
type Point struct {
	At     time.Time `json:"at"`
	Buffer float64   `json:"buffer"` // s
	Mbps   float64   `json:"mbps"`   // fetched
}

// Estimate summarizes a node's measurements: their typical value and the
// 90% range of one sample, and how many samples' worth of evidence remain
// after fade-out.
type Estimate struct {
	Measured bool    `json:"measured"`
	Mean     float64 `json:"mean"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
	Evidence float64 `json:"evidence"`
}

type Node struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Protocol    string    `json:"protocol"`
	Provider    string    `json:"provider"`
	RTTMs       Estimate  `json:"rttMs"`
	RateMbps    Estimate  `json:"rateMbps"`
	Sampled     time.Time `json:"sampled,omitzero"` // its last rate sample
	BreakerOpen bool      `json:"breakerOpen"`
	OpenUntil   time.Time `json:"openUntil,omitzero"`
	Roles       []string  `json:"roles"` // primary, media, pinned
}

type NodeDetail struct {
	Node    `tstype:",extends"`
	Samples []Sample `json:"samples"`
}

type Sample struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Mbps   float64   `json:"mbps"`
	TTFBMs float64   `json:"ttfbMs"`
	Error  string    `json:"error"`
}

type Event struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs"`
}
