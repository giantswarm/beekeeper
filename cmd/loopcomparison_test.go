package cmd

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	comparisonStarts    = "SUPERVISOR WOULD NEXT fill 20 to capacity.ceiling (first: https://github.com/o/r/issues/1)\n"
	testCandidateBinary = "candidate"
	wantRetryable       = "retryable"
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
		{name: "budget held", before: comparisonStarts, after: "SUPERVISOR GITHUB HELD", missing: wantRetryable},
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
			err := compareLoopPasses(t.Context(), workerGitHubCurrentKey, testCandidateBinary, "fixture.yaml")
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
