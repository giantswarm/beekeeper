package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const retireFixtureTracked = "tracked.txt"

func retireRequest(trigger string) RetireRequest {
	return RetireRequest{ID: uuid.NewString(), Trigger: trigger, Reason: "fixture ended"}
}

// launched starts the fixture's execution as a running worker.
func launched(t *testing.T, f fixture) Handle {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Phase, j.Ready = Running, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

// leaveWork gives the worker an unpublished commit, an edit and an untracked file.
func leaveWork(t *testing.T, f fixture, h Handle) string {
	t.Helper()
	ctx := context.Background()
	git := func(args ...string) string {
		t.Helper()
		out, err := f.s.git(ctx, h.Workspace, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	writeFixture(t, filepath.Join(h.Workspace, retireFixtureTracked), "unpublished commit\n")
	git("add", retireFixtureTracked)
	git("-c", "user.name=Example", "-c", "user.email=example@example.invalid", "commit", "-m", "unpublished follow-up")
	head := git("rev-parse", "HEAD")
	writeFixture(t, filepath.Join(h.Workspace, retireFixtureTracked), "uncommitted edit\n")
	writeFixture(t, filepath.Join(h.Workspace, "untracked.txt"), "untracked work\n")
	return head
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func retiredRoot(f fixture) string {
	return filepath.Join(f.s.Config.WorkspaceRoot, retiredWorkspaces, retiredLabel(f.r.Ref))
}

// recoveredFile reads path of the recovery commit from its bundle alone, in a
// fresh repository holding only the bundle's base: what a later recovery has.
func recoveredFile(t *testing.T, f fixture, x Retirement, path string) string {
	t.Helper()
	ctx := context.Background()
	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := f.s.git(ctx, "", "clone", "--quiet", "--no-local", "--", f.source, clone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.git(ctx, clone, "fetch", "--quiet", x.Recovery.Bundle, x.Recovery.Ref+":refs/heads/recovered"); err != nil {
		t.Fatalf("bundle does not recover: %v", err)
	}
	out, err := f.s.git(ctx, clone, "show", "refs/heads/recovered:"+path)
	if err != nil {
		t.Fatalf("recovered tree lacks %s: %v", path, err)
	}
	return string(out)
}

// Report completion, an explicit stop and a crash reach the same retirement:
// the worker's work survives in its bundle and moved-aside tree alike.
func TestRetirePreservesWorkOnEveryTerminalPath(t *testing.T) {
	for _, trigger := range []string{RetireTriggerReport, RetireTriggerStop, RetireTriggerCrash} {
		t.Run(trigger, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			h := launched(t, f)
			head := leaveWork(t, f, h)
			source := f.s.sourcePath(f.r.Ref)
			writeFixture(t, source, "owned source bundle\n")
			credential := filepath.Join(f.s.Root, workerGitHubBinDir, string(f.r.Ref.Execution), "gh")
			writeFixture(t, credential, "#!/bin/sh\n")
			env, err := f.s.ownedEnvironment(Journal{Handle: h}, nil)
			if err != nil {
				t.Fatal(err)
			}
			temporary := ""
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, "TMPDIR="); ok {
					temporary = v
				}
			}
			writeFixture(t, filepath.Join(temporary, "scratch"), "temporary\n")
			switch trigger {
			case RetireTriggerStop:
				if _, err := f.s.Stop(ctx, f.r.Ref, StopRequest{ID: uuid.NewString(), Reason: "operator stop"}); err != nil {
					t.Fatal(err)
				}
			default:
				f.launcher.states[h.Unit] = unitInactive
			}
			x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(trigger))
			if err != nil {
				t.Fatal(err)
			}
			if x.Stage != RetireRetired || x.Recovery == nil || x.Recovery.Head != head || x.Recovery.Base != h.Commit || !x.Recovery.Dirty || x.RetainUntil.IsZero() {
				t.Fatalf("retirement = %+v", x)
			}
			if got := recoveredFile(t, f, x, retireFixtureTracked); got != "uncommitted edit\n" {
				t.Fatalf("recovered edit = %q", got)
			}
			if got := recoveredFile(t, f, x, "untracked.txt"); got != "untracked work\n" {
				t.Fatalf("recovered untracked file = %q", got)
			}
			if digest, err := fileDigest(x.Recovery.Bundle); err != nil || digest != x.Recovery.BundleDigest {
				t.Fatalf("bundle digest %s, %v", digest, err)
			}
			if _, err := os.Lstat(filepath.Dir(h.Workspace)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("execution root stayed: %v", err)
			}
			aside := filepath.Join(f.s.Root, retiredResources, retiredLabel(f.r.Ref))
			for path, want := range map[string]string{
				filepath.Join(retiredRoot(f), "workspace", retireFixtureTracked): "uncommitted edit\n",
				filepath.Join(retiredRoot(f), temporaryDir, "scratch"):           "temporary\n",
				filepath.Join(aside, "sources", filepath.Base(source)):           "owned source bundle\n",
				filepath.Join(aside, workerGitHubBinDir, "gh"):                   "#!/bin/sh\n",
			} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want { //nolint:gosec // paths under this test's temporary execution directory
					t.Fatalf("moved aside %s: %q, %v", path, got, err)
				}
			}
			if _, err := f.s.Read(f.r.Ref); err != nil {
				t.Fatalf("journal evidence lost: %v", err)
			}
			again, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerCrash))
			if err != nil || again.ID != x.ID || again.Trigger != trigger || !again.RetiredAt.Equal(x.RetiredAt) {
				t.Fatalf("rerun changed the retirement: %+v, %v", again, err)
			}
		})
	}
}

