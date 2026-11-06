package loopworkflow

import (
	"os"
	"testing"
)

// The test executable doubles only as the external GitHub CLI, Git tracing
// wrapper, or sandbox Claude harness. Beekeeper itself is a separate binary.
func TestMain(m *testing.M) {
	if code, served := Serve(); served {
		os.Exit(code)
	}
	if code, served := serveTestCandidate(); served {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
