package install

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexInstallKeepsExistingSkills(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink", "dangling", "matching"} {
		for _, replacement := range []bool{false, true} {
			for _, purge := range []bool{false, true} {
				t.Run(kind+map[bool]string{false: "/existing", true: "/replacement"}[replacement]+map[bool]string{false: "/uninstall", true: "/purge"}[purge], func(t *testing.T) {
					home := t.TempDir()
					f := &fakeSetup{}
					e, out := env(t, home, f, f.run)
					e.CodexSkills = filepath.Join(home, ".codex", "skills")
					dst := filepath.Join(e.CodexSkills, "park")
					if replacement {
						if err := Install(context.Background(), e); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(dst); err != nil {
							t.Fatal(err)
						}
					}
					target := filepath.Join(home, "personal-skill")
					write(t, filepath.Join(target, "SKILL.md"), "personal skill\n")
					if err := os.MkdirAll(e.CodexSkills, 0o750); err != nil {
						t.Fatal(err)
					}
					switch kind {
					case "directory":
						write(t, filepath.Join(dst, "SKILL.md"), "personal skill\n")
					case "file":
						write(t, dst, "personal skill\n")
					default:
						if kind == "dangling" {
							target = filepath.Join(home, "missing")
						}
						if kind == "matching" {
							target = filepath.Join(AgySkillsDir(e.StateDir), "park")
						}
						if err := os.Symlink(target, dst); err != nil {
							t.Fatal(err)
						}
					}
					check := func() {
						t.Helper()
						switch kind {
						case "directory", "file":
							path := dst
							if kind == "directory" {
								path = filepath.Join(dst, "SKILL.md")
							}
							got, err := os.ReadFile(path)
							if err != nil || string(got) != "personal skill\n" {
								t.Fatalf("personal content changed: %q, %v", got, err)
							}
						default:
							got, err := os.Readlink(dst)
							if err != nil || got != target {
								t.Fatalf("personal link changed: %q, %v", got, err)
							}
						}
					}
					// A matching replacement is indistinguishable from the recorded link.
					owned := replacement && kind == "matching"
					if !owned {
						e.DryRun = true
						if err := Install(context.Background(), e); err != nil {
							t.Fatal(err)
						}
						check()
						e.DryRun = false
					}
					out.Reset()
					for range 2 {
						if err := Install(context.Background(), e); err != nil {
							t.Fatal(err)
						}
						check()
						m, err := readManifest(e.manifest())
						if err != nil {
							t.Fatal(err)
						}
						if !owned && slices.Contains(m.CodexSkillLinks, dst) {
							t.Fatal("claimed personal skill")
						}
					}
					if !owned && !strings.Contains(out.String(), "collision") {
						t.Fatal("collision was not reported")
					}
					if err := Uninstall(context.Background(), e, purge); err != nil {
						t.Fatal(err)
					}
					if !owned {
						check()
					}
				})
			}
		}
	}
}

func TestCodexUninstallKeepsReplacedSkillLink(t *testing.T) {
	home := t.TempDir()
	f := &fakeSetup{}
	e, _ := env(t, home, f, f.run)
	e.CodexSkills = filepath.Join(home, ".codex", "skills")
	if err := Install(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.CodexSkills, "park")
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "personal")
	write(t, target, "personal")
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(dst); err != nil || got != target {
		t.Fatalf("replacement link removed: %q, %v", got, err)
	}
}
