package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// failedLauncher has failed units and records the ones it reset.
type failedLauncher struct {
	platform.Launcher
	failed, patterns, reset []string
}

func (l *failedLauncher) Failed(_ context.Context, patterns ...string) []string {
	l.patterns = patterns
	return l.failed
}

func (l *failedLauncher) ResetFailed(_ context.Context, units ...string) error {
	l.reset = append(l.reset, units...)
	return nil
}

// The doctor clears the failed state of beekeeper's own units and says
// which; a dry run only says them.
func TestDoctorResetsOwnFailedUnits(t *testing.T) {
	orig := plat
	t.Cleanup(func() { plat = orig })
	units := []string{"beekeeper-merge-a.service", "beekeeper-wake-b.service"}
	for name, tc := range map[string]struct {
		dryRun bool
		reset  []string
		line   string
	}{
		"a pass":    {false, units, "reset the failed units beekeeper-merge-a.service, beekeeper-wake-b.service"},
		"a dry run": {true, nil, "would reset the failed units beekeeper-merge-a.service, beekeeper-wake-b.service"},
	} {
		l := &failedLauncher{failed: units}
		plat.Launcher = l
		line := resetOwnUnits(context.Background(), tc.dryRun)
		if !strings.HasPrefix(line, tc.line) || !slices.Equal(l.reset, tc.reset) || !slices.Equal(l.patterns, []string{ownUnits}) {
			t.Errorf("%s: %q, reset %v of %v", name, line, l.reset, l.patterns)
		}
	}
	plat.Launcher = &failedLauncher{}
	if line := resetOwnUnits(context.Background(), false); line != "" {
		t.Errorf("no failed unit: %q", line)
	}
}
