package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// mergedPR is the worktree whose pull request merged, holding only build
// output.
const mergedPR = "mergedpr"

const removeActive = "active"

const removeOther = "other"

const removeReview = "review"

const removeTask = "task"

const removeDesktop = "desktop"

const removeParked = "parked"

const removeUnused = "unused"
const cleanupUpstream = "upstream"

// gitT runs git in dir for a test.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput() //nolint:gosec // the test's own git calls
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// worktreeApp is an app and, seven hours on, a machine with one clone of
// acme/app and its worktrees under a root:
//
//	mergedpr  pull request merged, only build output (node_modules, an ignored log)
//	work      merged, an untracked note beside its node_modules
//	unpushed  merged, a commit neither a remote branch nor the pull request has
//	open      pull request open
//	nopr      no pull request
//	live      merged, a process works in it
//	recent    merged an hour ago
//	pushedas  merged, its branch wt/pushedas pushed as pushedas
func worktreeApp(t *testing.T) (*app, func() (worktreeMachine, error), string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{litGitAuthorName, "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{litGitAuthorEmail, "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
	base := t.TempDir()
	bare, clone, root := filepath.Join(base, "bare.git"), filepath.Join(base, "clone"), filepath.Join(base, "worktrees")
	gitT(t, base, "init", "-q", "--bare", bare)
	gitT(t, base, "init", "-q", "-b", "main", clone)
	writeT(t, filepath.Join(clone, ".gitignore"), "*.log\n")
	gitT(t, clone, "add", ".")
	gitT(t, clone, "commit", "-q", "-m", "init")
	gitT(t, clone, "remote", "add", workerGitHubOrigin, "https://github.com/acme/app.git")
	gitT(t, clone, "remote", "add", "local", bare)
	gitT(t, clone, "push", "-q", "local", "main")
	for _, b := range []string{mergedPR, "work", litUnpushed, "open", "nopr", "live", "recent"} {
		gitT(t, clone, "worktree", "add", "-q", "-b", b, filepath.Join(root, "app", b), "main")
		writeT(t, filepath.Join(root, "app", b, "node_modules", "pkg", "index.js"), strings.Repeat("x", 4096))
	}
	// pushed as another branch than the one checked out
	gitT(t, clone, "worktree", "add", "-q", "-b", "wt/pushedas", filepath.Join(root, "app", "pushedas"), "main")
	gitT(t, clone, "update-ref", "refs/remotes/origin/pushedas", "main")
	gitT(t, clone, "branch", "--set-upstream-to=origin/pushedas", "wt/pushedas")
	writeT(t, filepath.Join(root, "app", mergedPR, "debug.log"), "log")
	writeT(t, filepath.Join(root, "app", "work", "notes.txt"), "mine")
	writeT(t, filepath.Join(root, "app", "unpushed", "change.go"), "package x")
	gitT(t, filepath.Join(root, "app", "unpushed"), "add", "change.go")
	gitT(t, filepath.Join(root, "app", "unpushed"), "commit", "-q", "-m", "more")
	head := gitT(t, clone, "rev-parse", "main")

	now := time.Now().Add(7 * time.Hour)
	long := now.Add(-24 * time.Hour)
	pulls := map[string]github.HeadPull{
		mergedPR:   {N: 1, State: github.Merged, HeadOid: head, ClosedAt: long},
		"work":     {N: 2, State: github.Merged, HeadOid: head, ClosedAt: long},
		"unpushed": {N: 3, State: github.Merged, HeadOid: head, ClosedAt: long},
		"open":     {N: 4, State: github.Open, HeadOid: head},
		"live":     {N: 5, State: github.Closed, HeadOid: head, ClosedAt: long},
		"pushedas": {N: 7, State: github.Merged, HeadOid: head, ClosedAt: long},
		"recent":   {N: 6, State: github.Merged, HeadOid: head, ClosedAt: now.Add(-time.Hour)},
	}
	machine := func() (worktreeMachine, error) {
		return worktreeMachine{
			dirs:  worktrees.Find([]string{root}),
			roots: []string{root},
			live:  []string{filepath.Join(root, "app", "live", "node_modules")},
			git:   runGit,
			pulls: func(_ context.Context, heads []github.Head) (map[github.Head]github.HeadPull, error) {
				out := map[github.Head]github.HeadPull{}
				for _, h := range heads {
					if p, ok := pulls[h.Branch]; ok && h.Repo == "acme/app" {
						out[h] = p
					}
				}
				return out, nil
			},
			now: now,
		}, nil
	}
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, Doctor: config.Doctor{WorktreesEvery: config.Duration{Duration: 6 * time.Hour}}}
	return &app{cfg: cfg, store: store, now: now, out: &bytes.Buffer{}}, machine, filepath.Join(root, "app")
}

