package cmd

import (
	"errors"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// needsPlatform skips a test that drives the machine itself (its memory,
// processes, units or links) on a build whose platform is the stub: macOS,
// or Linux built with the nosystemd tag.
func needsPlatform(t *testing.T) {
	t.Helper()
	if _, err := plat.Machine.Mem(); errors.Is(err, platform.ErrNotAvailable) {
		t.Skip(err)
	}
}