// A clean tree on its starting commit has no work and gets no recovery entry.
func TestRetireCleanExecutionNeedsNoRecovery(t *testing.T) {
	f := testFixture(t)
	h := launched(t, f)
	f.launcher.states[h.Unit] = unitInactive
	x, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerReport))
	if err != nil || x.Stage != RetireRetired || x.Recovery != nil {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	if _, err := os.Lstat(f.s.recoveryBundle(f.r.Ref)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean execution left a bundle: %v", err)
	}
}

// A worker reset back to its starting commit looks clean, yet its commits live
// on in the reflog: they are work, and the bundle must carry them.
func TestRetirePreservesCommitsOnlyReflogsReach(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h := launched(t, f)
	writeFixture(t, filepath.Join(h.Workspace, "abandoned.txt"), "abandoned work\n")
	for _, args := range [][]string{
		{"add", "abandoned.txt"},
		{"-c", "user.name=Example", "-c", "user.email=example@example.invalid", "commit", "-m", "abandoned follow-up"},
	} {
		if _, err := f.s.git(ctx, h.Workspace, args...); err != nil {
			t.Fatal(err)
		}
	}
	abandoned, err := f.s.git(ctx, h.Workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.git(ctx, h.Workspace, "reset", "--hard", h.Commit); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[h.Unit] = unitInactive
	x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerCrash))
	if err != nil || x.Recovery == nil || x.Recovery.Head != h.Commit {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := f.s.git(ctx, "", "clone", "--quiet", "--no-local", "--", f.source, clone); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.git(ctx, clone, "fetch", "--quiet", x.Recovery.Bundle, x.Recovery.Ref+":refs/heads/recovered"); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.git(ctx, clone, "show", strings.TrimSpace(string(abandoned))+":abandoned.txt")
	if err != nil || string(got) != "abandoned work\n" {
		t.Fatalf("abandoned commit not in the bundle: %q, %v", got, err)
	}
}

// A parked worker between turns is unfinished: its worktree stays, and so does
// any execution whose unit state the supervisor cannot confirm.
func TestRetireRefusesUnfinishedExecutions(t *testing.T) {
	for name, state := range map[string]string{"parked between turns": unitActive, "unknown supervisor state": "unknown"} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			h := launched(t, f)
			f.launcher.states[h.Unit] = state
			if _, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerCrash)); !errors.Is(err, ErrUnfinished) {
				t.Fatalf("retire = %v", err)
			}
			j, err := f.s.Read(f.r.Ref)
			if err != nil || j.Retirement != nil {
				t.Fatalf("journal = %+v, %v", j.Retirement, err)
			}
			if _, err := os.Stat(h.Workspace); err != nil {
				t.Fatalf("worktree touched: %v", err)
			}
		})
	}
	t.Run("prepared and never stopped", func(t *testing.T) {
		f := testFixture(t)
		if _, err := f.s.Start(context.Background(), f.r); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerStop)); !errors.Is(err, ErrUnfinished) {
			t.Fatalf("retire = %v", err)
		}
	})
}

