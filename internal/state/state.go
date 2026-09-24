// Package state is beekeeper's shared memory on the machine: the supervisor,
// grants, holds, registered agents and notes, one JSON document read and
// rewritten under an exclusive file lock, plus an append-only event log of
// every change. It outlives any session: a restarted supervisor or its
// successor reads what the previous one knew instead of rebuilding it from
// prose.
package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Party names a session (or a person) in the state.
type Party struct {
	// Session is the Claude Code session id (CLAUDE_CODE_SESSION_ID).
	Session string `json:"session,omitempty"`
	// HostSession is the desktop app's id for it (local_…); it survives a
	// CLI restart that changes Session.
	HostSession string `json:"hostSession,omitempty"`
	Name        string `json:"name"`
}

// Is reports whether p and o name the same session or person.
func (p Party) Is(o Party) bool {
	switch {
	case p.Session != "" && p.Session == o.Session:
		return true
	case p.HostSession != "" && p.HostSession == o.HostSession:
		return true
	case p.Session == "" && o.Session == "" && p.Name != "" && p.Name == o.Name:
		return true
	}
	return false
}

// Supervisor is the session holding the watch.
type Supervisor struct {
	Party
	Since time.Time `json:"since"`
}

// Grant is the supervisor's word that a session may claim a resource.
type Grant struct {
	Resource string    `json:"resource"`
	To       Party     `json:"to"`
	By       Party     `json:"by"`
	At       time.Time `json:"at"`
}

// Hold stops work on a target (a repository's merges, "github" for every
// GitHub call) until it is lifted or Until passes.
type Hold struct {
	Target string    `json:"target"`
	Reason string    `json:"reason"`
	By     Party     `json:"by"`
	At     time.Time `json:"at"`
	Until  time.Time `json:"until,omitzero"`
}

// Active reports whether the hold still applies at now.
func (h Hold) Active(now time.Time) bool {
	return h.Until.IsZero() || now.Before(h.Until)
}

// Agent is an empty session registered as spare capacity.
type Agent struct {
	Party
	Registered time.Time `json:"registered"`
	Task       string    `json:"task,omitempty"`
	AssignedAt time.Time `json:"assignedAt,omitzero"`
	IdleSince  time.Time `json:"idleSince,omitzero"`
	LastTask   string    `json:"lastTask,omitempty"`
}

// Note is an open item: a question for a person, a deadline.
type Note struct {
	ID   int       `json:"id"`
	For  string    `json:"for,omitempty"`
	Text string    `json:"text"`
	Due  time.Time `json:"due,omitzero"`
	By   Party     `json:"by"`
	At   time.Time `json:"at"`
}

// State is the whole document.
type State struct {
	Supervisor *Supervisor `json:"supervisor,omitempty"`
	// Grants are queued per resource in the order given.
	Grants []Grant `json:"grants,omitempty"`
	// Released is when each resource was last released; a grant's TTL runs
	// from the later of it and the grant.
	Released map[string]time.Time `json:"released,omitempty"`
	Holds    []Hold               `json:"holds,omitempty"`
	Agents   []Agent              `json:"agents,omitempty"`
	Notes    []Note               `json:"notes,omitempty"`
	NextNote int                  `json:"nextNote,omitempty"`
	// BudgetETag makes the budget probe a conditional request (a 304
	// costs no budget).
	BudgetETag string `json:"budgetETag,omitempty"`
}

// Event is one line of events.jsonl.
type Event struct {
	At     time.Time `json:"at"`
	By     Party     `json:"by"`
	Verb   string    `json:"verb"`
	Detail string    `json:"detail"`
}

// Store is the state directory.
type Store struct{ dir string }

// Open returns the store in dir, creating the directory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// Read returns the current state under a shared lock.
func (s *Store) Read() (*State, error) {
	l := flock.New(s.path("state.lock"))
	if err := l.RLock(); err != nil {
		return nil, err
	}
	defer func() { _ = l.Unlock() }()
	return s.load()
}

// Update runs fn on the state under the exclusive lock and writes the result
// back atomically together with the events fn returns. When fn fails nothing
// is written.
func (s *Store) Update(fn func(*State) ([]Event, error)) error {
	l := flock.New(s.path("state.lock"))
	if err := l.Lock(); err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	st, err := s.load()
	if err != nil {
		return err
	}
	events, err := fn(st)
	if err != nil {
		return err
	}
	if err := writeJSON(s.path("state.json"), st); err != nil {
		return err
	}
	return s.append(events)
}

func (s *Store) load() (*State, error) {
	st := &State{}
	raw, err := os.ReadFile(s.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path("state.json"), err)
	}
	return st, nil
}

func (s *Store) append(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.path("events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// Events returns the last n events, oldest first (all when n <= 0).
func (s *Store) Events(n int) ([]Event, error) {
	f, err := os.Open(s.path("events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}

// ReadFile decodes a JSON side file of the store (the last snapshot);
// found is false when it does not exist yet.
func (s *Store) ReadFile(name string, v any) (found bool, err error) {
	raw, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

// WriteFile atomically replaces a JSON side file of the store.
func (s *Store) WriteFile(name string, v any) error {
	return writeJSON(s.path(name), v)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
