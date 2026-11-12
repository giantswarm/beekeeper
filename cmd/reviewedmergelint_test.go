package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/remotebuild"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// Exercise the real merge attempt and remote routing: main advanced after the
// PR forked, and its existing lint must neither refuse the merge nor create a
// branch-repair outcome. New findings in unchanged files still refuse it.
func TestMergeLintBaseline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		newCount  int
		badBase   bool
		wantCode  int
		wantTests bool
	}{
		{name: "existing main finding", wantTests: true},
		{name: "new finding in unchanged test", newCount: 1, wantCode: github.SquashRed},
		{name: "duplicate finding", newCount: 2, wantCode: github.SquashRed},
		{name: "unreadable baseline", badBase: true, wantCode: github.SquashTooling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
			base := repo.advanceMain(t, repo.head1)
			r := newLoopRig(t)
			r.a.cfg.StateDir = r.a.store.Dir()
			r.a.cfg.Disk.Checkouts = []string{repo.dir}
			r.a.cfg.Run.Remote.Enabled = true
			oldRun := mergeRemoteCheck
			t.Cleanup(func() { mergeRemoteCheck = oldRun })
			var baselineDir string
			tested := false
			mergeRemoteCheck = func(ctx context.Context, runner remotebuild.Runner, argv []string) (bool, int) {
				if argv[0] == "go" {
					tested = true
					if _, err := os.Stat(filepath.Join(runner.Dir, testGuardPR2File)); err != nil {
						t.Fatal("tests did not use the merged source")
					}
					return true, 0
				}
				if !slices.Contains(argv, "--output.json.path=stdout") {
					_, _ = fmt.Fprintln(runner.Stdout, "existing main lint")
					return true, 1
				}
				isBase := strings.HasPrefix(filepath.Base(runner.Dir), "lint-base-")
				issue := mergeLintIssue{FromLinter: "errcheck", Text: "unchecked receive", SourceLines: []string{"<-done"}}
				issue.Pos.Filename = "guard/unchanged_test.go"
				issue.Pos.Line = 457
				issues := []mergeLintIssue{issue}
				if isBase {
					baselineDir = runner.Dir
					out, err := runGit(ctx, runner.Dir, "rev-parse", "HEAD")
					if err != nil || strings.TrimSpace(string(out)) != base {
						t.Fatalf("baseline is not fetched main: %s %v", out, err)
					}
					if tc.badBase {
						_, _ = fmt.Fprintln(runner.Stdout, "missing JSON report")
						return true, 0
					}
				} else {
					issues[0].Pos.Line += 10 // An unrelated insertion is not a regression.
					if tc.newCount > 0 {
						if tc.newCount == 1 {
							issue.Text = "new finding in unchanged file"
						}
						issues = append(issues, issue)
					}
				}
				if err := json.NewEncoder(runner.Stdout).Encode(map[string]any{"Issues": issues}); err != nil {
					t.Fatal(err)
				}
				return true, 0
			}
			checked, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
			if !checked || code != tc.wantCode || tested != tc.wantTests {
				t.Fatalf("checked=%v code=%d tests=%v: %s", checked, code, tested, reason)
			}
			if baselineDir == "" {
				t.Fatal("did not compare the baseline")
			}
			if _, err := os.Stat(baselineDir); !os.IsNotExist(err) {
				t.Fatalf("baseline worktree retained: %v", err)
			}
		})
	}
}