// A start that failed never ran a worker: what it left retires without
// reading a possibly half-built repository.
func TestRetireFailedStartLeavesNothingBehind(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Phase, j.LastError = Failed, "workspace preparation failed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(filepath.Dir(h.Workspace), "repository.git")); err != nil {
		t.Fatal(err)
	}
	x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerFailedStart))
	if err != nil || x.Stage != RetireRetired || x.Recovery != nil {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	if _, err := os.Lstat(h.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start left its workspace: %v", err)
	}
}

// Work that cannot be preserved stops the retirement before anything moves, and
// a retry after the fault resumes the same retirement.
func TestRetireInterruptionResumes(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h := launched(t, f)
	leaveWork(t, f, h)
	f.launcher.states[h.Unit] = unitInactive
	command := f.s.Command
	failing := true
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if failing && slices.Contains(args, "bundle") {
			return nil, errors.New("disk full")
		}
		return command(ctx, dir, env, bin, args...)
	}
	first := retireRequest(RetireTriggerCrash)
	x, err := f.s.Retire(ctx, f.r.Ref, first)
	if err == nil || x.Stage != RetireRequested || x.LastError == "" {
		t.Fatalf("failed preservation reported %+v, %v", x, err)
	}
	if _, err := os.Stat(filepath.Join(h.Workspace, "untracked.txt")); err != nil {
		t.Fatalf("work moved before it was preserved: %v", err)
	}
	if _, err := f.s.Start(ctx, f.r); err == nil {
		t.Fatal("a retiring execution started again")
	}
	failing = false
	x, err = f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerStop))
	if err != nil || x.Stage != RetireRetired || x.ID != first.ID || x.Trigger != RetireTriggerCrash || x.LastError != "" || x.Recovery == nil {
		t.Fatalf("resumed retirement = %+v, %v", x, err)
	}
}

// A retirement interrupted between moves finds the moved root done on its rerun,
// and never preserves twice from a tree that is gone.
func TestRetireResumesAfterPartialMove(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h := launched(t, f)
	leaveWork(t, f, h)
	f.launcher.states[h.Unit] = unitInactive
	source := f.s.sourcePath(f.r.Ref)
	writeFixture(t, source, "owned source bundle\n")
	aside := filepath.Join(f.s.Root, retiredResources, retiredLabel(f.r.Ref), "sources")
	writeFixture(t, filepath.Join(aside, "conflict"), "already there\n")
	x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerCrash))
	if err == nil || x.Stage != RetirePreserved {
		t.Fatalf("conflicting move reported %+v, %v", x, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("conflicting move lost the source: %v", err)
	}
	if err := os.Rename(aside, aside+".inspected"); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerCrash))
	if err != nil || resumed.Stage != RetireRetired || resumed.Recovery.Commit != x.Recovery.Commit {
		t.Fatalf("resumed retirement = %+v, %v", resumed, err)
	}
}

// Two workers on one repository own separate clones: retiring one touches
// neither the other's work nor the provisioned source, even through a symlink
// the retiring worker planted in its tree.
func TestRetireLeavesAnotherExecutionAndSources(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	other := f
	other.r.Ref.Execution = ExecutionID(uuid.NewString())
	other.r.RequestID, other.r.ConversationID = uuid.NewString(), uuid.NewString()
	h := launched(t, f)
	otherHandle := launched(t, other)
	otherFile := filepath.Join(otherHandle.Workspace, "other.txt")
	writeFixture(t, otherFile, "other worker's work\n")
	for link, target := range map[string]string{"to-other": otherHandle.Workspace, "to-source": f.source} {
		if err := os.Symlink(target, filepath.Join(h.Workspace, link)); err != nil {
			t.Fatal(err)
		}
	}
	f.launcher.states[h.Unit] = unitInactive
	x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerReport))
	if err != nil || x.Stage != RetireRetired {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	if got, err := os.ReadFile(otherFile); err != nil || string(got) != "other worker's work\n" { //nolint:gosec // path under this test's temporary execution directory
		t.Fatalf("other worker's file: %q, %v", got, err)
	}
	if _, err := f.s.git(ctx, otherHandle.Workspace, "status", "--porcelain"); err != nil {
		t.Fatalf("other worker's repository damaged: %v", err)
	}
	if _, err := f.s.git(ctx, f.source, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("provisioned source damaged: %v", err)
	}
	if state := f.launcher.states[otherHandle.Unit]; state != unitActive {
		t.Fatalf("other unit state = %s", state)
	}
}

