// Package takeover hands a Claude Code session's permission requests to the
// person's screen while the screen has taken the session over, and back to
// the session's own window when it has not.
//
// The state is files in one folder, so the PermissionRequest hook reads it
// without beekeeper's state lock: a session is taken over while its flag
// (<session>.json, naming the screen's process) exists and that process
// runs. Only then does the hook hold a request: it writes it into the
// session's folder (<session>/<id>.request) and waits for the screen's
// answer beside it (<id>.answer). Every other request is decided at once
// with no decision, as before.
package takeover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// GiveUp is how long the hook holds a request for the screen: under the
// hook's own 300 s timeout, so the outcome is the hook's and never depends
// on Claude Code cancelling it.
const GiveUp = 290 * time.Second

// Poll is how often a held request looks for its answer and its flag.
const Poll = 250 * time.Millisecond

// Flag says a screen has taken a session over.
type Flag struct {
	// PID is the screen's process: a flag whose process is gone is no
	// take-over.
	PID   int       `json:"pid"`
	By    string    `json:"by"`
	Since time.Time `json:"since"`
}

// Request is a permission request the hook holds for the screen.
type Request struct {
	ID      string          `json:"id"`
	Session string          `json:"session"`
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input,omitempty"`
	At      time.Time       `json:"at"`
}

// The behaviours a Decision has, as Claude Code's PermissionRequest hook
// output names them.
const (
	Allow = "allow"
	Deny  = "deny"
)

// Decision is the screen's answer to a request.
type Decision struct {
	Behavior string `json:"behavior"`
	// Message tells the model why a request was denied.
	Message string `json:"message,omitempty"`
}

// Dir is the take-over folder in beekeeper's state folder.
func Dir(stateDir string) string { return filepath.Join(stateDir, "takeover") }

const (
	flagExt    = ".json"
	requestExt = ".request"
	answerExt  = ".answer"
)

// ErrHandedBack is an answer to a request the hook no longer holds: it
// gave up, or the session's window answered it.
var ErrHandedBack = errors.New("the request is no longer held: it went back to the session's window")

// Take records that the screen f names took session over.
func Take(dir, session string, f Flag) error {
	if err := checkName(session); err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return withSessionLock(context.Background(), dir, session, func() error {
		return writeAtomic(filepath.Join(dir, session+flagExt), b)
	})
}

// Release hands session back: its held requests go to its own window at
// their next look.
func Release(dir, session string) error {
	if err := checkName(session); err != nil {
		return err
	}
	return withSessionLock(context.Background(), dir, session, func() error {
		err := os.Remove(filepath.Join(dir, session+flagExt))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// Taken returns session's flag when one exists and alive says its screen
// runs. It takes no lock: one file read.
func Taken(dir, session string, alive func(pid int) bool) (Flag, bool) {
	if checkName(session) != nil {
		return Flag{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, session+flagExt)) //nolint:gosec // a session id under the take-over folder
	if err != nil {
		return Flag{}, false
	}
	var f Flag
	if json.Unmarshal(b, &f) != nil || f.PID <= 0 || !alive(f.PID) {
		return Flag{}, false
	}
	return f, true
}

// Pending returns the requests held for session, oldest first.
func Pending(dir, session string) ([]Request, error) {
	if err := checkName(session); err != nil {
		return nil, err
	}
	folder := filepath.Join(dir, session)
	entries, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), requestExt) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(folder, entry.Name())) //nolint:gosec // a request file under the take-over folder
		if err != nil {
			continue // answered or given up since the directory read
		}
		var r Request
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Request) int { return a.At.Compare(b.At) })
	return out, nil
}

// Answer gives the held request id of session the screen's decision; a
// request no longer held is ErrHandedBack.
func Answer(dir, session, id string, d Decision) error {
	return answer(dir, session, id, d, writeAtomic)
}

