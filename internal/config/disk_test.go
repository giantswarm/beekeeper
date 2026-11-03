package config

import (
	"os"
	"path/filepath"
	"strings"
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
		{"disk: {caches: [{path: ~/.cache/app, olderThan: 7d}, {path: '/cache/*-dev', maxGiB: 1.5, mode: files}]}", true},
		{"disk: {caches: [{name: go-build, maxGiB: 5}, {name: sccache, mode: off}, {name: beekeeper-tmp, olderThan: 2h}]}", true},
		{"disk: {caches: [{path: /cache/app}]}", false},
		{"disk: {caches: [{path: relative, olderThan: 1h}]}", false},
		{"disk: {caches: [{path: /, olderThan: 1h}]}", false},
		{"disk: {caches: [{path: '/cache/[', olderThan: 1h}]}", false},
		{"disk: {caches: [{path: /cache/app, olderThan: 1h, mode: go}]}", false},
		{"disk: {caches: [{name: sccache, mode: go}]}", false},
		{"disk: {caches: [{path: /cache/app, olderThan: 1h, mode: wipe}]}", false},
		{"disk: {caches: [{path: /cache/app, maxGiB: -1}]}", false},
		{"disk: {caches: [{name: app}]}", false},
		{"disk: {caches: [{path: ~/.cache/uv, mode: command, command: uv cache prune, maxGiB: 2}]}", true},
		{"disk: {caches: [{path: /cache/tool, mode: command}]}", false},
		{"disk: {caches: [{path: /cache/tool, olderThan: 1h, command: tool clean}]}", false},
		{"disk: {caches: [{path: /cache/a, olderThan: 1h}, {path: /cache/a, maxGiB: 1}]}", false},
	} {
		if err := os.WriteFile(p, []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := Load(p)
		if (err == nil) != tc.valid {
			t.Errorf("config %s: %v", tc.raw, err)
		}
		if err == nil && strings.Contains(tc.raw, "~/.cache/app") {
			if e := got.Disk.Caches[0]; e.Name != e.Path || !filepath.IsAbs(e.Path) || e.OlderThan.Duration != 7*24*time.Hour {
				t.Fatalf("cache entry=%+v", e)
			}
		}
		if err == nil && tc.raw == "disk: {prune: ask}" && got.Disk.Auto() {
			t.Fatal("ask became auto")
		}
	}
}
