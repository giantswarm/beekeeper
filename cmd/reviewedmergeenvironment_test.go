//nolint:goconst // Keep failure scenarios and wire-protocol fixtures readable.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/remotebuild"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestMergeEnvironmentRetryAndCIProof(t *testing.T) {
	const (
		modeQuotaPass      = "quota then pass"
		modeQuotaCI        = "quota then CI"
		modeQuotaPersists  = "quota persists"
		modeCodeFailure    = "code failure"
		modeRunnerDown     = "runner unavailable"
		modeCIAlreadyGreen = "CI already green"
	)
	for _, mode := range []string{modeQuotaPass, modeQuotaCI, modeQuotaPersists, modeCodeFailure, modeRunnerDown, modeCIAlreadyGreen} {
		t.Run(mode, func(t *testing.T) {
			repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, map[string]string{golangCILintYAML: "version: '2'\n"})
			base := repo.advanceMain(t, repo.head1)
			r := newLoopRig(t)
			r.a.cfg.StateDir = r.a.store.Dir()
			r.a.cfg.Disk.Checkouts = []string{repo.dir}
			r.a.cfg.Run.Remote.Enabled = true
			oldRun := mergeRemoteCheck
			t.Cleanup(func() { mergeRemoteCheck = oldRun })
			mergeLocalHeadroom = func(string) error { t.Fatal("remote validation inspected local load"); return nil }
			var dirs, commands []string
			mergeRemoteCheck = func(_ context.Context, runner remotebuild.Runner, argv []string) (bool, int) {
				if runner.Dir == repo.dir || !filepath.IsAbs(runner.Dir) {
					t.Fatalf("did not select isolated merge worktree: %s", runner.Dir)
				}
				for _, file := range []string{testGuardPR1File, testGuardPR2File} {
					if _, err := os.Stat(filepath.Join(runner.Dir, file)); err != nil {
						t.Fatal(err)
					}
				}
				if !slices.Contains(dirs, runner.Dir) {
					dirs = append(dirs, runner.Dir)
				}
				commands = append(commands, strings.Join(argv, " "))
				if argv[0] == mergeLintCommand && !strings.Contains(commands[len(commands)-1], "--config .golangci.yaml") {
					t.Fatalf("remote lint config is not worktree-relative: %v", argv)
				}
				switch {
				case mode == modeRunnerDown:
					return false, 0
				case mode == modeCodeFailure:
					if argv[0] == mergeLintCommand {
						return true, 0
					}
					_, _ = fmt.Fprintln(runner.Stdout, "--- FAIL: TestCombinedRegression")
					return true, 1
				case mode == modeQuotaPersists || len(commands) == 1:
					_, _ = fmt.Fprintln(runner.Stdout, "compile: writing output: disk quota exceeded (typecheck)")
					return true, 1
				default:
					return true, 0
				}
			}
			reviewedMergeCI = func(_ context.Context, _ github.GH, gotRepo, gotBase, gotSHA, gotHead string, number int) (bool, error) {
				if gotRepo != loopMergeRepo || gotBase != mainBranch || gotSHA != base || gotHead != repo.head2 || number != 8 {
					t.Fatal("CI proof lost exact merge identity")
				}
				return mode == modeCIAlreadyGreen || mode == modeQuotaCI && len(commands) > 0, nil
			}
			checked, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
			wantCode, wantDirs := 0, 2
			switch mode {
			case modeCodeFailure:
				wantCode, wantDirs = github.SquashRed, 1
			case modeQuotaPersists, modeRunnerDown:
				wantCode = github.SquashTooling
			case modeQuotaCI:
				wantDirs = 1
			case modeCIAlreadyGreen:
				wantDirs = 0
			}
			if !checked || code != wantCode || len(dirs) != wantDirs {
				t.Fatalf("checked=%v code=%d dirs=%v: %s", checked, code, dirs, reason)
			}
			if mode == modeQuotaPass && (len(commands) != 3 || commands[2] != "go test -short ./...") {
				t.Fatalf("missing full-module check: %v", commands)
			}
			for _, dir := range dirs {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("attempt worktree retained: %s", dir)
				}
			}
			events, err := r.a.store.Events(0, func(e state.Event) bool {
				return e.Verb == loop.LoopFileVerb && strings.Contains(e.Detail, "merge environment")
			})
			if err != nil {
				t.Fatal(err)
			}
			wantFindings := 1
			if mode == modeCodeFailure || mode == modeCIAlreadyGreen {
				wantFindings = 0
			}
			if len(events) != wantFindings {
				t.Fatalf("environment findings: %v", events)
			}
		})
	}
}

