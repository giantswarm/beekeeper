package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// With agents.dir set, an agent runs there by default, or in a worktree
// under agents.roots, never anywhere else; unset, the caller's folder.
func TestAgentDir(t *testing.T) {
	a, _ := stubApp(t)
	base := t.TempDir()
	lab, wt := filepath.Join(base, "lab"), filepath.Join(base, "worktrees")
	if got, err := a.agentDir(""); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("no agents.dir: %q, %v, want the caller's folder", got, err)
	}
	a.cfg.Agents.Dir, a.cfg.Agents.Roots = lab, []string{wt}
	for _, c := range []struct{ dir, want string }{
		{"", lab},
		{lab, lab},
		{filepath.Join(wt, "beekeeper", "fix"), filepath.Join(wt, "beekeeper", "fix")},
	} {
		if got, err := a.agentDir(c.dir); err != nil || got != c.want {
			t.Errorf("agentDir(%q) = %q, %v, want %q", c.dir, got, err, c.want)
		}
	}
	for _, dir := range []string{filepath.Join(base, "handover"), filepath.Join(lab, "sub"), wt + "-other", "."} {
		if got, err := a.agentDir(dir); err == nil || !strings.Contains(err.Error(), "never elsewhere") {
			t.Errorf("agentDir(%q) = %q, %v, want a refusal", dir, got, err)
		}
	}
}
