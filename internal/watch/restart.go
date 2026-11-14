package watch

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// RestartRequest is the restart file's content.
type RestartRequest struct {
	At time.Time `json:"at"`
	// Binary is the path the install wrote.
	Binary string `json:"binary"`
}

// RestartWatch tells a long-running process when an install asked it to
// restart on the installed binary.
type RestartWatch struct {
	File string
	// since is the newest request time this process already answered: its
	// start, or the request it was re-executed for.
	Since time.Time
	// binary says the running binary's path and whether another file was
	// renamed over it; nil reads this process's.
	Binary func() (string, bool)
}

// Due is the request this process restarts for, nil when none: one newer
// than the last it answered, for the binary path this process runs, which
// another file replaced since.
func (r *RestartWatch) Due() *RestartRequest {
	raw, err := os.ReadFile(r.File)
	if err != nil {
		return nil
	}
	var req RestartRequest
	if json.Unmarshal(raw, &req) != nil || !req.At.After(r.Since) {
		return nil
	}
	bin := r.Binary
	if bin == nil {
		bin = func() (string, bool) { return Cmd.ProcessBinary(os.Getpid()) }
	}
	if path, replaced := bin(); !replaced || path != req.Binary {
		return nil
	}
	return &req
}

// Exec replaces this process with the installed binary for req; it returns
// only when that binary does not start, and the process then stays on the
// running one, req answered.
func (r *RestartWatch) Exec(req *RestartRequest) error {
	r.Since = req.At
	b := platform.RunningBinary()
	if b == nil {
		return errors.New("the running binary is unknown")
	}
	return b.Exec(restartedEnv + "=" + strconv.FormatInt(req.At.UnixNano(), 10))
}