func TestTrimWorktreesRemovesTheFinishedAndKeepsWork(t *testing.T) {
	a, machine, app := worktreeApp(t)
	r := a.trimWorktrees(false, machine)
	if r.err != nil || len(r.failed) > 0 {
		t.Fatalf("run failed: %v %+v", r.err, r.failed)
	}
	for _, c := range []struct {
		path string
		kept bool
	}{
		{mergedPR, false},
		{"pushedas", false},
		{"work/notes.txt", true},
		{"work/node_modules", false},
		{"unpushed/change.go", true},
		{"unpushed/node_modules", false},
		{"open/node_modules", false},
		{"nopr/node_modules", false},
		{"live/node_modules", true},
		{"recent/node_modules", false},
	} {
		if exists(filepath.Join(app, c.path)) != c.kept {
			t.Errorf("%s kept = %v, want %v", c.path, !c.kept, c.kept)
		}
	}
	if len(r.removed) != 2 || len(r.cleaned) != 2 || len(r.idle) != 3 || r.kept[worktrees.KeptWork] != 0 {
		t.Errorf("removed %d, cleaned %d, idle %d, kept %v; want 2, 2, 3, none holding work: %s", len(r.removed), len(r.cleaned), len(r.idle), r.kept, r)
	}
	if s := r.String(); !strings.HasPrefix(s, "worktrees: removed 2 finished, ") ||
		!strings.Contains(s, "; removed the build output of 2 finished ones holding work, ") ||
		!strings.Contains(s, "; removed the build output of 3 unfinished ones untouched for 6h, ") || !strings.Contains(s, " in all") {
		t.Errorf("run = %q", r)
	}
	for _, want := range []string{"  removed ", "  removed the build output of "} {
		if !slices.ContainsFunc(r.paths(), func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("no path line says %q: %q", want, r.paths())
		}
	}
	if slices.ContainsFunc(r.paths(), func(l string) bool { return strings.Contains(l, "would") }) {
		t.Errorf("an applied run's path lines say would: %q", r.paths())
	}
	if list := gitT(t, filepath.Join(app, "work"), "worktree", "list"); strings.Contains(list, filepath.Join(app, mergedPR)) {
		t.Errorf("git still lists the removed worktree:\n%s", list)
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == worktreesVerb })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, filepath.Join(app, mergedPR)) {
		t.Errorf("logged %+v, %v; want one %s event naming the removed path", evs, err, worktreesVerb)
	}
}

// readOnlyT puts under dir a read-only file in a read-only directory, as
// envtest's binaries under go/bin/k8s and Go's module cache are, and makes
// them writable again when the test ends so its temporary directory goes.
func readOnlyT(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "k8s", "1.37.0-linux-amd64")
	writeT(t, filepath.Join(bin, "kube-apiserver"), "binary")
	for _, p := range []string{filepath.Join(bin, "kube-apiserver"), bin, filepath.Dir(bin)} {
		if err := os.Chmod(p, 0o500); err != nil { //nolint:gosec // read-only on purpose
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = worktrees.Writable(dir) })
}

func TestTrimWorktreesRemovesReadOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	a, machine, app := worktreeApp(t)
	readOnlyT(t, filepath.Join(app, mergedPR, "node_modules"))
	readOnlyT(t, filepath.Join(app, "work", "node_modules"))
	r := a.trimWorktrees(false, machine)
	if r.err != nil || len(r.failed) > 0 {
		t.Fatalf("run = %q, failed %+v; want no failure", r, r.failed)
	}
	if exists(filepath.Join(app, mergedPR)) || exists(filepath.Join(app, "work", "node_modules")) || !exists(filepath.Join(app, "work", "notes.txt")) {
		t.Errorf("run = %q, want the finished worktree and the build output removed, the work kept", r)
	}
}

func TestTrimWorktreesDryRunRemovesNothing(t *testing.T) {
	a, machine, app := worktreeApp(t)
	orphan := orphanT(t, filepath.Dir(app))
	r := a.trimWorktrees(true, machine)
	if !strings.HasPrefix(r.String(), "worktrees: would remove 2 finished, ") || !strings.HasSuffix(r.String(), "kept: 1 live") ||
		!strings.Contains(r.String(), "; would move aside 1 orphaned (their repository is gone) ") || r.err != nil {
		t.Errorf("run = %q", r)
	}
	for _, l := range r.paths() {
		if !strings.HasPrefix(l, "  would remove ") && !strings.HasPrefix(l, "  would move aside ") {
			t.Errorf("a dry run's path line %q says no would", l)
		}
	}
	for _, p := range []string{mergedPR, "work/node_modules", "unpushed/node_modules", "open/node_modules", "../" + orphan} {
		if !exists(filepath.Join(app, p)) {
			t.Errorf("a dry run removed %s", p)
		}
	}
	if evs, _ := a.store.Events(10, func(e state.Event) bool { return e.Verb == worktreesVerb }); len(evs) != 0 {
		t.Errorf("a dry run logged %+v", evs)
	}
}

func TestTrimWorktreesKeepsWholeWhatHoldsOnlyWork(t *testing.T) {
	a, machine, app := worktreeApp(t)
	if err := os.RemoveAll(filepath.Join(app, "unpushed", "node_modules")); err != nil {
		t.Fatal(err)
	}
	r := a.trimWorktrees(false, machine)
	if r.kept[worktrees.KeptWork] != 1 || !exists(filepath.Join(app, "unpushed", "change.go")) {
		t.Errorf("kept %v, want the unpushed worktree kept whole: %s", r.kept, r)
	}
}

func TestTrimWorktreesWithoutThePullRequestsRemovesNothing(t *testing.T) {
	a, machine, app := worktreeApp(t)
	read := func() (worktreeMachine, error) {
		m, err := machine()
		m.pulls = func(context.Context, []github.Head) (map[github.Head]github.HeadPull, error) {
			return nil, os.ErrDeadlineExceeded
		}
		return m, err
	}
	r := a.trimWorktrees(false, read)
	if r.err == nil || r.acted() || !exists(filepath.Join(app, mergedPR)) {
		t.Errorf("run = %q, want an error and nothing removed", r)
	}
}

