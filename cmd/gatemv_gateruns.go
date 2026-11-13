package cmd

import (
	"encoding/json"
	"os"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// RunInfoExt is the extension of what gate runs shows of a detached run
	// (runInfo), which its merge-child writes before it starts the command.
	RunInfoExt = ".run"
	// StoppedExt marks a run gate stop stopped, with who did.
	StoppedExt = ".stopped"
)

// RunInfo is what gate runs shows of a detached run: the command (never its
// environment, which carries tokens), its owner, its gate and its start.
type RunInfo struct {
	Command        string      `json:"command"`
	Owner          state.Party `json:"owner,omitzero"`
	Gate           int         `json:"gate,omitempty"`
	Started        time.Time   `json:"started"`
	ProcessStarted time.Time   `json:"processStarted,omitzero"`
}

// ReadRunInfo reads what the run at base left for gate runs, false when it
// left nothing (a run started by an older release).
func ReadRunInfo(base string) (RunInfo, bool) {
	var i RunInfo
	raw, err := os.ReadFile(base + RunInfoExt) //nolint:gosec // the gate's own file
	return i, err == nil && json.Unmarshal(raw, &i) == nil
}