// answer keeps the existence check and publication in one transition.
// publish is injectable so tests can check the lock at the write boundary.
func answer(dir, session, id string, d Decision, publish func(string, []byte) error) error {
	if err := checkName(session); err != nil {
		return err
	}
	if err := checkName(id); err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return withSessionLock(context.Background(), dir, session, func() error {
		if _, taken := Taken(dir, session, func(int) bool { return true }); !taken {
			return ErrHandedBack
		}
		if _, err := os.Stat(filepath.Join(dir, session, id+requestExt)); err != nil {
			return ErrHandedBack
		}
		return publish(filepath.Join(dir, session, id+answerExt), b)
	})
}

// Hold holds r for the screen until it answers, its flag goes or its
// screen stops, or ctx ends (the caller's GiveUp). Release and decision
// acceptance serialize on the session lock: an invalidation observed before
// consumption wins over a queued answer. A replacement flag starts a new
// takeover and cannot revive this hold. Request finalization holds the same
// lock as Answer, so an answer cannot be published after cleanup.
func Hold(ctx context.Context, dir string, r Request, alive func(pid int) bool, poll time.Duration) (d Decision, ok bool, err error) {
	if err := checkName(r.Session); err != nil {
		return Decision{}, false, err
	}
	if err := checkName(r.ID); err != nil {
		return Decision{}, false, err
	}
	folder := filepath.Join(dir, r.Session)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return Decision{}, false, err
	}
	// Remove only an empty folder, after the request and answer cleanup.
	defer func() { _ = os.Remove(folder) }()
	b, err := json.Marshal(r)
	if err != nil {
		return Decision{}, false, err
	}
	var owner Flag
	published := false
	err = withSessionLock(context.Background(), dir, r.Session, func() error {
		var taken bool
		owner, taken = Taken(dir, r.Session, alive)
		if ctx.Err() != nil || !taken {
			return nil
		}
		if err := writeAtomic(filepath.Join(dir, r.Session, r.ID+requestExt), b); err != nil {
			return err
		}
		published = true
		return nil
	})
	if err != nil || !published {
		return Decision{}, false, err
	}
	defer func() {
		// Also clean up if a read or lock operation failed. This transition
		// has its own short deadline even when the hold's context ended.
		cleanupErr := withSessionLock(context.Background(), dir, r.Session, func() error {
			return removeHeld(dir, r)
		})
		if err == nil && cleanupErr != nil {
			ok, err = false, cleanupErr
		}
	}()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		var done bool
		d, ok, done, err = inspectHeld(ctx, dir, r, owner, alive)
		if done || err != nil {
			return d, ok, err
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// inspectHeld checks invalidation, consumes an answer and removes finished
// files in one short transition. Tests can drive this boundary directly.
func inspectHeld(ctx context.Context, dir string, r Request, owner Flag, alive func(int) bool) (d Decision, ok, done bool, err error) {
	err = withSessionLock(context.Background(), dir, r.Session, func() error {
		current, taken := Taken(dir, r.Session, alive)
		if ctx.Err() != nil || !taken || current.PID != owner.PID || current.By != owner.By || !current.Since.Equal(owner.Since) {
			done = true
			return removeHeld(dir, r)
		}
		b, readErr := os.ReadFile(filepath.Join(dir, r.Session, r.ID+answerExt)) //nolint:gosec // the answer beside the request
		if readErr == nil && json.Unmarshal(b, &d) == nil && (d.Behavior == Allow || d.Behavior == Deny) {
			ok, done = true, true
			return removeHeld(dir, r)
		}
		d = Decision{}
		return nil
	})
	return d, ok, done, err
}

func removeHeld(dir string, r Request) error {
	for _, ext := range []string{requestExt, answerExt} {
		if err := os.Remove(filepath.Join(dir, r.Session, r.ID+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// checkName refuses a session or request id that is not one plain file
// name: the hook's input names the files.
func checkName(s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("takeover: %q is not a session or request id", s)
	}
	return nil
}

// writeAtomic writes b to path through a temporary file and one rename, so
// a reader sees the whole file or none.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
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
