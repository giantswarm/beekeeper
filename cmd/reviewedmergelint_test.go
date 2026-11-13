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
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/remotebuild"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// golangci-lint runs before go test and reports a package whose cgo build
// stops on a native dependency the checker host lacks as a typecheck finding
// quoting go's build output, on the package itself and on each package
// importing it. When those are all the merge result's new findings, lint shows
// nothing about the branch: go test decides, and a host that cannot build the
// repository files one environment finding while the pull request's own checks
// stand. A typecheck finding quoting a compile error or a header the repository
// tracks, or any other new finding beside it, still refuses the merge.
func TestMergeResultLintOfMissingHostBuildDependencyIsNotABranchRepair(t *testing.T) {
	const (
		alsaBuild = "# example.com/r/midi\n./midi.go:4:10: fatal error: alsa/asoundlib.h: No such file or directory\n" +
			"    4 | // #include <alsa/asoundlib.h>\n      |          ^~~~~~~~~~~~~~~~~~\ncompilation terminated."
		alsaTest = alsaBuild + "\nFAIL\texample.com/r/midi [build failed]\nFAIL\n"
	)
	typecheck := func(file, text string) mergeLintIssue {
		issue := mergeLintIssue{FromLinter: "typecheck", Text: text, SourceLines: []string{"package midi"}}
		issue.Pos.Filename = file
		issue.Pos.Line = 1
		return issue
	}
	own := typecheck("midi/b.go", ": # example.com/r/midi\n# [example.com/r/midi]\n"+strings.TrimPrefix(alsaBuild, "# example.com/r/midi\n"))
	importer := typecheck("cmd/main.go", "could not import example.com/r/midi (-: "+alsaBuild+")")
	errcheck := mergeLintIssue{FromLinter: "errcheck", Text: "unchecked receive", SourceLines: []string{"<-done"}}
	errcheck.Pos.Filename = "guard/pr2.go"
	for name, tc := range map[string]struct {
		issues        []mergeLintIssue
		goTest        string
		missing, tool string
		wantTests     bool
	}{
		"on the package and its importer": {issues: []mergeLintIssue{own, importer}, goTest: alsaTest, missing: "C header alsa/asoundlib.h", tool: "go test", wantTests: true},
		"go test passes":                  {issues: []mergeLintIssue{importer}, missing: "C header alsa/asoundlib.h", tool: "golangci-lint", wantTests: true},
		"beside another new finding":      {issues: []mergeLintIssue{own, errcheck}},
		"quoting a compile error":         {issues: []mergeLintIssue{typecheck("guard/pr2.go", ": # example.com/r/guard\nguard/pr2.go:2:5: undefined: A")}},
		"of a tracked header": {issues: []mergeLintIssue{typecheck("guard/pr2.go",
			": # example.com/r/guard\nguard/pr2.go:3:10: fatal error: native.h: No such file or directory\ncompilation terminated.")}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA},
				map[string]string{testGuardPR2File: testGuardSourceB, "guard/native.h": "int native(void);\n"}, nil)
			repo.advanceMain(t, repo.head1)
			r := newLoopRig(t)
			r.a.cfg.StateDir = r.a.store.Dir()
			r.a.cfg.Disk.Checkouts = []string{repo.dir}
			r.a.cfg.Run.Remote.Enabled = true
			oldRun := mergeRemoteCheck
			t.Cleanup(func() { mergeRemoteCheck = oldRun })
			tests := 0
			mergeRemoteCheck = func(_ context.Context, runner remotebuild.Runner, argv []string) (bool, int) {
				switch {
				case argv[0] == "go":
					tests++
					if tc.goTest == "" {
						return true, 0
					}
					_, _ = fmt.Fprint(runner.Stdout, tc.goTest)
					return true, 1
				case !slices.Contains(argv, "--output.json.path=stdout"):
					_, _ = fmt.Fprintln(runner.Stdout, "b.go:1: : # example.com/r/midi (typecheck)")
					return true, 1
				}
				issues := tc.issues
				if strings.HasPrefix(filepath.Base(runner.Dir), "lint-base-") {
					issues = []mergeLintIssue{}
				}
				if err := json.NewEncoder(runner.Stdout).Encode(map[string]any{"Issues": issues}); err != nil {
					t.Fatal(err)
				}
				return true, 0
			}
			checked, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
			findings, err := r.a.store.Events(0, func(e state.Event) bool { return e.Verb == loop.LoopFileVerb })
			if err != nil {
				t.Fatal(err)
			}
			if (tests == 1) != tc.wantTests {
				t.Fatalf("go test ran %d times: %d: %s", tests, code, reason)
			}
			if tc.missing == "" {
				if !checked || code != github.SquashRed || !strings.Contains(reason, "failed golangci-lint") || len(findings) != 0 {
					t.Fatalf("the merge result's own lint finding was not returned to its branch: %d: %s; findings %v", code, reason, findings)
				}
				return
			}
			if !checked || code != 0 || !strings.HasPrefix(reason, "merge-result validation skipped: ") || !strings.Contains(reason, tc.missing) ||
				!strings.Contains(reason, "that "+tc.tool+" of") || !strings.Contains(reason, "own checks stand") {
				t.Fatalf("a host without %s refused or repaired the branch: %d: %s", tc.missing, code, reason)
			}
			if len(findings) != 1 || !strings.Contains(findings[0].Detail, "not a branch repair") || !strings.Contains(findings[0].Detail, tc.missing) {
				t.Fatalf("want one environment finding naming %s, got %v", tc.missing, findings)
			}
		})
	}
}

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
