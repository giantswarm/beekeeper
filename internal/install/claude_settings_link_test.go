package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallPreservesLinkedClaudeSettings(t *testing.T) {
	for _, relative := range []bool{false, true} {
		name := "absolute"
		if relative {
			name = "relative"
		}
		t.Run(name, func(t *testing.T) {
			testUninstallPreservesLinkedClaudeSettings(t, relative)
		})
	}
}

func testUninstallPreservesLinkedClaudeSettings(t *testing.T, relative bool) {
	t.Helper()
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
	link := target
	if relative {
		var err error
		link, err = filepath.Rel(filepath.Dir(e.Settings), target)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(link, e.Settings); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(e.Settings); err != nil || got != link {
		t.Errorf("uninstall removed the person's settings link: %q, %v", got, err)
	}
	raw, err := os.ReadFile(filepath.Clean(target))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "{}" {
		t.Errorf("uninstall target = %q, want an empty settings object", got)
	}
}