// Containment refuses and leaves everything in place: a resource replaced by a
// symlink, an inventory naming a neighbour, one overlapping a provisioned path.
func TestRetireContainment(t *testing.T) {
	for name, attack := range map[string]func(*testing.T, fixture, Handle) string{
		"execution root is a symlink": func(t *testing.T, f fixture, h Handle) string {
			victim := filepath.Join(filepath.Dir(f.s.Config.WorkspaceRoot), "victim")
			writeFixture(t, filepath.Join(victim, "keep"), "operator file\n")
			dir := filepath.Dir(h.Workspace)
			if err := os.Rename(dir, dir+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, dir); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(victim, "keep")
		},
		"source directory is a symlink": func(t *testing.T, f fixture, _ Handle) string {
			victim := filepath.Join(filepath.Dir(f.s.Root), "victim")
			writeFixture(t, filepath.Join(victim, "keep"), "operator file\n")
			dir := filepath.Dir(f.s.sourcePath(f.r.Ref))
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, dir); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(victim, "keep")
		},
		"inventory names another execution": func(t *testing.T, f fixture, _ Handle) string {
			victim := filepath.Join(f.s.Config.WorkspaceRoot, uuid.NewString())
			writeFixture(t, filepath.Join(victim, "keep"), "operator file\n")
			if err := f.s.update(context.Background(), f.r.Ref, func(j *Journal) error {
				j.Resources[0].Path = victim
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(victim, "keep")
		},
		"resource overlaps a provisioned source": func(t *testing.T, f fixture, h Handle) string {
			f.s.Config.Repositories["example/overlap"] = filepath.Dir(h.Workspace)
			keep := filepath.Join(filepath.Dir(h.Workspace), "keep")
			writeFixture(t, keep, "operator file\n")
			return keep
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			h := launched(t, f)
			keep := attack(t, f, h)
			f.launcher.states[h.Unit] = unitInactive
			x, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerCrash))
			if err == nil || x.Finished() {
				t.Fatalf("retirement = %+v, %v", x, err)
			}
			if got, err := os.ReadFile(keep); err != nil || string(got) != "operator file\n" { //nolint:gosec // path under this test's temporary execution directory
				t.Fatalf("protected file: %q, %v", got, err)
			}
		})
	}
}

// A worker's repository configuration must not run as the retirement: a clean
// filter it left behind stays inert while its work is still preserved.
func TestRetireRunsNoWorkerFilter(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h := launched(t, f)
	marker := filepath.Join(t.TempDir(), "ran")
	if _, err := f.s.git(ctx, h.Workspace, "config", "filter.evil.clean", "touch "+marker+"; cat"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.git(ctx, h.Workspace, "config", "filter.evil.required", "true"); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(h.Workspace, ".gitattributes"), "*.txt filter=evil\n")
	writeFixture(t, filepath.Join(h.Workspace, "untracked.txt"), "untracked work\n")
	f.launcher.states[h.Unit] = unitInactive
	x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerCrash))
	if err != nil || x.Recovery == nil {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker filter ran during retirement: %v", err)
	}
}

// A worktree pointed at another repository is not read as this worker's: the
// retirement stops with everything in place.
func TestRetireRefusesRedirectedWorktree(t *testing.T) {
	f := testFixture(t)
	h := launched(t, f)
	leaveWork(t, f, h)
	writeFixture(t, filepath.Join(h.Workspace, ".git"), "gitdir: "+filepath.Join(f.source, ".git")+"\n")
	f.launcher.states[h.Unit] = unitInactive
	x, err := f.s.Retire(context.Background(), f.r.Ref, retireRequest(RetireTriggerCrash))
	if err == nil || x.Stage != RetireRequested {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	if _, err := os.Stat(filepath.Join(h.Workspace, "untracked.txt")); err != nil {
		t.Fatalf("work moved: %v", err)
	}
}

// A journal written before the inventory existed retires too: its resources
// are recorded before anything acts on them.
func TestRetireRecordsAnOlderJournal(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h := launched(t, f)
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Resources = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[h.Unit] = unitInactive
	if _, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerCrash)); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || !sameResources(j.Resources, f.s.ownedResources(f.r.Ref)) {
		t.Fatalf("journal resources = %+v, %v", j.Resources, err)
	}
}

