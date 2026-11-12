package cmd

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	comparisonStarts    = "SUPERVISOR WOULD NEXT fill 20 to capacity.ceiling (first: https://github.com/o/r/issues/1)\n"
	testCandidateBinary = "candidate"
	wantRetryable       = "retryable"
	// comparisonIncomplete is the error of a dry-run pass that could not read
	// every observation it needs, as the running release prints it.
	comparisonIncomplete = "beekeeper: dry-run observations incomplete; retry later"
	comparisonHeld       = "SUPERVISOR GITHUB HELD"
)

func TestLoopComparisonRejectsLostTransitionsAndRetriesFailures(t *testing.T) {
	original := runLoopComparison
	t.Cleanup(func() { runLoopComparison = original })
	for _, tc := range []struct {
		name, before, after, missing string
		failure                      error
	}{
		{name: "healthy-pass", before: comparisonStarts, after: comparisonStarts},
		{name: "no starts", before: comparisonStarts, missing: labStart},
		{name: "no continuations", before: "SUPERVISOR WOULD CONTINUE worker-1", missing: loopContinuationTransition},
		{name: "no reviews", before: "SUPERVISOR WOULD REVIEW pull", missing: taskKindReview},
		{name: "no merges", before: "SUPERVISOR WOULD GO pull", missing: gitMerge},
		{name: "deadline exceeded", before: comparisonStarts, failure: context.DeadlineExceeded, missing: wantRetryable},
		{name: "github error", before: comparisonStarts, failure: errors.New("comparison GitHub unavailable"), missing: wantRetryable},
		{name: "budget held", before: comparisonStarts, after: comparisonHeld, missing: wantRetryable},
		{name: "zero fill", before: "SUPERVISOR WOULD NEXT fill 0 to capacity.ceiling", after: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runLoopComparison = func(ctx context.Context, binary string, args ...string) (string, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > loopComparisonTimeout {
					return "", errors.New("missing own deadline")
				}
				if strings.Join(args, " ") != "--config fixture.yaml supervisor loop --once --dry-run" {
					return "", errors.New("wrong read-only invocation")
				}
				if binary == workerGitHubCurrentKey {
					return tc.before, nil
				}
				return tc.after, tc.failure
			}
			_, err := compareLoopPasses(t.Context(), workerGitHubCurrentKey, testCandidateBinary, "fixture.yaml")
			if tc.missing == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.missing) {
				t.Fatalf("want %s, got %v", tc.missing, err)
			}
		})
	}
}

func TestSelfUpdateComparisonIsRetryableAndExplicitlyOverridable(t *testing.T) {
	const transientErrorMode = "transient-error"
	oldUpdate, oldCanary, oldPass := runUpdate, runCanaryCheck, runLoopComparison
	t.Cleanup(func() { runUpdate, runCanaryCheck, runLoopComparison = oldUpdate, oldCanary, oldPass })
	for _, mode := range []string{throughputHealthy, "regression", transientErrorMode, urlOverrideMode} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_STATE_HOME", dir)
			cfg := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
			runCanaryCheck = func(context.Context, string, ...string) (string, error) { return "ok", nil }
			runLoopComparison = func(_ context.Context, binary string, _ ...string) (string, error) {
				if binary != testCandidateBinary || mode == throughputHealthy {
					return comparisonStarts, nil
				}
				if mode == transientErrorMode {
					return "", errors.New("comparison probe unavailable")
				}
				return "", nil
			}
			installed := false
			runUpdate = func(ctx context.Context, _ io.Writer, _ bool) (update.Result, error) {
				opts := update.OptionsFrom(ctx)
				err := opts.Canary(ctx, testCandidateBinary, "v9.9.9")
				if opts.Skip("v9.9.9") {
					t.Fatal("comparison quarantined a retryable candidate")
				}
				installed = err == nil
				return update.Result{}, err
			}
			root := New()
			args := []string{workerGitHubConfigOption, cfg, selfUpdateCmdName}
			if mode == urlOverrideMode {
				args = append(args, "--allow-loop-regression")
			}
			root.SetArgs(args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			wantInstall := mode == throughputHealthy || mode == urlOverrideMode
			if installed != wantInstall || (err == nil) != wantInstall {
				t.Fatalf("mode %s: installed=%v, err=%v", mode, installed, err)
			}
		})
	}
}

