package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskDefaultsAndValidation(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	p := filepath.Join(base, "config.yaml")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Disk.Auto() || c.Disk.Every.Duration != time.Hour || len(c.Disk.TrashDirs) != 2 || c.Disk.SccacheMaxGiB != 10 {
		t.Fatalf("defaults=%+v", c.Disk)
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"disk: {prune: ask}", true},
		{"disk: {sccacheMaxGiB: -1}", true},
		{"disk: {prune: never}", false},
		{"disk: {trashDirs: [/]} ", false},
		{"disk: {buildDirs: [../source]}", false},
		{"disk: {buildDirs: [.git]}", false},
		{"disk: {buildOutputs: [{path: /scratch, worktree: /scratch/worktree}]}", false},
		{"disk: {buildOutputs: [{path: relative, worktree: /scratch/worktree}]}", false},
		{"disk: {buildDirs: [artifacts], buildOutputs: [{path: /scratch/output, worktree: /scratch/worktree}]}", true},
	} {
		if err := os.WriteFile(p, []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(p)
		if (err == nil) != tc.valid {
			t.Errorf("config %s: %v", tc.raw, err)
		}
		if err == nil && tc.raw == "disk: {prune: ask}" && got.Disk.Auto() {
			t.Fatal("ask became auto")
		}
	}
}
