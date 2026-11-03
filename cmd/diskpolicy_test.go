package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const (
	diskRosterWorker = "assigned-worker"
	diskParkedWorker = "parked-worker"
	diskRemoteQuery  = "ls-remote"
)

func TestScheduledDiskPrunePreservesWarmCachesAndPressureReclaimsRecentTrash(t *testing.T) {
	a, m := diskFixture(t)
	recent := filepath.Join(m.trash[0], "recent-output")
	writeT(t, filepath.Join(recent, diskTestArtifact), "output")
	held := filepath.Join(m.trash[0], "held-output")
	writeT(t, filepath.Join(held, diskTestArtifact), "held")
	m.refs = []string{filepath.Join(held, diskTestArtifact)}
	read := func() (diskMachine, error) { return m, nil }
	r := a.pruneDisk(false, false, read)
	for _, p := range []string{filepath.Join(m.cache, "ab", "data"), filepath.Join(m.sccache, "0", "data"), recent, held} {
		if !exists(p) {
			t.Fatalf("scheduled cleanup lost warm or recent data: %s", r)
		}
	}
	r = a.pruneDisk(false, true, read)
	if len(r.Errors) > 0 || exists(recent) || !exists(held) || exists(filepath.Join(m.cache, "ab", "data")) || exists(filepath.Join(m.sccache, "0")) {
		t.Fatalf("pressure policy: %s", r)
	}
}

func TestScheduledDiskPruneTrimsOnlyOverCapCaches(t *testing.T) {
	a, m := diskFixture(t)
	a.cfg.Doctor.GoCacheMaxGiB = 1
	p := filepath.Join(m.cache, "ab", "data")
	// Apparent-size Go cache accounting allows a small sparse fixture.
	if err := os.Truncate(p, (1<<30)+1); err != nil {
		t.Fatal(err)
	}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return m, nil })
	if len(r.Errors) > 0 || exists(p) || !exists(filepath.Join(m.sccache, "0", "data")) {
		t.Fatalf("Go cap policy: %s", r)
	}
	cache := diskRun{}
	pruneSccache(&cache, m, 1, false)
	if len(cache.Errors) > 0 || exists(filepath.Join(m.sccache, "0")) {
		t.Fatalf("sccache cap policy: %s", cache)
	}
}

func TestAutomaticDiskKeepsNeverPushedAndDeletedUnmergedBranches(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "never pushed", true: "deleted unmerged"}[closed], func(t *testing.T) {
			a, read, root := worktreeApp(t)
			m, _ := read()
			d := filepath.Join(root, "nopr")
			m.dirs = []string{d}
			if closed {
				writeT(t, filepath.Join(d, "source.go"), "package feature")
				gitT(t, d, "add", "source.go")
				gitT(t, d, "commit", "-qm", "feature")
				gitT(t, d, "push", "-q", "local", "HEAD:feature")
				gitT(t, d, "push", "-q", "local", "--delete", "feature")
				gitT(t, d, "update-ref", "refs/remotes/origin/nopr", "HEAD")
				gitT(t, d, "branch", "--set-upstream-to=origin/nopr", "nopr")
				head := gitT(t, d, "rev-parse", "HEAD")
				m.pulls = func(context.Context, []github.Head) (map[github.Head]github.HeadPull, error) {
					return map[github.Head]github.HeadPull{{Repo: "acme/app", Branch: "nopr"}: {State: github.Closed, HeadOid: head, ClosedAt: m.now.Add(-24 * time.Hour)}}, nil
				}
			}
			git := m.git
			m.git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				if args[0] == diskRemoteQuery {
					return git(ctx, dir, "ls-remote", "--heads", "local")
				}
				return git(ctx, dir, args...)
			}
			r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
			if len(r.Errors) > 0 || !exists(d) {
				t.Fatalf("unpublished worktree removed: %s", r)
			}
			if closed && !exists(filepath.Join(d, "source.go")) {
				t.Fatal("lost unmerged commit")
			}
			if !closed && !exists(filepath.Join(d, "node_modules")) {
				t.Fatal("unfinished task lost output")
			}
		})
	}
}

func TestAutomaticDiskProtectsCanonicalSessionPaths(t *testing.T) {
	a, read, root := worktreeApp(t)
	m, _ := read()
	d := filepath.Join(root, mergedPR)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(d, alias); err != nil {
		t.Skip(err)
	}
	m.dirs = []string{d}
	m.live = []string{filepath.Join(alias, "node_modules")}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	if len(r.Errors) > 0 || !exists(d) || r.trees.kept[worktrees.KeptLive] != 1 {
		t.Fatalf("live alias removed: %s", r)
	}
}

