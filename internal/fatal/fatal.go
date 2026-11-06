// Package fatal owns the persistent, machine-local outage queue. It deliberately
// does not depend on the supervisor, hooks, board or main configuration loader.
package fatal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const File = "fatal.json"

// Observation distinguishes a failed probe from one that could not run, and
// from a component deliberately disabled by its owner.
type Observation struct {
	ID, Evidence              string
	Failed, Unknown, Disabled bool
}

// Incident survives restarts. Only a successful probe resolves an outage;
// finishing a repair session never does. Attempts are reserved before launch.
type Incident struct {
	ID          string    `json:"id"`
	Evidence    string    `json:"evidence"`
	Owner       string    `json:"owner,omitempty"`
	Opened      time.Time `json:"opened"`
	Resolved    time.Time `json:"resolved,omitzero"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	Attempts    int       `json:"attempts"`
	Outcome     string    `json:"outcome,omitempty"`
}

type Queue struct {
	Incidents []Incident `json:"incidents"`
}

func Read(dir string) (Queue, error) {
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return Queue{}, nil
	}
	if err != nil {
		return Queue{}, err
	}
	var q Queue
	err = json.Unmarshal(b, &q)
	return q, err
}

// Save atomically publishes the admission barrier before any repair starts.
// The caller must hold fatal.lock for the entire reconciliation.
func (q Queue) Save(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".fatal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(dir, File))
}

// Observe returns a transition message, empty for a repeated observation.
// Unknown never clears a known outage. Disabled explicitly withdraws it.
func (q *Queue) Observe(o Observation, now time.Time) string {
	for i := range q.Incidents {
		x := &q.Incidents[i]
		if x.ID != o.ID {
			continue
		}
		if o.Unknown {
			return ""
		}
		if !o.Failed || o.Disabled {
			if !x.Resolved.IsZero() {
				return ""
			}
			x.Resolved = now
			x.Outcome = "verified healthy"
			if o.Disabled {
				x.Outcome = "deliberately disabled"
			}
			return fmt.Sprintf("%s: %s", x.ID, x.Outcome)
		}
		if x.Resolved.IsZero() {
			x.Evidence = o.Evidence
			return ""
		}
		*x = Incident{ID: o.ID, Evidence: o.Evidence, Opened: now}
		return "fatal detected: " + o.ID
	}
	if o.Unknown || o.Disabled || !o.Failed {
		return ""
	}
	q.Incidents = append(q.Incidents, Incident{ID: o.ID, Evidence: o.Evidence, Opened: now})
	return "fatal detected: " + o.ID
}

// Barrier leaves existing work running, admitting only the reserved repair
// owner while an outage stands. A display name cannot grant stop/hold bypass.
func (q Queue) Barrier(name string) error {
	if name != "" {
		for _, x := range q.Incidents {
			if x.Owner == name && x.Resolved.IsZero() {
				return nil
			}
		}
	}
	for _, x := range q.Incidents {
		if x.Resolved.IsZero() {
			return fmt.Errorf("fatal outage %s takes priority (owner %q): beekeeper fatal status", x.ID, x.Owner)
		}
	}
	return nil
}

// Reserve chooses at most one repair, with a global owner and a bounded retry
// budget. A live owner blocks every other repair, even after its cooldown.
func (q *Queue) Reserve(now time.Time, live func(string) bool) *Incident {
	for _, x := range q.Incidents {
		if x.Owner != "" && live(x.Owner) {
			return nil
		}
	}
	for i := range q.Incidents {
		x := &q.Incidents[i]
		if x.Resolved.IsZero() && x.Attempts >= 3 {
			x.Outcome = "repair budget exhausted; outage unresolved"
		}
		if !x.Resolved.IsZero() || x.Attempts >= 3 || (!x.LastAttempt.IsZero() && now.Sub(x.LastAttempt) < 10*time.Minute) {
			continue
		}
		x.Attempts++
		x.LastAttempt = now
		x.Owner = fmt.Sprintf("fatal repair %d %d", x.Opened.UnixNano(), x.Attempts)
		x.Outcome = "reserved; launch outcome not yet known"
		return x
	}
	return nil
}
