package cmd

import (
	"strings"

	"github.com/giantswarm/beekeeper/pkg/project"
)

// LoopVersionFile is the running loop's version marker, beside the state.
const LoopVersionFile = "supervisor-loop-version.json"

type LoopVersionRecord struct {
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

// LoopRunningVersion returns the binary version of the current process.
var LoopRunningVersion = project.Version

func NormalizeLoopVersion(ver string) string {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return ""
	}
	if !strings.HasPrefix(ver, "v") && ver[0] >= '0' && ver[0] <= '9' {
		return "v" + ver
	}
	return ver
}
