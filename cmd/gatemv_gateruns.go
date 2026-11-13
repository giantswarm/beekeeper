package cmd

import (
	"encoding/json"
	"os"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// runInfoExt is the extension of what gate runs shows of a detached run
	// (runInfo), which its merge-child writes before it starts the command.
	runInfoExt = ".run"
	// stoppedExt marks a run gate stop stopped, with who did.
	stoppedExt = ".stopped"
)

// runInfo is what gate runs shows of a detached run: the command (never its
// environment, which carries tokens), its owner, its gate and its start.
type runInfo struct {
	Command        string      `json:"command"`
	Owner          state.Party `json:"owner,omitzero"`
	Gate           int         `json:"gate,omitempty"`
	Started        time.Time   `json:"started"`
	ProcessStarted time.Time   `json:"processStarted,omitzero"`
}

// readRunInfo reads what the run at base left for gate runs, false when it
// left nothing (a run started by an older release).
func readRunInfo(base string) (runInfo, bool) {
	var i runInfo
	raw, err := os.ReadFile(base + runInfoExt) //nolint:gosec // the gate's own file
	return i, err == nil && json.Unmarshal(raw, &i) == nil
}