// The running release's own pass is only the baseline: when it cannot
// complete, it holds back no candidate, even one that carries its fix
// (https://github.com/giantswarm/beekeeper/issues/3813). Only a failing
// candidate pass refuses then.
func TestLoopComparisonWithoutBaselineLeavesTheCandidateToItsOwnPass(t *testing.T) {
	const (
		incomplete = "SUPERVISOR PULL WATCH held: invalid character '<' looking for beginning of value; retrying with backoff up to 10m0s\n" +
			comparisonIncomplete + "\n"
		held        = comparisonHeld + ": GraphQL budget unknown or under github.floor; no board, ownership or PR reads until a fresh reading recovers\n"
		incomplete1 = "exit status 1: " + comparisonIncomplete
	)
	exit1, exit2 := errors.New("exit status 1"), errors.New("exit status 2")
	original := runLoopComparison
	t.Cleanup(func() { runLoopComparison = original })
	type pass struct {
		out string
		err error
	}
	for _, tc := range []struct {
		name              string
		before, after     pass
		wantErr, wantNote []string
	}{
		{
			name:   "neither pass observes the desk",
			before: pass{incomplete, exit1}, after: pass{incomplete, exit1},
			wantNote: []string{incomplete1, "candidate's dry-run did not complete either"},
		},
		{
			name:   "running release held, candidate completes",
			before: pass{held, nil}, after: pass{comparisonStarts, nil},
			wantNote: []string{comparisonHeld, "candidate's dry-run completed"},
		},
		{
			name:   "running release runs out of time",
			before: pass{"SUPERVISOR WOULD CONTINUE worker-1\n", context.DeadlineExceeded}, after: pass{comparisonStarts, nil},
			wantNote: []string{"ran out of its 2m0s after: SUPERVISOR WOULD CONTINUE worker-1"},
		},
		{
			name:   "running release fails",
			before: pass{"beekeeper: reading the event log: boom\n", exit1}, after: pass{comparisonStarts, nil},
			wantNote: []string{"exit status 1: beekeeper: reading the event log: boom"},
		},
		{
			name:   "candidate fails without a baseline",
			before: pass{incomplete, exit1}, after: pass{"panic: boom\n", exit2},
			wantErr: []string{"candidate dry-run unavailable (retryable): exit status 2: panic: boom"},
		},
		{
			name:   "candidate cannot observe what the running release observed",
			before: pass{comparisonStarts, nil}, after: pass{incomplete, exit1},
			wantErr: []string{"candidate dry-run unavailable (retryable): " + incomplete1, "the running release's completed"},
		},
		{
			name:   "candidate runs out of time after a complete running release",
			before: pass{comparisonStarts, nil}, after: pass{"", context.DeadlineExceeded},
			wantErr: []string{"candidate dry-run unavailable (retryable): ran out of its 2m0s"},
		},
		{
			name:   "candidate held after a complete running release",
			before: pass{comparisonStarts, nil}, after: pass{held, nil},
			wantErr: []string{"candidate dry-run unavailable (retryable)", comparisonHeld},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runLoopComparison = func(_ context.Context, binary string, _ ...string) (string, error) {
				if binary == workerGitHubCurrentKey {
					return tc.before.out, tc.before.err
				}
				return tc.after.out, tc.after.err
			}
			note, err := compareLoopPasses(t.Context(), workerGitHubCurrentKey, testCandidateBinary, "")
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("want a refusal, got none (note %q)", note)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q does not say %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("a missing baseline refused the candidate: %v", err)
			}
			if !strings.Contains(note, "no baseline") {
				t.Fatalf("note %q does not say the candidate went uncompared", note)
			}
			for _, want := range tc.wantNote {
				if !strings.Contains(note, want) {
					t.Fatalf("note %q does not say %q", note, want)
				}
			}
		})
	}
}

// A comparison cut short by its caller installs nothing: only its own
// deadline counts as a pass that could not observe the desk.
func TestLoopComparisonStopsWithItsCaller(t *testing.T) {
	original := runLoopComparison
	t.Cleanup(func() { runLoopComparison = original })
	ran := 0
	runLoopComparison = func(ctx context.Context, _ string, _ ...string) (string, error) {
		ran++
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	note, err := compareLoopPasses(ctx, workerGitHubCurrentKey, testCandidateBinary, "")
	if !errors.Is(err, context.Canceled) || note != "" || ran != 1 {
		t.Fatalf("canceled comparison = %q, %v after %d passes", note, err, ran)
	}
}

// archon's case: the running release's dry-run cannot observe the desk, so
// self-update installs the candidate on its own pass, says so and logs it.
func TestSelfUpdateInstallsWithoutABaselineAndSaysSo(t *testing.T) {
	oldUpdate, oldCanary, oldPass := runUpdate, runCanaryCheck, runLoopComparison
	t.Cleanup(func() { runUpdate, runCanaryCheck, runLoopComparison = oldUpdate, oldCanary, oldPass })
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	cfg := emptyTestConfig(t, filepath.Join(dir, "config.yaml"))
	runCanaryCheck = func(context.Context, string, ...string) (string, error) { return "ok", nil }
	runLoopComparison = func(_ context.Context, binary string, _ ...string) (string, error) {
		if binary == testCandidateBinary {
			return comparisonStarts, nil
		}
		return comparisonIncomplete + "\n", errors.New("exit status 1")
	}
	installed := false
	runUpdate = func(ctx context.Context, _ io.Writer, _ bool) (update.Result, error) {
		opts := update.OptionsFrom(ctx)
		err := opts.Canary(ctx, testCandidateBinary, "v9.9.9")
		if opts.Skip("v9.9.9") {
			t.Fatal("comparison quarantined the candidate")
		}
		installed = err == nil
		return update.Result{}, err
	}
	var out strings.Builder
	root := New()
	root.SetArgs([]string{workerGitHubConfigOption, cfg, selfUpdateCmdName})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil || !installed {
		t.Fatalf("self-update without a baseline: installed=%v, err=%v", installed, err)
	}
	if !strings.Contains(out.String(), "v9.9.9 is not compared with the running release: its dry-run gives no baseline (exit status 1: "+comparisonIncomplete+")") {
		t.Fatalf("output does not say the candidate went uncompared:\n%s", out.String())
	}
	store, err := state.Open(filepath.Join(dir, "beekeeper"))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == verbSelfUpdateComparison })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "v9.9.9 is not compared") {
		t.Fatalf("comparison events = %+v", evs)
	}
}
