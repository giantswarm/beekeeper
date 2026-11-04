package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallPreservesLinkedClaudeSettings(t *testing.T) {
	f := &fakeSetup{}
	e, _ := env(t, t.TempDir(), f, f.run)
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.Spec.Home, "dotfiles", "claude.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.Settings, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.Settings); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(e.Settings); err != nil || got != target {
		t.Errorf("uninstall removed the person's settings link: %q, %v", got, err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || strings.Contains(string(raw), "beekeeper hook") {
		t.Error("uninstall left its own hooks in the linked settings target")
	}
}
