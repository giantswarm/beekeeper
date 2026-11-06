//go:build integration

package loopworkflow

import (
	"os"
	"testing"
)

// The test executable doubles only as the external GitHub CLI and a Git
// tracing wrapper. Beekeeper itself is always a separately built binary.
func TestMain(m *testing.M) {
	if code, served := Serve(); served {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
