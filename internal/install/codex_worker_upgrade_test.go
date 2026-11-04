package install

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestCodexHookUpgradeUpdatesWorker(t *testing.T) {
	e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
	e.CodexHooks = filepath.Join(e.Spec.Home, ".codex", "hooks.json")
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	e.Exe, e.Spec.Exe = replacementExe, replacementExe
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{e.CodexHooks, CodexWorkerHooks(e.StateDir)} {
		if got := CheckCodexHooks(path, e.Exe, e.Spec.Home); got != "" {
			t.Error(got)
		}
	}
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if exists(CodexWorkerHooks(e.StateDir)) {
		t.Error("uninstall left the worker's obsolete guard")
	}
}
