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
	return writeAtomic(filepath.Join(dir, session+flagExt), b)
}

// Release hands session back: its held requests go to its own window at
// their next look.
func Release(dir, session string) error {
	if err := checkName(session); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(dir, session+flagExt))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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
	paths, err := filepath.Glob(filepath.Join(dir, session, "*"+requestExt))
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // a request file under the take-over folder
		if err != nil {
			continue // answered or given up since the glob
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
	if err := checkName(session); err != nil {
		return err
	}
	if err := checkName(id); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, session, id+requestExt)); err != nil {
		return ErrHandedBack
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, session, id+answerExt), b)
}

// Hold holds r for the screen until it answers, its flag goes or its
// screen stops, or ctx ends (the caller's GiveUp): ok is false for each
// of the latter, and the request goes to the session's own window. Hold
// leaves no file behind.
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
	b, err := json.Marshal(r)
	if err != nil {
		return Decision{}, false, err
	}
	req, ans := filepath.Join(folder, r.ID+requestExt), filepath.Join(folder, r.ID+answerExt)
	if err := writeAtomic(req, b); err != nil {
		return Decision{}, false, err
	}
	defer func() {
		_ = os.Remove(req)
		_ = os.Remove(ans)
	}()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if b, err := os.ReadFile(ans); err == nil { //nolint:gosec // the answer beside the request
			var d Decision
			if json.Unmarshal(b, &d) == nil && (d.Behavior == Allow || d.Behavior == Deny) {
				return d, true, nil
			}
		}
		if _, taken := Taken(dir, r.Session, alive); !taken {
			return Decision{}, false, nil
		}
		select {
		case <-ctx.Done():
			return Decision{}, false, nil
		case <-t.C:
		}
	}
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
