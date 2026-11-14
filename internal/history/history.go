// Package history records events and decisions in Postgres (contract §5.2, plan §5). The Writer never
// blocks deciding: rows are buffered in memory (10,000, then dropped and counted) while Postgres is down.
package history

import (
	"context"
	"time"
)

// Event is a row of the events table.
type Event struct {
	// ID is assigned when the row is written (ascending); the harness uses it for since_step.
	ID     int64
	At     time.Time
	Team   string
	Item   string // issue ref, e.g. giantswarm/roadmap#101
	Change string // PR ref, e.g. giantswarm/klaus#55
	Type   string
	Source string
	Data   map[string]any
}

// Decision is a row of the decisions table, unique on (Point, Subject, InputsHash).
type Decision struct {
	ID            int64
	At            time.Time
	Point         string
	Subject       string // an item's bare ref or a person's login
	Other         string // the other item of parallel-safe?
	Inputs        map[string]any
	InputsHash    string
	Rule          string // the rule's answer
	JEV           string
	JEVConfidence float64
	JEVError      string
	Chosen        string
	Mode          string
	// Outcome is agree, disagree, jev_error or jev_low_confidence; empty until JEV answered (or mode rule).
	Outcome string
	Team    string
}

// Key identifies a decision.
type Key struct{ Point, Subject, InputsHash string }

// Key returns the decision's key.
func (d Decision) Key() Key { return Key{d.Point, d.Subject, d.InputsHash} }

// Duration kinds.
const (
	DurationCI    = "ci"
	DurationStage = "stage"
)

// Writer appends history. Every method returns at once; rows are written asynchronously.
type Writer interface {
	Append(events ...Event)
	// Decide records a decision once per key; it reports false when the key was recorded before.
	Decide(d Decision) bool
	// Answer sets JEV's answer and the outcome on a recorded decision.
	Answer(k Key, jev string, confidence float64, jevErr string, outcome string)
	// AddDuration records an observed duration (kind ci: key is the repo; stage: team/phase).
	AddDuration(kind, key string, d time.Duration, at time.Time)
	// Pending is the number of buffered rows not yet written (settle probe).
	Pending() int
	// Dropped is the number of rows dropped while the buffer was full.
	Dropped() int
}

// EventFilter selects events; empty fields match everything.
type EventFilter struct {
	Item, Change, Type, Source string
	// AfterID selects rows with a larger ID.
	AfterID int64
}

// DecisionFilter selects decisions.
type DecisionFilter struct {
	Point, Subject string
	AfterID        int64
}

// Reader reads history back (the harness, replay, and the engine at startup).
type Reader interface {
	Events(ctx context.Context, f EventFilter) ([]Event, error)
	Decisions(ctx context.Context, f DecisionFilter) ([]Decision, error)
	// Durations returns the recorded durations of kind and key, oldest first.
	Durations(ctx context.Context, kind, key string) ([]time.Duration, error)
	// MaxIDs returns the largest event and decision ids written so far (a step mark).
	MaxIDs(ctx context.Context) (events, decisions int64, err error)
	// DecisionKeys returns every recorded decision key (to rebuild the once-per-key set at startup).
	DecisionKeys(ctx context.Context) ([]Key, error)
}
