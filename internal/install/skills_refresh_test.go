package install

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/plugin"
)

func TestRefreshSkillsRepairsCompleteMirror(t *testing.T) {
	stateDir := t.TempDir()
	dir := AgySkillsDir(stateDir)
	write(t, filepath.Join(dir, "worker-rules", "SKILL.md"), "outdated")
	write(t, filepath.Join(dir, "custom", "SKILL.md"), "keep")
	if err := CheckSkillMirror(stateDir); err == nil || !strings.Contains(err.Error(), "review") {
		t.Fatalf("missing review was not detected: %v", err)
	}
	if err := RefreshSkills(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := fs.WalkDir(plugin.Skills, plugin.SkillsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		want, err := plugin.Skills.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("mirror differs: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "custom", "SKILL.md"))
	if err != nil || string(raw) != "keep" {
		t.Fatalf("custom skill changed: %q, %v", raw, err)
	}
	// Supporting files are health checked as well as entry points.
	write(t, filepath.Join(dir, "github", "references", "browser-and-security.md"), "outdated")
	if err := CheckSkillMirror(stateDir); err == nil {
		t.Fatal("stale reference passed health check")
	}
	if err := RefreshSkills(stateDir); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSkillsLeavesAbsentMirrorAlone(t *testing.T) {
	stateDir := t.TempDir()
	if err := RefreshSkills(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AgySkillsDir(stateDir)); !os.IsNotExist(err) {
		t.Fatalf("created absent mirror: %v", err)
	}
}
