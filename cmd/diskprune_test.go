package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const (
	diskTestGlobalLock = "global lock"
	diskTestArtifact   = "artifact"
	diskTestOutputDir  = "build-output"
)

func diskFixture(t *testing.T) (*app, diskMachine) {
	t.Helper()
	a, _ := stubApp(t)
	a.now = time.Now()
	base := t.TempDir()
	m := diskMachine{cache: filepath.Join(base, "go-cache"), sccache: filepath.Join(base, "sccache"), tmp: []string{filepath.Join(base, "tmp")}, trash: []string{filepath.Join(base, "trash")}}
	old := a.now.Add(-8 * 24 * time.Hour)
	for _, d := range []string{filepath.Join(m.cache, "ab"), filepath.Join(m.sccache, "0"), filepath.Join(m.tmp[0], "dead"), filepath.Join(m.trash[0], "output")} {
		writeT(t, filepath.Join(d, "data"), strings.Repeat("x", 4096))
		_ = os.Chtimes(filepath.Join(d, "data"), old, old)
		_ = os.Chtimes(d, old, old)
	}
	return a, m
}

func TestDiskPruneFreesCachesTmpAndTrash(t *testing.T) {
	a, m := diskFixture(t)
	read := func() (diskMachine, error) { return m, nil }
	r := a.pruneDisk(true, true, read)
	if len(r.Errors) > 0 || r.Freed == 0 {
		t.Fatalf("dry=%s", r)
	}
	if !exists(filepath.Join(m.cache, "ab", "data")) {
		t.Fatal("dry removed cache")
	}
	r = a.pruneDisk(false, true, read)
	if len(r.Errors) > 0 || r.Freed == 0 {
		t.Fatalf("apply=%s", r)
	}
	for _, p := range []string{filepath.Join(m.cache, "ab", "data"), filepath.Join(m.sccache, "0"), filepath.Join(m.tmp[0], "dead"), filepath.Join(m.trash[0], "output")} {
		if exists(p) {
			t.Errorf("left output %s", filepath.Base(p))
		}
	}
}

func TestDiskPruneKeepsCachesUnderRunsAndReferences(t *testing.T) {
	for _, kind := range []string{"build", diskTestGlobalLock, "Go lock", "reference"} {
		t.Run(kind, func(t *testing.T) {
			a, m := diskFixture(t)
			switch kind {
			case "build":
				m.builds = true
			case diskTestGlobalLock, "Go lock":
				d := m.cache
				if kind == diskTestGlobalLock {
					d = filepath.Join(a.store.Dir(), "build-cache")
				}
				release, err := gocache.Use(context.Background(), d)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "reference":
				m.refs = []string{filepath.Join(m.cache, "ab", "data"), filepath.Join(m.sccache, "0", "data")}
			}
			r := a.pruneDisk(false, false, func() (diskMachine, error) { return m, nil })
			if len(r.Errors) > 0 {
				t.Fatal(r)
			}
			for _, p := range []string{filepath.Join(m.cache, "ab", "data"), filepath.Join(m.sccache, "0", "data")} {
				if !exists(p) {
					t.Fatal("removed a held cache")
				}
			}
			if len(r.Kept) == 0 {
				t.Fatal("no keep reason")
			}
		})
	}
}

func TestDiskPruneReadFailureKeepsEverything(t *testing.T) {
	a, m := diskFixture(t)
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{}, errors.New("processes unreadable") })
	if len(r.Errors) == 0 || !exists(filepath.Join(m.tmp[0], "dead")) {
		t.Fatal("pruned after failed process read")
	}
}

