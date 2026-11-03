package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveTree lays out a worktree root with a live worker's worktree, a
// finished one and a plain folder: root/worktrees/<repo>/<branch>.
type liveTree struct {
	root, wts, live, finished, cwd string
}

func newLiveTree(t *testing.T) liveTree {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lt := liveTree{root: root, wts: filepath.Join(root, "worktrees")}
	lt.live = filepath.Join(lt.wts, "backstage", "feat-2867")
	lt.finished = filepath.Join(lt.wts, "bwi", "done")
	lt.cwd = filepath.Join(lt.live, "plugins", "agent-platform")
	for _, wt := range []string{lt.live, lt.finished} {
		if err := os.MkdirAll(filepath.Join(wt, "node_modules"), 0o750); err != nil {
			t.Fatal(err)
		}
		gitfile := "gitdir: " + filepath.Join(root, "repo", ".git", "worktrees", filepath.Base(wt)) + "\n"
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(gitfile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{lt.cwd, filepath.Join(root, "plain", "sub")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(lt.live, "README.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return lt
}

// TestLiveWorktreeGuardRefusesMovingAFolderOfLiveWorktrees: a move, trash or
// delete of a folder that is or holds a worktree a live process works in is
// refused, however the folder is named; the worktree trim's own removals
// go through worktrees.Decide, which keeps a live one.
func TestLiveWorktreeGuardRefusesMovingAFolderOfLiveWorktrees(t *testing.T) {
	lt := newLiveTree(t)
	h := Hook{LiveDirs: func() []string { return []string{lt.cwd, filepath.Join(lt.root, "plain", "sub")} }}
	refused := []string{
		"mv " + lt.wts + " " + filepath.Join(lt.root, "aside"),
		"gio trash " + lt.wts,
		"trash-put " + lt.wts,
		"trash " + lt.live,
		"rm -rf " + lt.wts,
		"rmdir " + lt.live,
		"mv -t /elsewhere " + lt.wts,
		"mv " + filepath.Join(lt.wts, "*") + " /elsewhere/",
		"gio move " + lt.wts + " /elsewhere",
		"gio rename " + lt.live + " other",
		"git worktree remove --force " + lt.live,
		"git -C " + lt.root + " worktree remove worktrees/backstage/feat-2867",
		"cd " + lt.root + " && mv worktrees aside",
		"D=" + lt.wts + "; rm -rf \"${D:?}\"",
		"bash -c 'gio trash " + lt.wts + "'",
		"sudo mv " + lt.wts + " /elsewhere",
		"cd -P " + lt.root + " && mv worktrees aside",
		"mv -vt /elsewhere " + lt.wts,
		"mv -t/elsewhere " + lt.wts,
		"mv --target-directory /elsewhere " + lt.wts,
		"mv " + lt.wts + " /elsewhere 2>/dev/null",
		"bash <<'EOF'\nmv " + lt.wts + " /aside\nEOF",
		"for d in " + filepath.Join(lt.wts, "*") + "; do rm -rf \"$d\"; done",
		"gio trash file://" + lt.wts,
		"cd " + lt.root + " || exit 1; gio trash worktrees",
	}
	for _, cmd := range refused {
		r := h.liveWorktreeRefusal(cmd, lt.root)
		if !strings.Contains(r, "Refused") || !strings.Contains(r, lt.live) {
			t.Errorf("%q: want a refusal naming %s, got %q", cmd, lt.live, r)
		}
	}
	passed := []string{
		"mv " + lt.finished + " " + filepath.Join(lt.root, "aside"),
		"rm -rf " + filepath.Join(lt.finished, "node_modules"),
		"rm -rf " + filepath.Join(lt.live, "node_modules"),
		"mv " + filepath.Join(lt.live, "README.md") + " " + filepath.Join(lt.live, "DOCS.md"),
		"mv " + filepath.Join(lt.root, "plain") + " " + filepath.Join(lt.root, "aside"),
		"gio trash --empty",
		"mv " + lt.wts,
		"rm -rf \"$(mktemp -d)\"",
		"mv \"$UNSET_DIR_1541\" /elsewhere",
		"ls " + lt.wts,
		// a redirection is no operand: a move into a live worktree passes
		"mv " + filepath.Join(lt.live, "README.md") + " " + lt.wts + " 2>/dev/null",
		"mv " + filepath.Join(lt.live, "README.md") + " " + lt.wts + " > /dev/null",
		"mv " + filepath.Join(lt.live, "README.md") + " " + lt.wts + " 2>&1",
		"WT=" + lt.live + "; mv patch.diff \"$WT\" 2>/dev/null",
		"mv -f " + filepath.Join(lt.live, "README.md") + " " + lt.wts + "/",
		// where the shell stands is unknown after cd - or cd $(…): nothing resolves
		"cd " + lt.wts + " && cd - && mv backstage aside",
		"cd \"$(mktemp -d)\" && mv worktrees aside",
		"for d in $(ls); do rm -rf \"$d\"; done",
	}
	for _, cmd := range passed {
		if r := h.liveWorktreeRefusal(cmd, lt.root); r != "" {
			t.Errorf("%q: want it to pass, got %q", cmd, r)
		}
	}
}

// TestLiveWorktreeGuardPassesWithoutLiveSessions: once the session ended,
// its folder of worktrees moves; without LiveDirs the guard is off.
func TestLiveWorktreeGuardPassesWithoutLiveSessions(t *testing.T) {
	lt := newLiveTree(t)
	cmd := "gio trash " + lt.wts
	if r := (Hook{LiveDirs: func() []string { return nil }}).liveWorktreeRefusal(cmd, lt.root); r != "" {
		t.Errorf("no live session: want a pass, got %q", r)
	}
	if r := (Hook{}).liveWorktreeRefusal(cmd, lt.root); r != "" {
		t.Errorf("no LiveDirs: want a pass, got %q", r)
	}
}

// TestLiveWorktreeGuardReadsLiveDirsOnlyForAMovedDirectory: the live
// folders (a process table read) are read only for a command that moves or
// removes an existing directory.
func TestLiveWorktreeGuardReadsLiveDirsOnlyForAMovedDirectory(t *testing.T) {
	lt := newLiveTree(t)
	reads := 0
	h := Hook{LiveDirs: func() []string { reads++; return []string{lt.cwd} }}
	for _, cmd := range []string{"go vet ./...", "rm -f " + filepath.Join(lt.live, "README.md"), "mv /nonexistent-1541 /x"} {
		_ = h.liveWorktreeRefusal(cmd, lt.root)
	}
	if reads != 0 {
		t.Errorf("live folders read %d times for commands that move no directory", reads)
	}
}

// TestHookRefusesTrashingLiveWorktrees: the hook answers a Bash call that
// trashes the folder of live worktrees with a deny.
func TestHookRefusesTrashingLiveWorktrees(t *testing.T) {
	lt := newLiveTree(t)
	h := Hook{Self: self, Shell: testShell, LiveDirs: func() []string { return []string{lt.cwd} }}
	if d := decide(t, h, lt.root, "gio trash "+lt.wts, nil); !denied(d) {
		t.Fatalf("want a deny, got %+v", d)
	}
	if d := decide(t, h, lt.root, "gio trash "+lt.finished, nil); denied(d) {
		t.Fatalf("a finished worktree: want a pass, got %+v", d)
	}
}
