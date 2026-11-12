package cmd

import (
	"strings"

	"github.com/giantswarm/beekeeper/pkg/project"
)

// loopVersionFile is the running loop's version marker, beside the state.
const loopVersionFile = "supervisor-loop-version.json"

type loopVersionRecord struct {
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

// loopRunningVersion returns the binary version of the current process.
var loopRunningVersion = project.Version

func normalizeLoopVersion(ver string) string {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return ""
	}
	if !strings.HasPrefix(ver, "v") && ver[0] >= '0' && ver[0] <= '9' {
		return "v" + ver
	}
	return ver
}
