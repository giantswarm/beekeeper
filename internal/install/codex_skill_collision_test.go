package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestCodexInstallKeepsExistingSkills(t *testing.T) {
	const (
		skillDirectory = "directory"
		skillSymlink   = "symlink"
	)
	for _, kind := range []string{skillDirectory, "file", skillSymlink} {
		t.Run(kind, func(t *testing.T) {
			e, out := env(t, t.TempDir(), platform.Stub().Setup, nil)
			e.CodexSkills = filepath.Join(e.Spec.Home, ".codex", "skills")
			dst := filepath.Join(e.CodexSkills, "park")
			target := filepath.Join(e.Spec.Home, "custom", "SKILL.md")
			switch kind {
			case skillDirectory:
				write(t, filepath.Join(dst, "SKILL.md"), "user's park instructions\n")
			case "file":
				write(t, dst, "user's park instructions\n")
			case skillSymlink:
				write(t, target, "user's park instructions\n")
				if err := os.MkdirAll(e.CodexSkills, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dst); err != nil {
					t.Fatal(err)
				}
			}
			before := tree(t, e.CodexSkills)
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			checkPreserved := func() {
				t.Helper()
				for path, want := range before {
					if strings.HasSuffix(path, "/") {
						continue
					}
					raw, err := os.ReadFile(filepath.Clean(path))
					if err != nil || string(raw) != want {
						t.Errorf("existing skill %s was replaced: %v", path, err)
					}
				}
				if kind == skillDirectory && symlink(dst) {
					t.Error("existing skill directory became a link")
				}
				if kind == skillSymlink {
					if got, err := os.Readlink(dst); err != nil || got != target {
						t.Errorf("existing skill link changed: %q, %v", got, err)
					}
				}
			}
			checkPreserved()
			if !strings.Contains(out.String(), "an existing skill install did not write") {
				t.Error("install did not report the collision")
			}

			m, err := readManifest(e.manifest())
			if err != nil {
				t.Fatal(err)
			}
			for _, link := range m.CodexSkillLinks {
				if link == dst {
					t.Error("installer claimed the user's existing skill")
				}
			}
			// Reinstall must keep excluding the collision from ownership.
			if err := Install(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			checkPreserved()
			if err := Uninstall(context.Background(), e, false); err != nil {
				t.Fatal(err)
			}
			checkPreserved()
		})
	}
}
