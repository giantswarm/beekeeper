// Package sink dispatches a configured GitHub workflow on selected events
// (config.Sink): workflow_dispatch with the inputs verb, subject and time,
// nothing else. Every watch on the machine offers the events it follows;
// sink.json in the state directory claims each one once, so two watches
// never dispatch the same event twice.
package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Window is how old an event may be and still be dispatched: one a watch
// did not reach within it (the machine was down, no watch ran) is let go,
// and a failed dispatch is retried within it.
const Window = 10 * time.Minute

// lockTimeout bounds the wait for another watch's change of the ledger.
const lockTimeout = 10 * time.Second

// ledger is sink.json: when the sink first ran, and the claimed events.
type ledger struct {
	// Since is when the ledger was started: an event before it is never
	// dispatched, so a first watch does not replay the log.
	Since time.Time `json:"since"`
	// Claimed is when each event (Key) was claimed.
	Claimed map[string]time.Time `json:"claimed"`
}

// Ledger keeps sink.json in beekeeper's state directory, changed only under
// sink.lock.
type Ledger struct {
	dir  string
	lock *flock.Flock
}

// NewLedger returns the ledger kept in dir.
func NewLedger(dir string) *Ledger {
	return &Ledger{dir: dir, lock: flock.New(filepath.Join(dir, "sink.lock"))}
}

// Path is the ledger file.
func (l *Ledger) Path() string { return filepath.Join(l.dir, "sink.json") }

// Key names an event across the watches that read it.
func Key(e state.Event) string {
	return e.At.UTC().Format(time.RFC3339Nano) + " " + e.Verb + " " + e.Subject
}

// Claim returns the events of evs this caller dispatches: not claimed yet,
// within Window of now and not before the ledger's start, and claims them.
// The first call starts the ledger and claims nothing.
func (l *Ledger) Claim(ctx context.Context, now time.Time, evs []state.Event) ([]state.Event, error) {
	var out []state.Event
	err := l.update(ctx, now, func(st *ledger) {
		for _, e := range evs {
			k := Key(e)
			if _, ok := st.Claimed[k]; ok || e.At.Before(st.Since) || now.Sub(e.At) > Window {
				continue
			}
			st.Claimed[k] = now
			out = append(out, e)
		}
	})
	return out, err
}

// Unclaim gives e back after its dispatch failed: the next watch to offer
// it within Window tries again.
func (l *Ledger) Unclaim(ctx context.Context, now time.Time, e state.Event) error {
	return l.update(ctx, now, func(st *ledger) { delete(st.Claimed, Key(e)) })
}

// update changes the ledger under its lock with one rename, dropping the
// claims older than twice Window.
func (l *Ledger) update(ctx context.Context, now time.Time, fn func(*ledger)) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	ok, err := l.lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !ok {
		return fmt.Errorf("%s: %w", l.lock.Path(), errors.Join(err, ctx.Err()))
	}
	defer func() { _ = l.lock.Unlock() }()
	st := &ledger{}
	raw, err := os.ReadFile(l.Path())
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.Since = now
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(raw, st); err != nil {
			return fmt.Errorf("%s: %w", l.Path(), err)
		}
	}
	if st.Claimed == nil {
		st.Claimed = map[string]time.Time{}
	}
	fn(st)
	for k, t := range st.Claimed {
		if now.Sub(t) > 2*Window {
			delete(st.Claimed, k)
		}
	}
	raw, err = json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.Path() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.Path())
}

// Dispatch runs the sink's workflow for e through gh: workflow_dispatch on
// s.Ref (the repository's default branch when unset) with the inputs verb,
// subject and time.
func Dispatch(ctx context.Context, gh github.GH, s config.Sink, e state.Event) error {
	args := []string{"workflow", "run", s.Workflow, "--repo", s.Repo}
	if s.Ref != "" {
		args = append(args, "--ref", s.Ref)
	}
	args = append(args, "-f", "verb="+e.Verb, "-f", "subject="+e.Subject, "-f", "time="+e.At.UTC().Format(time.RFC3339))
	if _, err := gh(github.ActingOn(ctx, s.Repo), args...); err != nil {
		return fmt.Errorf("%s %s: %w", s.Repo, s.Workflow, err)
	}
	return nil
}
