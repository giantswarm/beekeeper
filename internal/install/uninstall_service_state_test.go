package install

import (
	"context"
	"slices"
	"testing"
)

// systemd's Started contract is enabled AND active. A failed unit is still
// enabled, and a manually disabled unit can still be running.
type partialStateSetup struct{ fakeSetup }

func (*partialStateSetup) NeedsStop(context.Context, string) bool { return true }

func TestUninstallStopsPartiallyStartedServices(t *testing.T) {
	f := &partialStateSetup{}
	e, _ := env(t, t.TempDir(), f, f.run)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	f.started, f.ran = false, nil
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.ran, "stop notify.service") {
		t.Errorf("removed unit without disabling or stopping its partial state: %v", f.ran)
	}
}
