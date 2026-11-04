//go:build linux && !nosystemd

package platform

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupNeedsStopIncludesPartialStates(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
case "$2:$3" in
is-enabled:enabled-*.service) printf 'enabled\n' ;;
is-active:*-active.service) printf 'active\n' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, systemctl), []byte(script), 0o700); err != nil { //nolint:gosec // the isolated systemctl fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	setup := systemdSetup{}
	for _, tc := range []struct {
		unit string
		want bool
	}{
		{"enabled-active.service", true},
		{"enabled-inactive.service", true},
		{"disabled-active.service", true},
		{"disabled-inactive.service", false},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			if got := setup.NeedsStop(context.Background(), tc.unit); got != tc.want {
				t.Errorf("NeedsStop = %v, want %v", got, tc.want)
			}
			if got := setup.Started(context.Background(), tc.unit); got != (tc.unit == "enabled-active.service") {
				t.Error("fixture did not preserve the Started contract")
			}
		})
	}
}
