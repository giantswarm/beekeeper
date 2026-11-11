package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/remotebuild"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

var mergeRemoteCheck = func(ctx context.Context, runner remotebuild.Runner, argv []string) (bool, int) {
	return runner.TryMergeCheck(ctx, argv)
}

// An overloaded coordinator never starts a full-module checker. The configured
// remote runner is selected before this guard and never falls back to local work.
var mergeLocalHeadroom = func(dir string) error {
	if load, err := machine.ReadLoad(); err == nil && load[0] >= float64(runtime.NumCPU()) {
		return fmt.Errorf("coordinator overloaded (load %.1f, CPUs %d); configure run.remote or await merge-result CI", load[0], runtime.NumCPU())
	}
	if disk, err := machine.ReadDisk(dir); err == nil && disk.FreeMiB < 8*1024 {
		return fmt.Errorf("check filesystem has %d MiB free; at least 8192 MiB required", disk.FreeMiB)
	}
	return nil
}

func (a *app) recordMergeEnvironment(p superloop.Pull, head, reason string) error {
	if a.store == nil {
		return fmt.Errorf("no store for merge environment finding")
	}
	key := "merge environment " + p.Ref + "@" + head
	events, err := a.store.Events(0, func(e state.Event) bool {
		return e.Verb == loopFileVerb && strings.HasPrefix(e.Detail, "["+key+"] ")
	})
	if err != nil || len(events) > 0 {
		return err
	}
	ref, _ := parseRef(p.Ref)
	finding := fmt.Sprintf("Merge-result check environment failed for https://github.com/%s/pull/%d at head %s: %s. This is a checker environment finding, not a branch repair; retrying once in a clean environment, or accepting CI on the exact merge result.", ref.Repo, ref.N, head, reason)
	return a.store.Record(about(event(loopParty, loopFileVerb, "[%s] %s%s%s", key, loopParty.Name, fileReportSeparator, finding), p.Ref))
}

func (a *app) runReviewedMergeChecks(ctx context.Context, dir, _, base, head string) (string, int) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", 0
	}
	if a.cfg.Run.Remote.Enabled {
		var output bytes.Buffer
		runner := remotebuild.Runner{Dir: dir, Config: a.cfg.Run.Remote, StateDir: a.cfg.StateDir,
			Stdout: &output, Stderr: &output, Record: a.runRecorder(), Head: head}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// This attempt's worktree is finished, including after an environment
			// failure. A retry gets a new Job with fresh scratch and build caches.
			_ = runner.Sweep(cleanup, []string{dir}, false)
		}()
		run := func(ctx context.Context, _, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, mergeCheckTimeout)
			defer cancel()
			output.Reset()
			runner.Name = name + " " + strings.Join(args, " ")
			handled, rc := mergeRemoteCheck(ctx, runner, append([]string{name}, args...))
			out := output.String()
			if !handled || ctx.Err() != nil || rc == 125 || rc == 126 || rc == 127 || rc >= 128 || rc != 0 && mergeCheckHostFailure(out) {
				return out, fmt.Errorf("%w: remote checker exit %d: %s; %s", errMergeCheckEnvironment, rc, mergeCheckHostDiagnostic(out), summarizeMergeCheckOutput(out))
			}
			if rc != 0 {
				return out, fmt.Errorf("remote check exited %d", rc)
			}
			return out, nil
		}
		return runReviewedMergeChecksWith(ctx, dir, base, head, run, false)
	}
	if err := mergeLocalHeadroom(dir); err != nil {
		return mergeValidationUnavailable("%v", err), github.SquashTooling
	}
	// TMPDIR and the compiler/linter caches live on the checked filesystem,
	// outside the source snapshot, not in the desk's quota-limited default temp.
	scratch, err := os.MkdirTemp(filepath.Dir(dir), "environment-*")
	if err != nil {
		return mergeValidationUnavailable("creating check scratch: %v", err), github.SquashTooling
	}
	defer func() { _ = os.RemoveAll(scratch) }() //nolint:gosec // owned temporary checker environment
	env := []string{"TMPDIR=" + scratch, "GOTMPDIR=" + scratch, "GOCACHE=" + filepath.Join(scratch, "go-cache"), "GOLANGCI_LINT_CACHE=" + filepath.Join(scratch, "lint-cache"), "GOMAXPROCS=2"}
	run := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		return runMergeCheckCmdEnv(ctx, dir, env, name, args...)
	}
	return runReviewedMergeChecksWith(ctx, dir, base, head, run, true)
}