func TestDiskPruneExternalOutputOnlyForEndedOwner(t *testing.T) {
	a, read, root := worktreeApp(t)
	external := filepath.Join(t.TempDir(), "target")
	writeT(t, filepath.Join(external, diskTestArtifact), "build")
	m, err := read()
	if err != nil {
		t.Fatal(err)
	}
	// Avoid remote queries for unrelated fixture branches in this proof.
	m.dirs = []string{filepath.Join(root, mergedPR)}
	m.automatic = true
	m.buildDirs = []string{"generated-output"}
	writeT(t, filepath.Join(m.dirs[0], "generated-output", diskTestArtifact), "build")
	m.outputs = []config.BuildOutput{{Path: external, Worktree: m.dirs[0]}}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	if len(r.Errors) > 0 || exists(external) || exists(m.dirs[0]) {
		t.Fatalf("prune=%s", r)
	}
	if len(r.trees.external) != 1 || len(r.trees.cleaned) != 0 || !strings.Contains(r.String(), "external build output (1)") {
		t.Fatalf("external output counted as a worktree: %s", r)
	}
}

func TestDiskPrunePressureScheduleAndAsk(t *testing.T) {
	a, m := diskFixture(t)
	a.cfg.Disk = config.Disk{Prune: config.DiskPruneAuto, Every: config.Duration{Duration: time.Hour}}
	w := &watcher{app: a, diskPruneOn: true, last: map[string]time.Time{}}
	saved := liveDisk
	defer func() { liveDisk = saved }()
	calls := 0
	liveDisk = func(*app) (diskMachine, error) { calls++; return m, nil }
	now := a.now
	w.diskPrune(now, false)
	w.bg.Wait()
	w.diskPrune(now.Add(time.Minute), true)
	w.bg.Wait()
	if calls != 1 {
		t.Fatalf("pressure retried too soon: %d", calls)
	}
	w.diskPrune(now.Add(6*time.Minute), true)
	w.bg.Wait()
	if calls != 2 {
		t.Fatalf("pressure failed to run ahead of schedule: %d", calls)
	}
	w.diskPrune(now.Add(2*time.Hour), false)
	w.bg.Wait()
	if calls != 3 {
		t.Fatalf("scheduled prune did not run: %d", calls)
	}
	a.cfg.Disk.Prune = config.DiskPruneAsk
	w.diskPrune(now.Add(4*time.Hour), true)
	w.bg.Wait()
	if calls != 3 {
		t.Fatal("ask performed automatic disk pruning")
	}
}

func TestLowDiskStartsDiskPrune(t *testing.T) {
	a, m := diskFixture(t)
	a.cfg.Disk.Prune = config.DiskPruneAuto
	w := &watcher{app: a, diskPruneOn: true, last: map[string]time.Time{}}
	saved := liveDisk
	defer func() { liveDisk = saved }()
	calls := 0
	liveDisk = func(*app) (diskMachine, error) { calls++; return m, nil }
	w.sampleDisk(a.now, machine.Disk{FreeMiB: 1, TotalMiB: 10000}, nil, nil)
	w.bg.Wait()
	if calls != 1 {
		t.Fatal("LOW DISK did not trigger disk pruning")
	}
}

func TestHarnessWorktreeDiscovery(t *testing.T) {
	_, read, root := worktreeApp(t)
	m, err := read()
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(filepath.Dir(filepath.Dir(root)), "clone")
	nested := filepath.Join(clone, ".claude", "worktrees", "agent")
	gitT(t, clone, "worktree", "add", "-q", "-b", "harness", nested, "main")
	dirs := worktrees.FindHarness([]string{filepath.Dir(clone)})
	if len(dirs) != 1 || dirs[0] != nested {
		t.Fatalf("harness discovery=%v (fixture=%d)", dirs, len(m.dirs))
	}
}

func TestDiskPruneDeletedRemoteAndActiveWork(t *testing.T) {
	a, read, root := worktreeApp(t)
	m, err := read()
	if err != nil {
		t.Fatal(err)
	}
	m.dirs = []string{filepath.Join(root, "nopr"), filepath.Join(root, "work"), filepath.Join(root, "unpushed"), filepath.Join(root, "live")}
	gitT(t, filepath.Join(root, "nopr"), "update-ref", "refs/remotes/origin/nopr", "HEAD")
	gitT(t, filepath.Join(root, "nopr"), "branch", "--set-upstream-to=origin/nopr", "nopr")
	git := m.git
	m.git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if args[0] == diskRemoteQuery {
			return git(ctx, dir, "ls-remote", "--heads", "local")
		}
		return git(ctx, dir, args...)
	}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	if len(r.Errors) > 0 {
		t.Fatal(r)
	}
	if exists(filepath.Join(root, "nopr")) {
		t.Fatal("deleted remote branch worktree remained")
	}
	for _, p := range []string{"work/notes.txt", "unpushed/change.go", "live/node_modules"} {
		if !exists(filepath.Join(root, p)) {
			t.Errorf("lost protected work %s", p)
		}
	}
}

