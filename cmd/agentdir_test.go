package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// With agents.dir set, an agent runs there by default, or in a worktree
// under agents.roots, never anywhere else; unset, the caller's folder.
func TestAgentDir(t *testing.T) {
	a, _ := stubApp(t)
	base := t.TempDir()
	lab, wt := filepath.Join(base, "lab"), filepath.Join(base, "worktrees")
	if got, err := a.agentDir("", ""); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("no agents.dir: %q, %v, want the caller's folder", got, err)
	}
	a.cfg.Agents.Dir, a.cfg.Agents.Roots = lab, []string{wt}
	for _, c := range []struct{ dir, want string }{
		{"", lab},
		{lab, lab},
		{filepath.Join(wt, "beekeeper", "fix"), filepath.Join(wt, "beekeeper", "fix")},
	} {
		if got, err := a.agentDir(c.dir, ""); err != nil || got != c.want {
			t.Errorf("agentDir(%q) = %q, %v, want %q", c.dir, got, err, c.want)
		}
	}
	for _, dir := range []string{filepath.Join(base, "handover"), filepath.Join(lab, "sub"), wt + "-other", "."} {
		if got, err := a.agentDir(dir, ""); err == nil || !strings.Contains(err.Error(), "never elsewhere") {
			t.Errorf("agentDir(%q) = %q, %v, want a refusal", dir, got, err)
		}
	}
}

// A role may use exactly its own configured folder outside the worker roots.
func TestAgentDirRoleFolders(t *testing.T) {
	a, _ := stubApp(t)
	base := t.TempDir()
	a.cfg.Agents.Dir = filepath.Join(base, "workers")
	a.cfg.Agents.Roots = []string{filepath.Join(base, "worktrees")}
	a.cfg.Guide.Dir = filepath.Join(a.cfg.Agents.Dir, "desk")
	a.cfg.Guide.Dir = filepath.Join(base, "supervision")
	for _, rl := range roles {
		t.Run(rl.name, func(t *testing.T) {
			dir := rl.cfg(a.cfg).Dir
			got, err := a.agentDir(dir, rl.name)
			if err != nil || got != dir {
				t.Fatalf("role folder: %q, %v, want %q", got, err, dir)
			}
			for _, name := range []string{"", workerRole, kindUnknown} {
				if _, err := a.agentDir(dir, name); err == nil {
					t.Errorf("role folder accepted for %q", name)
				}
			}
			for _, other := range roles {
				if other.name != rl.name {
					if _, err := a.agentDir(dir, other.name); err == nil {
						t.Errorf("role folder accepted for %s", other.name)
					}
				}
			}
			for _, bad := range []string{filepath.Join(dir, "child"), dir + "-other"} {
				if _, err := a.agentDir(bad, rl.name); err == nil {
					t.Errorf("role accepted unrelated folder %q", bad)
				}
			}
			if got, err := a.agentDir("", rl.name); err != nil || got != a.cfg.Agents.Dir {
				t.Errorf("default changed: %q, %v", got, err)
			}
		})
	}
}

// The relay launches each successor in its role folder, outside worker roots.
func TestSuccessorStartsInRoleFolder(t *testing.T) {
	for _, rl := range roles {
		t.Run(rl.name, func(t *testing.T) {
			a, _ := stubApp(t)
			plat.Machine = tableMachine{plat.Machine}
			a.cfg.Agents.Mode = config.ModeBackground
			a.cfg.Agents.Model = bgModel
			a.cfg.Agents.Dir = t.TempDir()
			a.cfg.Agents.Roots = []string{t.TempDir()}
			dir := t.TempDir()
			if rl.name == guideName {
				a.cfg.Guide.Dir = dir
			} else {
				a.cfg.Guide.Dir = dir
			}
			name := rl.runName(2)
			calls := stubBackground(t, "backgrounded · "+bgShortID+" · "+name+"\n", bgSession{ID: bgShortID, SessionID: bgID, Name: name})
			from := state.Party{Session: "predecessor", Name: rl.runName(1)}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				rl.set(st, state.Role{Holder: &state.Supervisor{Party: from, Since: a.now}, Run: 1})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			to, _, err := a.startSuccessor(t.Context(), rl, from, from, "")
			if err != nil {
				t.Fatal(err)
			}
			if to.Name != name || len(*calls) != 1 || (*calls)[0].dir != dir {
				t.Fatalf("successor %+v, launches %+v, want folder %q", to, *calls, dir)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if r := rl.get(st); r.Relay == nil || !r.Relay.To.Is(to) {
				t.Fatalf("successor relay lost: %+v", r)
			}
			if _, err := a.startAgent(t.Context(), agentStart{name: workerRole, role: workerRole, dir: dir, by: &from}); err == nil {
				t.Fatal("worker launched in the role folder")
			}
			if len(*calls) != 1 {
				t.Fatal("refused worker reached the launcher")
			}
		})
	}
}
