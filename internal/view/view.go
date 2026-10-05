// Package view holds the pane's DTOs and is the source of web/src/api/types.gen.ts.
// Secrets stay out by construction: no type here has a field for a
// subscription URL, credential, server address or token.
package view

import "time"

type Status struct {
	Version  string    `json:"version"`
	Started  time.Time `json:"started"`
	Upstream string    `json:"upstream"` // host only
	Primary  *NodeRef  `json:"primary,omitempty"`
	Nodes    int       `json:"nodes"`
	Usable   int       `json:"usable"` // breaker closed
	Sessions []Session `json:"sessions"`
	Limits   Limits    `json:"limits"`
}

// Limits are the control settings the pane draws against.
type Limits struct {
	StallRisk        float64 `json:"stallRisk"` // the target
	BufferMinSeconds float64 `json:"bufferMinSeconds"`
	ReadAheadSeconds float64 `json:"readAheadSeconds"`
}

type NodeRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Session struct {
	Key           string    `json:"key"`
	Media         NodeRef   `json:"media"`
	Streams       int       `json:"streams"` // open player connections
	BitrateMbps   float64   `json:"bitrateMbps"`
	BufferSeconds float64   `json:"bufferSeconds"`
	LiveMbps      float64   `json:"liveMbps"`
	StallRisk     float64   `json:"stallRisk"` // of its media node, at its buffer and bitrate
	Failovers     int       `json:"failovers"`
	Started       time.Time `json:"started"`
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

// SessionDetail is a session and the choice its next step faces. Once the
// session ends, only its events remain.
type SessionDetail struct {
	Session *Session `json:"session,omitempty"`
	Verdict *Verdict `json:"verdict,omitempty"`
	Choices []Choice `json:"choices"` // staying first, then moves from best
	Events  []Event  `json:"events"`
}

// Verdict is what the next step does: stay, or switch To.
type Verdict struct {
	To     *NodeRef `json:"to,omitempty"`
	Reason string   `json:"reason"`
}

// Choice is one node as the session's step judges it: staying on the media
// node, or switching to another, which delivers nothing for its gap.
type Choice struct {
	Node         NodeRef  `json:"node"`
	Role         string   `json:"role"` // media or ""
	GapSeconds   float64  `json:"gapSeconds"`
	StallRisk    float64  `json:"stallRisk"`
	StallSeconds float64  `json:"stallSeconds"` // expected over the horizon
	RateMbps     Estimate `json:"rateMbps"`     // as judged: the media node's includes the stream's samples
}

// Estimate summarizes a belief: its predictive median and 90% range, and
// how many samples' worth of evidence it holds after fade-out. Unmeasured,
// it is only the starting guess every new node shares.
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
