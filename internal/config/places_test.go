package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlacesDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BEEKEEPER_EXECUTION_HOME", "")
	for _, tc := range []struct {
		yaml, dir                string
		worktrees, places, roots []string
	}{
		{"", filepath.Join(home, "projects"), []string{filepath.Join(home, "projects/worktrees")}, []string{filepath.Join(home, ".local/state")}, []string{filepath.Join(home, "projects/worktrees")}},
		{"projects:\n  dir: ~/clones\n  worktrees: [~/sessions]\nhome:\n  places: [~/state]\n", filepath.Join(home, "clones"), []string{filepath.Join(home, "sessions")}, []string{filepath.Join(home, "state")}, []string{filepath.Join(home, "sessions")}},
		{"projects:\n  dir: ~/clones\nagents:\n  roots: [/tmp/agents]\n", filepath.Join(home, "clones"), []string{filepath.Join(home, "clones/worktrees")}, []string{filepath.Join(home, ".local/state")}, []string{"/tmp/agents"}},
	} {
		c, err := parse("test", []byte(tc.yaml))
		if err != nil {
			t.Fatal(err)
		}
		if c.Projects.Dir != tc.dir || !reflect.DeepEqual(c.Projects.Worktrees, tc.worktrees) || !reflect.DeepEqual(c.Home.Places, tc.places) || !reflect.DeepEqual(c.Agents.Roots, tc.roots) {
			t.Errorf("unexpected places: %+v %+v roots %v", c.Projects, c.Home, c.Agents.Roots)
		}
	}
}
