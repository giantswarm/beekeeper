package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// lockTiming contains no command arguments, state contents or local paths.
// HoldNS uses the monotonic clock; the endpoints allow correlation with writers
// and timer deadlines in an installed-release proof.
type lockTiming struct {
	PID      int       `json:"pid"`
	Shared   bool      `json:"shared"`
	Acquired time.Time `json:"acquired"`
	Released time.Time `json:"released"`
	HoldNS   int64     `json:"hold_ns"`
}

const lockTraceEnv = "BEEKEEPER_STATE_LOCK_TRACE"

// traceLockHold is opt-in, best-effort diagnostics. All diagnostic I/O happens
// after unlocking so a slow trace destination cannot lengthen a state hold.
// Operators bound the capture window and retain the trace as proof evidence.
func traceLockHold(acquired, released time.Time, shared bool) {
	path := os.Getenv(lockTraceEnv)
	if !filepath.IsAbs(path) {
		return
	}
	raw, err := json.Marshal(lockTiming{
		PID: os.Getpid(), Shared: shared, Acquired: acquired, Released: released,
		HoldNS: released.Sub(acquired).Nanoseconds(),
	})
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
