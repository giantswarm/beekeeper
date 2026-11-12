package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// lockTiming contains no command arguments, flag values, state contents or
// local paths: Command is the holder's subcommand path and flag names
// (SetCommand). HoldNS and WaitNS use the monotonic clock; the endpoints allow
// correlation with writers and timer deadlines in an installed-release proof.
type lockTiming struct {
	PID     int    `json:"pid"`
	Command string `json:"command,omitempty"`
	// Lock is commitLockName for the commit lock, empty for the state lock.
	Lock   string `json:"lock,omitempty"`
	Shared bool   `json:"shared"`
	// Busy is a wait that ended without the lock: no hold follows.
	Busy     bool      `json:"busy,omitempty"`
	WaitNS   int64     `json:"wait_ns"`
	Acquired time.Time `json:"acquired,omitzero"`
	Released time.Time `json:"released,omitzero"`
	HoldNS   int64     `json:"hold_ns"`
}

const (
	lockTraceEnv   = "BEEKEEPER_STATE_LOCK_TRACE"
	commitLockName = "commit"
)

// lockClock times one acquisition of a lock for its trace.
type lockClock struct {
	lock      string
	shared    bool
	requested time.Time
	acquired  time.Time
}

func startLockClock(lock string, shared bool) *lockClock {
	return &lockClock{lock: lock, shared: shared, requested: clock.Now()}
}

func (c *lockClock) acquire() { c.acquired = clock.Now() }

// release records the hold that ends now.
func (c *lockClock) release() {
	released := clock.Now()
	traceLock(lockTiming{
		Lock: c.lock, Shared: c.shared, WaitNS: c.acquired.Sub(c.requested).Nanoseconds(),
		Acquired: c.acquired, Released: released, HoldNS: released.Sub(c.acquired).Nanoseconds(),
	})
}

// busy records a wait that ended now without the lock.
func (c *lockClock) busy() {
	traceLock(lockTiming{Lock: c.lock, Shared: c.shared, Busy: true, WaitNS: clock.Since(c.requested).Nanoseconds()})
}

// traceLock is opt-in, best-effort diagnostics. A row is written after its
// lock is released, so a slow trace destination cannot lengthen that hold. A
// transaction's commit-lock rows are written inside its state-lock hold, which
// a slow destination does lengthen: the trace is a regular file, never a pipe.
// Operators bound the capture window and retain the trace as proof evidence.
func traceLock(t lockTiming) {
	path := os.Getenv(lockTraceEnv)
	if !filepath.IsAbs(path) {
		return
	}
	t.PID, t.Command = os.Getpid(), holderCommand()
	raw, err := json.Marshal(t)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // operator-selected absolute trace path
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(raw, '\n'))
}