// orphanT adds under root a worktree of a clone that is then deleted, its
// gitfile dangling, and returns its path relative to root.
func orphanT(t *testing.T, root string) string {
	t.Helper()
	clone, rel := filepath.Join(t.TempDir(), "gone"), filepath.Join("gone", testFixWord, "x")
	gitT(t, filepath.Dir(clone), "init", "-q", "-b", "main", clone)
	gitT(t, clone, "commit", "-q", "--allow-empty", "-m", "init")
	gitT(t, clone, "worktree", "add", "-q", "-b", "fix/x", filepath.Join(root, rel), "main")
	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestTrimWorktreesMovesOrphansAside(t *testing.T) {
	a, machine, app := worktreeApp(t)
	root := filepath.Dir(app)
	orphan := orphanT(t, root)
	r := a.trimWorktrees(false, machine)
	if r.err != nil || len(r.failed) > 0 || len(r.orphaned) != 1 {
		t.Fatalf("run = %q, failed %+v; want one orphan moved aside and no failure", r, r.failed)
	}
	if !strings.Contains(r.String(), "; moved aside 1 orphaned (their repository is gone) ") {
		t.Errorf("run = %q", r)
	}
	if want := "  moved aside "; !slices.ContainsFunc(r.paths(), func(l string) bool { return strings.HasPrefix(l, want+"0 MiB "+filepath.Join(root, orphan)) }) {
		t.Errorf("no path line says %q of the orphan: %q", want, r.paths())
	}
	if exists(filepath.Join(root, orphan)) || !exists(filepath.Join(root, orphansDir, orphan, ".git")) {
		t.Errorf("the orphan is not moved to %s", filepath.Join(root, orphansDir, orphan))
	}
	if again := a.trimWorktrees(true, machine); len(again.orphaned) != 0 || again.err != nil {
		t.Errorf("the next run = %q, want the moved orphan out of sight", again)
	}
}

func TestTrimWorktreesRetiresMergedBuildersEvenWhenWorkIsKept(t *testing.T) {
	a, read, root := worktreeApp(t)
	var finished []string
	var dry bool
	machine := func() (worktreeMachine, error) {
		m, err := read()
		m.retire = func(_ context.Context, paths []string, dryRun bool) error {
			finished = append(finished, paths...)
			dry = dryRun
			return nil
		}
		return m, err
	}
	r := a.trimWorktrees(true, machine)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !dry {
		t.Fatal("dry run reached live cleanup")
	}
	if !slices.Contains(finished, filepath.Join(root, mergedPR)) {
		t.Fatal(finished)
	}
	if slices.Contains(finished, filepath.Join(root, "open")) {
		t.Fatal(finished)
	}
	if !exists(filepath.Join(root, mergedPR)) {
		t.Fatal("dry run removed worktree")
	}
}

// Cleanup only touches scratch checkouts; all process data is injected.
func TestCleanRemovedWorktreeChecksLiveUsers(t *testing.T) {
	for _, name := range []string{removeUnused, removeActive, removeParked, removeDesktop, "process unavailable", "roster unavailable", litCanonicalChild, "canonical parent"} {
		t.Run(name, func(t *testing.T) {
			a, _ := stubApp(t)
			plat.Machine = ungatedMachine{Machine: plat.Machine, table: proc.NewTable()}
			a.cfg.Claude.DesktopDir = t.TempDir()
			a.cfg.Omp.SessionsDir = t.TempDir()
			a.cfg.Codex.SessionsDir = t.TempDir()
			a.cfg.Agy.BrainDir = t.TempDir()
			a.cfg.Agy.ProjectsDir = t.TempDir()
			a.cfg.Agy.ConversationsDir = t.TempDir()
			// Reuse a real, clean linked checkout whose HEAD is published.
			fixture, _, root := worktreeApp(t)
			dir := filepath.Join(root, "nopr")
			a.cfg.Disk.BuildDirs = fixture.cfg.Disk.BuildDirs
			if err := os.RemoveAll(filepath.Join(dir, "node_modules")); err != nil {
				t.Fatal(err)
			}
			head := gitT(t, dir, "rev-parse", "HEAD")
			live := dir
			if strings.HasPrefix(name, "canonical") {
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				live = alias
				if name == "canonical child" {
					live = filepath.Join(alias, "nopr", "child")
					if err := os.MkdirAll(filepath.Join(dir, "child"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch name {
			case removeActive, removeParked, "canonical child", "canonical parent":
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					agent := state.Agent{Party: state.Party{Session: removeOther}, Task: removeTask}
					if name == removeParked {
						agent.Task = ""
						agent.Park = &state.Park{Waits: removeReview}
					}
					st.Agents = append(st.Agents, agent)
					st.Starts = append(st.Starts, state.Start{Party: agent.Party, Dir: live})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			case removeDesktop:
				writeT(t, filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", "local_other.json"), `{"sessionId":"local_other","cliSessionId":removeOther,"cwd":"`+dir+`"}`)
			case "process unavailable":
				plat.Machine = platform.Stub().Machine
			case "roster unavailable":
				if err := os.RemoveAll(a.cfg.StateDir); err != nil {
					t.Fatal(err)
				}
				writeT(t, a.cfg.StateDir, "unreadable roster directory")
			}
			a.cleanRemovedWorktree(context.Background(), dir, head)
			if got, want := exists(dir), name != removeUnused; got != want {
				t.Fatalf("checkout exists = %v, want %v", got, want)
			}
		})
	}
}

// The recheck callback changes task identity after publication evidence was read.
// These temporary repositories never query a real publication service.
func TestAutomaticWorktreesRejectChangedTask(t *testing.T) {
	for _, action := range []string{worktrees.Remove, worktrees.Clean} {
		for _, change := range []string{"branch", cleanupUpstream, workerGitHubOrigin, "index"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				a, read, root := worktreeApp(t)
				m, err := read()
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, mergedPR)
				if action == worktrees.Clean {
					dir = filepath.Join(root, "work")
				}
				external := filepath.Join(t.TempDir(), "output")
				writeT(t, filepath.Join(external, "artifact"), "build")
				m.dirs = []string{dir}
				m.automatic = true
				m.trash = filepath.Join(t.TempDir(), "trash")
				m.outputs = []config.BuildOutput{{Path: external, Worktree: dir}}
				before, err := worktrees.Inspect(context.Background(), m.git, dir)
				if err != nil {
					t.Fatal(err)
				}
				pulls, err := m.pulls(context.Background(), []github.Head{{Repo: before.Repo, Branch: before.HeadBranch()}})
				if err != nil {
					t.Fatal(err)
				}
				pull := pulls[github.Head{Repo: before.Repo, Branch: before.HeadBranch()}]
				if v := worktrees.Automatic(before, &pull, false, false, false, true, m.now); v.Action != action {
					t.Fatalf("initial action = %s, want %s", v.Action, action)
				}
				calls := 0
				m.recheck = func() ([]string, error) {
					calls++
					switch change {
					case "branch":
						gitT(t, dir, "checkout", "-q", "-b", "new-task")
					case cleanupUpstream:
						gitT(t, dir, "update-ref", "refs/remotes/origin/new-task", "HEAD")
						gitT(t, dir, "branch", "--set-upstream-to=origin/new-task")
					case workerGitHubOrigin:
						gitT(t, dir, "remote", "set-url", workerGitHubOrigin, "https://github.com/acme/other.git")
					case "index":
						gd := gitT(t, dir, "rev-parse", "--absolute-git-dir")
						// Advance activity while leaving it outside the grace period.
						later := before.Touched.Add(time.Minute)
						if err := os.Chtimes(filepath.Join(gd, "index"), later, later); err != nil {
							t.Fatal(err)
						}
					}
					if head := gitT(t, dir, "rev-parse", "HEAD"); head != before.Head {
						t.Fatal("callback moved HEAD")
					}
					return nil, nil
				}
				r := a.trimWorktrees(false, func() (worktreeMachine, error) { return m, nil })
				if r.err != nil || len(r.failed) > 0 || calls != 1 || r.acted() || r.kept["worktree changed during inspection"] != 1 {
					t.Errorf("run = %s, error = %v, failed = %v, callbacks = %d", r, r.err, r.failed, calls)
				}
				for _, path := range []string{dir, filepath.Join(dir, "node_modules", "pkg", "index.js"), filepath.Join(external, "artifact")} {
					if !exists(path) {
						t.Errorf("removed protected path %s", path)
					}
				}
			})
		}
	}
}