func TestMergeCheckOverloadDoesNotRunLocalTools(t *testing.T) {
	r := newLoopRig(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldLoad, oldCPU, oldDisk := mergeReadLoad, mergeNumCPU, mergeReadDisk
	t.Cleanup(func() {
		mergeReadLoad, mergeNumCPU, mergeReadDisk = oldLoad, oldCPU, oldDisk
	})
	mergeReadLoad = func() ([3]float64, error) { return [3]float64{47.5, 40.0, 30.0}, nil }
	mergeNumCPU = func() int { return 36 }
	mergeReadDisk = func(string) (machine.Disk, error) { return machine.Disk{FreeMiB: 32 * 1024}, nil }
	reason, code := r.a.runReviewedMergeChecks(t.Context(), dir, "", mainBranch, loopHead)
	if code != github.SquashTooling || !strings.Contains(reason, "coordinator overloaded (load 47.5, CPUs 36)") {
		t.Fatalf("%d: %s", code, reason)
	}
	mergeReadLoad = func() ([3]float64, error) { return [3]float64{1.0, 1.0, 1.0}, nil }
	if err := mergeLocalHeadroom(dir); err != nil {
		t.Fatalf("mergeLocalHeadroom under healthy load: %v", err)
	}
	mergeReadDisk = func(string) (machine.Disk, error) { return machine.Disk{FreeMiB: 4096}, nil }
	if err := mergeLocalHeadroom(dir); err == nil || !strings.Contains(err.Error(), "check filesystem has 4096 MiB free") {
		t.Fatalf("mergeLocalHeadroom under low disk: %v", err)
	}
}

// The production loop still requires review and green head checks. Once those
// hold, coordinator load and one remote quota failure cannot hold a proven merge.
func TestLoopMergesUnderLoadAfterCleanEnvironmentRetry(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	repo.advanceMain(t, repo.head1)
	r := newLoopRig(t)
	reviewedMergeCheck = checkReviewedMerge
	r.a.cfg.StateDir = r.a.store.Dir()
	r.a.cfg.Disk.Checkouts = []string{repo.dir}
	r.a.cfg.Run.Remote.Enabled = true
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.pull = superloop.Pull{Ref: loopPR, URL: loopURL, State: superloop.Open, Base: mainBranch, Head: repo.head2, Green: true, Author: loopDesk}
	mergeLocalHeadroom = func(string) error { return errors.New("coordinator overloaded") }
	oldRun := mergeRemoteCheck
	t.Cleanup(func() { mergeRemoteCheck = oldRun })
	calls := 0
	mergeRemoteCheck = func(_ context.Context, runner remotebuild.Runner, _ []string) (bool, int) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprintln(runner.Stdout, "disk quota exceeded")
			return true, 1
		}
		return true, 0
	}
	r.command = func(argv []string) (string, int) {
		if len(argv) > 3 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == superloop.KindMerge {
			if !slices.Contains(argv, repo.head2) {
				t.Fatal("lost exact-head guard")
			}
			r.pull.MergeCommit = repo.advanceMain(t, repo.head2)
			r.pull.State = superloop.Merged
		}
		return "", 0
	}
	parkMergeCheckPR(t, r, loopWorker, loopPR, loopURL, repo.head2)
	r.pass(t)
	if calls != 0 {
		t.Fatal("validation ran before review")
	}
	r.verdict("R1", repo.head2, superloop.Approved, "")
	r.pass(t)
	if r.pull.State != superloop.Merged || calls != 3 {
		t.Fatalf("merge held after successful clean retry: state=%s calls=%d", r.pull.State, calls)
	}
	if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
		t.Fatalf("environment failure requested branch repair: %v", wake)
	}
}
