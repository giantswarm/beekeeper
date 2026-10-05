package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The spool is how a sandboxed command asks the host for what the sandbox
// closes: Claude Code's sandbox blocks every Unix socket on Linux (seccomp
// cannot filter them by path), the user bus included, so a request is a
// file in a directory both sides write. The requester holds its request
// open while it waits; the broker on the host answers only the one process
// that holds it, as that process, and never takes a process id from the
// request itself.
//
// <dir>/<id>.req is a request, renamed into place once written;
// <dir>/<id>.reply the broker's answer, renamed into place as well.
const (
	reqSuffix   = ".req"
	replySuffix = ".reply"
	tmpSuffix   = ".tmp"
	// maxRequest bounds what the broker reads of a request.
	maxRequest = 4096
)

// Request is one ask of the broker.
type Request struct {
	// Op is OpPing, OpCapSlot or OpScope.
	Op string `json:"op"`
	// Unit is the scope to put the requester into (OpScope).
	Unit string `json:"unit,omitempty"`
	// Slice, Max and Swap are the cap (OpCapSlot, OpScope).
	Slice string `json:"slice,omitempty"`
	Max   string `json:"max,omitempty"`
	Swap  string `json:"swap,omitempty"`
}

// The broker's operations.
const (
	// OpPing answers whether a broker runs.
	OpPing = "ping"
	// OpCapSlot caps a build slot's slice.
	OpCapSlot = "cap-slot"
	// OpScope moves the requester into a capped scope.
	OpScope = "scope"
)

// reply is the broker's answer: an empty Error is done.
type reply struct {
	Error string `json:"error,omitempty"`
}

// ErrNoBroker is a request no broker answered in time.
var ErrNoBroker = errors.New("no beekeeper sandbox broker answered")

// SpoolDir is the spool under beekeeper's state directory, which the
// policy lets sessions write.
func SpoolDir(stateDir string) string { return filepath.Join(stateDir, "sandbox") }

// Ask sends req to the broker serving dir and waits up to timeout for its
// answer: nil when done, the broker's refusal, or ErrNoBroker.
func Ask(dir string, req Request, timeout time.Duration) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	base := filepath.Join(dir, hex.EncodeToString(b[:]))
	tmp, path, answer := base+tmpSuffix, base+reqSuffix, base+replySuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // a new request in our own spool
	if err != nil {
		return err
	}
	// the open file is the requester's proof: held until the answer is read
	defer func() { _ = f.Close() }()
	defer func() { _ = os.Remove(path); _ = os.Remove(tmp); _ = os.Remove(answer) }()
	if err := json.NewEncoder(f).Encode(req); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		raw, err := os.ReadFile(answer) //nolint:gosec // our own spool's answer
		if err == nil {
			var r reply
			if err := json.Unmarshal(raw, &r); err != nil {
				return fmt.Errorf("the broker's answer: %w", err)
			}
			if r.Error != "" {
				return errors.New(r.Error)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w within %s (%s)", ErrNoBroker, timeout, dir)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Handler acts on one request of process pid, the request's holder.
type Handler func(pid int, req Request) error
