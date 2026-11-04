package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestCodexUninstallKeepsRepointedLinks(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "targets recorded"
		if legacy {
			name = "legacy manifest"
		}
		t.Run(name, func(t *testing.T) { testCodexUninstallKeepsRepointedLinks(t, legacy) })
	}
}

func testCodexUninstallKeepsRepointedLinks(t *testing.T, legacy bool) {
	t.Helper()
	e, _ := env(t, t.TempDir(), platform.Stub().Setup, nil)
	e.CodexSkills = filepath.Join(e.Spec.Home, ".codex", "skills")
	write(t, filepath.Join(e.Spec.Home, ".codex", "AGENTS.md"), "standing instructions\n")
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if legacy {
		m, err := readManifest(e.manifest())
		if err != nil {
			t.Fatal(err)
		}
		m.CodexSkillTargets = nil
		if err := writeFile(e.manifest(), m.encode()); err != nil {
			t.Fatal(err)
		}
	}
	links := []string{filepath.Join(e.CodexSkills, "park"), CodexWorkerSkills(e.StateDir), filepath.Join(e.StateDir, "codex", "home", "AGENTS.md")}
	target := filepath.Join(e.Spec.Home, "replacement")
	write(t, target, "user's replacement\n")
	for _, link := range links {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if got, err := os.Readlink(link); err != nil || got != target {
			t.Errorf("uninstall removed changed link %s: %q, %v", link, got, err)
		}
	}
}