func TestDiskPruneRetainsOwnerUntilExternalOutputCanGo(t *testing.T) {
	a, read, root := worktreeApp(t)
	m, err := read()
	if err != nil {
		t.Fatal(err)
	}
	m.dirs = []string{filepath.Join(root, mergedPR)}
	external := filepath.Join(t.TempDir(), "target")
	writeT(t, filepath.Join(external, diskTestArtifact), "build")
	m.outputs = []config.BuildOutput{{Path: external, Worktree: m.dirs[0]}}
	held := []string{filepath.Join(external, diskTestArtifact)}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m, refs: held}, nil })
	if len(r.Errors) > 0 || !exists(external) || !exists(m.dirs[0]) {
		t.Fatalf("held output or owner lost: %s", r)
	}
	r = a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	if len(r.Errors) > 0 || exists(external) || exists(m.dirs[0]) {
		t.Fatalf("unused output or owner remained: %s", r)
	}
}

func TestDiskPruneRechecksNewProcessReferences(t *testing.T) {
	a, read, root := worktreeApp(t)
	m, err := read()
	if err != nil {
		t.Fatal(err)
	}
	m.dirs = []string{filepath.Join(root, mergedPR)}
	m.recheck = func() ([]string, error) { return []string{m.dirs[0]}, nil }
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	if len(r.Errors) > 0 || !exists(m.dirs[0]) {
		t.Fatalf("new process's worktree removed: %s", r)
	}
}

func TestDiskFillingStartsPruneBeforeLowDisk(t *testing.T) {
	a, m := diskFixture(t)
	a.cfg.Disk = config.Disk{Prune: config.DiskPruneAuto, Every: config.Duration{Duration: time.Hour}}
	a.cfg.Watch.DiskFillWithin.Duration = 24 * time.Hour
	now := a.now
	if err := a.store.WriteFile(diskPruneFile, worktreesMark{At: now}); err != nil {
		t.Fatal(err)
	}
	w := &watcher{app: a, diskPruneOn: true, last: map[string]time.Time{}}
	saved := liveDisk
	defer func() { liveDisk = saved }()
	calls := 0
	liveDisk = func(*app) (diskMachine, error) { calls++; return m, nil }
	w.sampleDisk(now, machine.Disk{FreeMiB: 500 * 1024, TotalMiB: 1000 * 1024}, nil, nil)
	w.bg.Wait()
	w.sampleDisk(now.Add(6*time.Minute), machine.Disk{FreeMiB: 490 * 1024, TotalMiB: 1000 * 1024}, nil, nil)
	w.bg.Wait()
	if calls != 1 {
		t.Fatalf("DISK FILLING prune calls=%d", calls)
	}
}

