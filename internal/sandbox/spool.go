package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Op is OpPing, OpCapSlot, OpScope, OpSecret, OpKubeconfig, OpVault or OpGate.
	Op string `json:"op"`
	// Unit is the scope to put the requester into (OpScope).
	Unit string `json:"unit,omitempty"`
	// Slice, Max and Swap are the cap (OpCapSlot, OpScope).
	Slice string `json:"slice,omitempty"`
	Max   string `json:"max,omitempty"`
	Swap  string `json:"swap,omitempty"`
	// Args are the beekeeper secret call's arguments after "secret"
	// (OpSecret), or the gated devctl command (OpGate).
	Args []string `json:"args,omitempty"`
	// Wait is how long a gated merge waits for its turn (OpGate).
	Wait string `json:"wait,omitempty"`
	// Resource is the lab lease whose kubeconfig to write (OpKubeconfig).
	Resource string `json:"resource,omitempty"`
}

// The broker's operations.
const (
	// OpPing answers whether a broker runs.
	OpPing = "ping"
	// OpCapSlot caps a build slot's slice.
	OpCapSlot = "cap-slot"
	// OpScope moves the requester into a capped scope.
	OpScope = "scope"
	// OpSecret runs a beekeeper secret call on the host, where sops and
	// op reach their keys, and answers its output and exit code.
	OpSecret = "secret"
	// OpKubeconfig writes a held lab lease's kubeconfig, which takes the
	// container runtime's socket the sandbox closes.
	OpKubeconfig = "kubeconfig"
	// OpVault answers whether the broker holds the vault session
	// (secret.session), never the session itself.
	OpVault = "vault"
	// OpGate runs a gated devctl command (beekeeper gate) on the host,
	// where devctl reads its keychain and the gate starts its units.
	OpGate = "gate"
)

// Reply is the broker's answer: an empty Error is done, Out, Err and Code
// are a call's output and exit code (OpSecret).
type Reply struct {
	Error string `json:"error,omitempty"`
	Out   string `json:"out,omitempty"`
	Err   string `json:"err,omitempty"`
	Code  int    `json:"code,omitempty"`
}

// ErrNoBroker is a request no broker answered in time.
var ErrNoBroker = errors.New("no beekeeper sandbox broker answered")

// SpoolDir is the spool under beekeeper's state directory, which the
// policy lets sessions write.
func SpoolDir(stateDir string) string { return filepath.Join(stateDir, "sandbox") }

// Ask sends req to the broker serving dir and waits up to timeout for its
// answer: nil when done, the broker's refusal, or ErrNoBroker.
func Ask(dir string, req Request, timeout time.Duration) error {
	_, err := Call(dir, req, timeout)
	return err
}

// Call is Ask that also returns the broker's reply.
func Call(dir string, req Request, timeout time.Duration) (Reply, error) {
	var r Reply
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return r, err
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return r, err
	}
	base := filepath.Join(dir, hex.EncodeToString(b[:]))
	tmp, path, answer := base+tmpSuffix, base+reqSuffix, base+replySuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // a new request in our own spool; O_EXCL refuses a planted symlink
	if err != nil {
		return r, err
	}
	// the open file is the requester's proof: held until the answer is read
	defer func() { _ = f.Close() }()
	defer func() { _ = os.Remove(path); _ = os.Remove(tmp); _ = os.Remove(answer) }()
	if err := json.NewEncoder(f).Encode(req); err != nil {
		return r, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return r, err
	}
	deadline := time.Now().Add(timeout)
	for {
		raw, err := os.ReadFile(answer) //nolint:gosec // our own spool's answer
		if err == nil {
			if err := json.Unmarshal(raw, &r); err != nil {
				return r, fmt.Errorf("the broker's answer: %w", err)
			}
			if r.Error != "" {
				return r, errors.New(r.Error)
			}
			return r, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
		if !time.Now().Before(deadline) {
			return r, fmt.Errorf("%w within %s (%s)", ErrNoBroker, timeout, dir)
		}
		// a capped run's answer comes at once, a merge's after an hour
		if time.Since(deadline.Add(-timeout)) < 5*time.Second {
			time.Sleep(20 * time.Millisecond)
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// Handler acts on one request of process pid, the request's holder, and
// answers the reply's output; an error is the refusal.
type Handler func(ctx context.Context, pid int, req Request) (Reply, error)
