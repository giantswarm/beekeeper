package cmd

import (
	"strings"
)

func isProbeInProgress(msg string) bool {
	return strings.HasPrefix(msg, probeInProgressPrefix)
}
