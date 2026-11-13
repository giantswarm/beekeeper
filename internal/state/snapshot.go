package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/Masterminds/semver/v3"
)

// StaleWriterError is a save refused because a newer release wrote the
// state, or a state a newer release wrote that this binary does not load
// (Load): this binary would save it by rules that release changed (a watch,
// a start's reopen or a gate call left running by an install). The state
// stays as it is; the process ends or is restarted, and the installed
// beekeeper runs its command.
type StaleWriterError struct {
	PID     int
	Command string
	// Version is this binary's, Newer the one that wrote the state.
	Version, Newer string
	// Load is why the state does not decode, nil for a refused save.
	Load error
}

func (e *StaleWriterError) Error() string {
	if e.Load != nil {
		return fmt.Sprintf("pid %d (%s) runs beekeeper %s, older than the %s that wrote the state: it does not load it (%v); restart it on the installed beekeeper", e.PID, e.Command, e.Version, e.Newer, e.Load)
	}
	return fmt.Sprintf("pid %d (%s) runs beekeeper %s, older than the %s that wrote the state: its save is refused; restart it on the installed beekeeper", e.PID, e.Command, e.Version, e.Newer)
}

func (e *StaleWriterError) Unwrap() error { return e.Load }

// VerbStaleWriter is the event of an older release's first refused save
// after a newer one wrote the state.
const VerbStaleWriter = "state.stale-writer"

// stamp records the binary as the state's writer. A newer release wrote it:
// the save is refused (StaleWriterError), logged once per process, since
// this binary would save by rules that release changed. A build without a
// release version (dev, a release candidate, a +dirty branch build) neither
// stamps nor is judged, and a stamp of one is overwritten by the next
// release that saves.
func (s *FileStore) stamp(st *State, now time.Time) error {
	if err := s.checkWriter(st, now); err != nil {
		return err
	}
	if _, ok := release(s.version); !ok {
		return nil
	}
	st.Writer = &Writer{Version: s.version, rest: writerRest(st.Writer)}
	// Releases before the refusal recorded their stale saves here instead.
	if _, has := st.rest["staleWriters"]; has {
		st.rest = maps.Clone(st.rest)
		delete(st.rest, "staleWriters")
	}
	return nil
}

// checkWriter refuses an older release before an update can have side effects.
// The caller holds the state lock.
func (s *FileStore) checkWriter(st *State, now time.Time) error {
	writer := ""
	if st.Writer != nil {
		writer = st.Writer.Version
	}
	return s.checkWriterVersion(writer, now)
}

// checkWriterVersion is checkWriter of a state its writer wrote, "" none.
func (s *FileStore) checkWriterVersion(writer string, now time.Time) error {
	if err := s.checkRecoveryFence(); err != nil {
		return err
	}
	own, ok := release(s.version)
	if !ok {
		return nil
	}
	if newer, ok := release(writer); ok && own.LessThan(newer) {
		return s.refuse(writer, now)
	}
	return nil
}

// refuse is the refused save's error, logged the first time in this process.
func (s *FileStore) refuse(newer string, now time.Time) error {
	err := &StaleWriterError{PID: os.Getpid(), Command: holderCommand(), Version: s.version, Newer: newer}
	s.mu.Lock()
	logged := s.refused
	s.refused = true
	s.mu.Unlock()
	if !logged {
		_ = s.append([]Event{{At: now, By: Party{Name: err.Command}, Verb: VerbStaleWriter, Detail: err.Error()}})
	}
	return err
}

// release is v as a release version: no prerelease, no build metadata.
func release(v string) (*semver.Version, bool) {
	r, err := semver.NewVersion(v)
	if err != nil || r.Prerelease() != "" || r.Metadata() != "" {
		return nil, false
	}
	return r, true
}

func writerRest(w *Writer) rest {
	if w == nil {
		return nil
	}
	return w.rest
}

func (s *FileStore) load() (*State, error) {
	f, err := os.Open(s.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return s.readSnapshot(f)
}

func (s *FileStore) decode(raw []byte) (*State, error) {
	st := &State{}
	if err := json.Unmarshal(raw, st); err != nil {
		err = fmt.Errorf("%s: %w", s.path("state.json"), err)
		if newer := s.newerWriter(raw); newer != "" {
			return nil, &StaleWriterError{PID: os.Getpid(), Command: holderCommand(), Version: s.version, Newer: newer, Load: err}
		}
		return nil, err
	}
	return st, nil
}

// newerWriter is the release that wrote raw, a state this binary does not
// decode, when it is newer than this one; "" otherwise. A build without a
// release version judges none, as stamp does.
func (s *FileStore) newerWriter(raw []byte) string {
	own, ok := release(s.version)
	if !ok {
		return ""
	}
	var head struct {
		Writer struct {
			Version string `json:"version"`
		} `json:"writer"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return ""
	}
	if newer, ok := release(head.Writer.Version); ok && own.LessThan(newer) {
		return head.Writer.Version
	}
	return ""
}