func TestWorktreeLivePathsDoNotDependOnDoctorRoots(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Doctor.WorktreeRoots = nil
	a.cfg.Omp.SessionsDir = t.TempDir()
	a.cfg.Claude.DesktopDir = filepath.Join(t.TempDir(), "desktop")
	dirs := map[string]string{}
	for _, name := range []string{"running", "desktop", diskRosterWorker, diskParkedWorker} {
		dirs[name] = filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dirs[name], 0700); err != nil {
			t.Fatal(err)
		}
	}
	procRoot := t.TempDir()
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[19] = "S", "1", "100"
	writeT(t, filepath.Join(procRoot, "stat"), "btime 1\n")
	writeT(t, filepath.Join(procRoot, "424242", "stat"), "424242 (claude) "+strings.Join(fields, " "))
	writeT(t, filepath.Join(procRoot, "424242", "cmdline"), "claude\x00--session-id\x00running\x00")
	writeT(t, filepath.Join(procRoot, "424242", "environ"), "")
	if err := os.Symlink(dirs["running"], filepath.Join(procRoot, "424242", "cwd")); err != nil {
		t.Skip(err)
	}
	table, err := proc.ReadAt(procRoot)
	if err != nil {
		t.Fatal(err)
	}
	plat.Machine = ungatedMachine{Machine: plat.Machine, table: table}
	writeT(t, filepath.Join(a.cfg.Claude.ProjectsDir, "project", "s424242.jsonl"), `{"type":"user","cwd":"`+dirs["running"]+`"}`+"\n")
	writeT(t, filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", "local_desktop.json"), `{"sessionId":"local_desktop","cliSessionId":"desktop","cwd":"`+dirs["desktop"]+`"}`)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, name := range []string{diskRosterWorker, diskParkedWorker} {
			agent := state.Agent{Party: state.Party{Session: name}, Task: "task"}
			if name == diskParkedWorker {
				agent.Park = &state.Park{Waits: "waiting for review"}
			}
			st.Agents = append(st.Agents, agent)
			st.Starts = append(st.Starts, state.Start{Party: agent.Party, Dir: dirs[name]})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	m, err := a.worktreeMachine()
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range dirs {
		if !diskprune.InUse(d, m.live) {
			t.Errorf("missing %s protection with empty doctor roots", name)
		}
	}
}

func TestDoctorSelectsAutomaticDiskPolicyAndAskRetainsLegacy(t *testing.T) {
	for _, mode := range []string{config.DiskPruneAuto, config.DiskPruneAsk} {
		t.Run(mode, func(t *testing.T) {
			a, read, root := worktreeApp(t)
			a.cfg.Disk.Prune = mode
			m, _ := read()
			m.dirs = []string{filepath.Join(root, "open")}
			oldDisk, oldTrees := liveDisk, liveWorktrees
			t.Cleanup(func() { liveDisk, liveWorktrees = oldDisk, oldTrees })
			liveDisk = func(*app) (diskMachine, error) { return diskMachine{trees: m}, nil }
			liveWorktrees = func(*app) (worktreeMachine, error) { return m, nil }
			lines := a.doctorDisk(false)
			if got := exists(filepath.Join(root, "open", "node_modules")); got != (mode == config.DiskPruneAuto) {
				t.Fatalf("doctor %s open output kept=%v: %s", mode, got, strings.Join(lines, "; "))
			}
		})
	}
}

func TestAutomaticBuildOutputMovesToTrashBeforePressureReclaimsIt(t *testing.T) {
	a, read, root := worktreeApp(t)
	m, _ := read()
	d := filepath.Join(root, "work")
	m.dirs = []string{d}
	external := filepath.Join(t.TempDir(), "target")
	writeT(t, filepath.Join(external, diskTestArtifact), "build output")
	m.outputs = []config.BuildOutput{{Path: external, Worktree: d}}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	trash := filepath.Join(a.store.Dir(), "trash")
	entries, err := os.ReadDir(trash)
	if err != nil || len(entries) != 2 || len(r.Errors) > 0 {
		t.Fatalf("staging failed: %s entries=%v err=%v", r, entries, err)
	}
	if exists(external) || exists(filepath.Join(d, "node_modules")) || !exists(filepath.Join(d, "notes.txt")) {
		t.Fatal("output not staged or work lost")
	}
	if r.Freed != 0 || r.trees.freed() != 0 || !strings.Contains(r.String(), "moved aside") {
		t.Fatalf("move counted as freed space: %s", r)
	}
	r = a.pruneDisk(false, true, func() (diskMachine, error) { return diskMachine{trash: []string{trash}}, nil })
	entries, err = os.ReadDir(trash)
	if err != nil || len(entries) != 0 || r.Freed == 0 || len(r.Errors) > 0 {
		t.Fatalf("pressure did not reclaim staged output: %s", r)
	}
}

func TestAutomaticDiskDisablesLegacyGoCacheSchedule(t *testing.T) {
	a, m := diskFixture(t)
	saved := goCacheDir
	t.Cleanup(func() { goCacheDir = saved })
	goCacheDir = func() string { return m.cache }
	w := &watcher{app: a, goCacheOn: true, last: map[string]time.Time{}}
	w.goCache(a.now)
	w.bg.Wait()
	var mark goCacheMark
	if found, _ := a.store.ReadFile(goCacheFile, &mark); found {
		t.Fatal("auto mode ran legacy cache schedule")
	}
	a.cfg.Disk.Prune = config.DiskPruneAsk
	w.goCache(a.now)
	w.bg.Wait()
	if found, _ := a.store.ReadFile(goCacheFile, &mark); !found {
		t.Fatal("ask mode lost legacy cache schedule")
	}
}
