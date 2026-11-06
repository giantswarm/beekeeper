package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// idleRecord writes an unarchived desktop record of cli working in cwd, last
// active at seen.
func idleRecord(t *testing.T, dir, host, cli, cwd string, seen time.Time) {
	t.Helper()
	raw := fmt.Sprintf(`{"sessionId":%q,"cliSessionId":%q,"cwd":%q,"isArchived":false,"lastActivityAt":%d}`, host, cli, cwd, seen.UnixMilli())
	writeT(t, filepath.Join(dir, "a", "b", host+".json"), raw)
}

func TestIdleDesktopSessionKeepsNoWorktreeLive(t *testing.T) {
	t.Run("worktree root", func(t *testing.T) { idleSessions(t, "") })
	t.Run("worktree subdirectory", func(t *testing.T) { idleSessions(t, "cmd") })
}

// idleSessions runs the desktop sessions in sub of each worktree.
func idleSessions(t *testing.T, sub string) {
	for _, k := range []string{litGitAuthorEmail, litGitCommitterEmail} {
		t.Setenv(k, "test@example.com")
	}
	base := t.TempDir()
	bare, clone := filepath.Join(base, "bare.git"), filepath.Join(base, "clone")
	gitT(t, base, "init", "-q", "--bare", bare)
	gitT(t, base, "init", "-q", "-b", "main", clone)
	writeT(t, filepath.Join(clone, "cmd", "a.txt"), "a\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-q", "-m", "init", "--author", "T <test@example.com>")
	gitT(t, clone, "remote", "add", "local", bare)
	gitT(t, clone, "push", "-q", "local", "main")
	tree := func(name string) string {
		dir := filepath.Join(base, "worktrees", name)
		gitT(t, clone, "worktree", "add", "-q", "-b", name, dir, "main")
		return dir
	}
	saved, dirty, unpushed, recent, parked := tree("saved"), tree("dirty"), tree("unpushed"), tree("recent"), tree("parked")
	writeT(t, filepath.Join(dirty, "note.txt"), "work\n")
	writeT(t, filepath.Join(unpushed, "b.txt"), "b\n")
	gitT(t, unpushed, "add", ".")
	gitT(t, unpushed, "commit", "-q", "-m", "local", "--author", "T <test@example.com>")

	a, _ := stubApp(t)
	plat.Machine = orphanProcMachine{plat.Machine}
	a.cfg.Claude.DesktopDir = filepath.Join(base, "desktop")
	now := time.Now()
	old := now.Add(-worktrees.SessionIdle - time.Hour)
	for i, d := range []string{saved, dirty, unpushed, parked} {
		idleRecord(t, a.cfg.Claude.DesktopDir, fmt.Sprintf("local_old%d", i), fmt.Sprintf("cli-old%d", i), filepath.Join(d, sub), old)
	}
	idleRecord(t, a.cfg.Claude.DesktopDir, "local_recent", "cli-recent", filepath.Join(recent, sub), now.Add(-time.Hour))
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		worker := state.Party{Session: "cli-old3", Name: "parked worker"}
		st.Starts = append(st.Starts, state.Start{Party: worker, Dir: parked, At: old})
		st.Agents = append(st.Agents, state.Agent{Party: worker, Registered: old, Task: "open task"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	live, err := a.liveFolders(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(saved, sub), filepath.Join(dirty, sub), filepath.Join(unpushed, sub)} {
		if slices.Contains(live, d) {
			t.Errorf("an idle desktop session alone kept %s live", d)
		}
	}
	for _, d := range []string{filepath.Join(recent, sub), parked} {
		if !slices.Contains(live, d) {
			t.Errorf("%s lost its session or its unfinished task", d)
		}
	}

	unsaved := unsavedSessionDirs(a.cfg, now)
	slices.Sort(unsaved)
	if want := []string{dirty, unpushed}; !slices.Equal(unsaved, want) {
		t.Errorf("unsaved = %v, want %v: a mv or rm stays refused only where work would be lost", unsaved, want)
	}
	hook := a.hookLiveDirs()
	if wt, _ := guard.LiveWorktreeUnder(dirty, hook); wt == "" {
		t.Errorf("rm of a worktree with unsaved work passes the hook: %v", hook)
	}
	if wt, _ := guard.LiveWorktreeUnder(saved, hook); wt != "" {
		t.Errorf("the saved worktree of an idle session stays refused: %s", wt)
	}

	// A transcript written since the record's last activity counts as activity.
	transcript := filepath.Join(a.cfg.Claude.ProjectsDir, "project", "cli-old0.jsonl")
	writeT(t, transcript, "{}\n")
	if err := os.Chtimes(transcript, now, now); err != nil {
		t.Fatal(err)
	}
	if live, err = a.liveFolders(now); err != nil || !slices.Contains(live, filepath.Join(saved, sub)) {
		t.Errorf("a session with a fresh transcript is not live: %v, %v", live, err)
	}
}