// A start's inventory is durable before the workspace it names exists.
func TestStartRecordsInventoryBeforeCreatingResources(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	command := f.s.Command
	var recorded []Resource
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if slices.Contains(args, "clone") && recorded == nil {
			j, err := f.s.Read(f.r.Ref)
			if err != nil {
				t.Fatal(err)
			}
			recorded = j.Resources
		}
		return command(ctx, dir, env, bin, args...)
	}
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if !sameResources(recorded, f.s.ownedResources(f.r.Ref)) {
		t.Fatalf("inventory before the clone = %+v", recorded)
	}
	for _, res := range recorded {
		if res.Owner != f.r.Ref {
			t.Fatalf("resource %s has owner %+v", res.Kind, res.Owner)
		}
	}
}

// An upload whose start never landed has no journal: its owner record makes it
// retirable, and a directory without one is of unknown ownership and stays.
func TestRetireUnjournaledSource(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if err := f.s.ReceiveSource(ctx, f.r.Ref, SourceChunk{Total: 4, Data: []byte("data")}); err != nil {
		t.Fatal(err)
	}
	unit := UnitName(f.r.Ref)
	f.launcher.states[unit] = unitActive
	if _, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerFailedStart)); !errors.Is(err, ErrUnfinished) {
		t.Fatalf("a live unit was retired: %v", err)
	}
	f.launcher.states[unit] = unitInactive
	x, err := f.s.Retire(ctx, f.r.Ref, retireRequest(RetireTriggerFailedStart))
	if err != nil || x.Stage != RetireNoJournal {
		t.Fatalf("retirement = %+v, %v", x, err)
	}
	if _, err := os.Lstat(filepath.Dir(f.s.sourcePath(f.r.Ref))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.s.Root, retiredResources, retiredLabel(f.r.Ref), "sources", filepath.Base(f.s.sourcePath(f.r.Ref)))); err != nil {
		t.Fatalf("source not moved aside: %v", err)
	}
	unknown := f.r.Ref
	unknown.Execution = ExecutionID(uuid.NewString())
	f.launcher.states[UnitName(unknown)] = unitInactive
	writeFixture(t, f.s.sourcePath(unknown), "unknown ownership\n")
	if _, err := f.s.Retire(ctx, unknown, retireRequest(RetireTriggerFailedStart)); err == nil {
		t.Fatal("a source of unknown ownership was retired")
	}
	if _, err := os.Stat(f.s.sourcePath(unknown)); err != nil {
		t.Fatalf("unknown source moved: %v", err)
	}
}

func TestOwnedEnvironmentRoutesWorkerPaths(t *testing.T) {
	f := testFixture(t)
	h := launched(t, f)
	env, err := f.s.ownedEnvironment(Journal{Handle: h}, []string{"PATH=/usr/bin", "GOCACHE=/home/worker/.cache/go-build", "GOPATH=/home/worker/go"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(h.Workspace)
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Fatalf("environment lost PATH: %v", env)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "GOTMPDIR", "XDG_CACHE_HOME", "GOCACHE", "GOPATH", "GOMODCACHE"} {
		values := []string{}
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				values = append(values, v)
			}
		}
		if len(values) != 1 || !strictlyWithin(dir, values[0]) {
			t.Fatalf("%s = %v, want one path inside %s", key, values, dir)
		}
	}
}

func TestHostRetentionValidates(t *testing.T) {
	f := testFixture(t)
	for retention, valid := range map[string]bool{"": true, "24h": true, "0s": false, "-1h": false, "soon": false} {
		c := f.s.Config
		c.Retention = retention
		if err := c.Validate(); (err == nil) != valid {
			t.Fatalf("retention %q: %v", retention, err)
		}
	}
}