func TestDiskPruneTmpAndTrashUseDescendantAge(t *testing.T) {
	a, m := diskFixture(t)
	for _, base := range append(m.tmp, m.trash...) {
		for _, tc := range []struct {
			name string
			age  time.Duration
		}{{"stale-disk-output", 2 * time.Hour}, {"recent-disk-output", 30 * time.Minute}} {
			d := filepath.Join(base, tc.name)
			writeT(t, filepath.Join(d, diskTestOutputDir, diskTestArtifact), "build")
			at := a.now.Add(-tc.age)
			for _, p := range []string{filepath.Join(d, diskTestOutputDir, diskTestArtifact), filepath.Join(d, diskTestOutputDir), d} {
				if err := os.Chtimes(p, at, at); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return m, nil })
	if len(r.Errors) > 0 {
		t.Fatal(r)
	}
	for _, base := range append(m.tmp, m.trash...) {
		if exists(filepath.Join(base, "stale-disk-output")) || !exists(filepath.Join(base, "recent-disk-output")) {
			t.Fatalf("output age policy failed: %s", r)
		}
	}
}

// Processes skipped as other namespaces' (a kind node's pause container)
// keep nothing: every step prunes, and the line counts them.
func TestDiskPruneForeignProcessesKeepNothing(t *testing.T) {
	a, m := diskFixture(t)
	m.refresh = func() (diskRefs, error) { return diskRefs{foreign: 3}, nil }
	r := a.pruneDisk(false, true, func() (diskMachine, error) { return m, nil })
	if len(r.Errors) > 0 || r.unknown != nil {
		t.Fatalf("foreign processes failed the run: %s", r)
	}
	if exists(filepath.Join(m.cache, "ab", "data")) || exists(filepath.Join(m.tmp[0], "dead")) || exists(filepath.Join(m.trash[0], "output")) {
		t.Errorf("foreign processes kept candidates: %s", r)
	}
	if !strings.Contains(r.String(), "skipped 3 unreadable processes of other namespaces") {
		t.Errorf("skipped processes not counted: %s", r)
	}
}

// An unreadable process keeps only the step decided while it is unreadable:
// the run goes on, and later steps read the processes again.
func TestDiskPruneUnknownProcessKeepsOnlyItsStep(t *testing.T) {
	unknown := &proc.UnknownError{Processes: []proc.UnknownProcess{{PID: 42, Command: "sh", Err: errors.New("unreadable")}}}
	for _, tc := range []struct {
		name     string
		unknownN int
		kept     []string
		pruned   func(diskMachine) []string
	}{
		{"every read", 99, []string{"caches: ", "tmp: ", "trash: "}, func(diskMachine) []string { return nil }},
		{"first read", 1, []string{"caches: "}, func(m diskMachine) []string {
			return []string{filepath.Join(m.tmp[0], "dead"), filepath.Join(m.trash[0], "output")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, m := diskFixture(t)
			reads := 0
			m.refresh = func() (diskRefs, error) {
				reads++
				if reads <= tc.unknownN {
					return diskRefs{}, unknown
				}
				return diskRefs{}, nil
			}
			r := a.pruneDisk(false, true, func() (diskMachine, error) { return m, nil })
			if len(r.Errors) > 0 {
				t.Fatalf("an unreadable process failed the run: %s", r)
			}
			for _, step := range tc.kept {
				if r.Kept[step+keptReferencesUnknown] == 0 {
					t.Errorf("%s kept no candidate: %s", step, r)
				}
			}
			if !exists(filepath.Join(m.cache, "ab", "data")) {
				t.Error("cache trimmed while references were unknown")
			}
			if !strings.Contains(r.String(), "references unknown: process 42 (sh)") {
				t.Errorf("unreadable process not named: %s", r)
			}
			for _, p := range tc.pruned(m) {
				if exists(p) {
					t.Errorf("readable step kept %s", filepath.Base(p))
				}
			}
			if tc.pruned(m) == nil && (!exists(filepath.Join(m.tmp[0], "dead")) || !exists(filepath.Join(m.trash[0], "output"))) {
				t.Error("pruned while references were unknown")
			}
		})
	}
}

func TestDiskPruneUnknownProcessKeepsRecheckedWorktree(t *testing.T) {
	a, read, root := worktreeApp(t)
	m, err := read()
	if err != nil {
		t.Fatal(err)
	}
	m.dirs = []string{filepath.Join(root, mergedPR)}
	m.recheck = func() ([]string, error) {
		return nil, &proc.UnknownError{Processes: []proc.UnknownProcess{{PID: 42, Err: errors.New("unreadable")}}}
	}
	r := a.pruneDisk(false, false, func() (diskMachine, error) { return diskMachine{trees: m}, nil })
	if len(r.Errors) > 0 || !exists(m.dirs[0]) || r.trees.kept[keptReferencesUnknown] != 1 {
		t.Fatalf("worktree of an unknown process: %s", r)
	}
}
