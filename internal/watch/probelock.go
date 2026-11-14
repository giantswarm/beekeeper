package watch

import (
	"strings"
)

func isProbeInProgress(msg string) bool {
	return strings.HasPrefix(msg, probeInProgressPrefix)
}
