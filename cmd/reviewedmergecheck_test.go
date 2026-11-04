package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	secondPR          = "o/r#8"
	secondURL         = "https://github.com/o/r/pull/8"
	secondWorker      = "worker two"
	testGuardBaseFile = "guard/base.go"
	testGuardPR1File  = "guard/pr1.go"
	testGuardPR2File  = "guard/pr2.go"
	testGuardCostFile = "guard/cost.go"
	testGuardSourceA  = "package guard\nvar A=1\n"
	testGuardSourceB  = "package guard\nvar B=2\n"
)

// mergeCheckRepo is a real git repository in a scratch directory backing
// loopMergeRepo ("o/r") with a Go module and two PR branches from main.
type mergeCheckRepo struct {
	dir       string
	originDir string
	base      string
	head1     string
	head2     string
}

func newMergeCheckRepo(t *testing.T, pr1Files, pr2Files map[string]string, baseFiles map[string]string) *mergeCheckRepo {
	t.Helper()
	// Model the configured lint command independently of the test runner image.
	lintDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(lintDir, "golangci-lint"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
		t.Fatal(err)
	}
	t.Setenv("PATH", lintDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	dir := filepath.Join(tmp, "r")
	if err := os.MkdirAll(filepath.Join(dir, "guard"), 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test git fixture
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "--bare", originDir)
	git("init", "-q", "-b", mainBranch)
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	git("remote", loopAdd, "origin", originDir)
	git("remote", "set-url", "--push", "origin", "https://github.com/"+loopMergeRepo+".git")

	write("go.mod", "module example.com/r\n\ngo 1.24\n")
	for k, v := range baseFiles {
		write(k, v)
	}
	git(loopAdd, ".")
	git("commit", "-q", "-m", "initial")
	base := git("rev-parse", "HEAD")
	git("push", "-q", originDir, mainBranch)

	git("checkout", "-q", "-b", "pr-1", base)
	for k, v := range pr1Files {
		write(k, v)
	}
	git(loopAdd, ".")
	git("commit", "-q", "-m", "pr 1")
	head1 := git("rev-parse", "HEAD")

	git("checkout", "-q", "-b", "pr-2", base)
	for k, v := range pr2Files {
		write(k, v)
	}
	git(loopAdd, ".")
	git("commit", "-q", "-m", "pr 2")
	head2 := git("rev-parse", "HEAD")

	git("checkout", "-q", mainBranch)
	git("update-ref", "refs/remotes/origin/"+mainBranch, base)
	return &mergeCheckRepo{dir: dir, originDir: originDir, base: base, head1: head1, head2: head2}
}

// advanceMain simulates GitHub merging head into main on origin.
func (m *mergeCheckRepo) advanceMain(t *testing.T, head string) string {
	t.Helper()
	git := func(args ...string) string {
		t.Helper()
		c := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test git fixture
		c.Dir = m.dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("checkout", "-q", mainBranch)
	git(gitMerge, "--no-ff", "--no-edit", head)
	sha := git("rev-parse", "HEAD")
	git("push", "-q", m.originDir, mainBranch)
	git("update-ref", "refs/remotes/origin/"+mainBranch, sha)
	return sha
}

func parkMergeCheckPR(t *testing.T, r *loopRig, worker, on, url, head string) {
	t.Helper()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == worker })
		if i < 0 {
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: worker}, Task: "fix " + url})
			i = len(st.Agents) - 1
		}
		st.Agents[i].Park = &state.Park{On: on, At: time.Now(), Waits: deskReviewWait, ReviewHead: head}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Two PRs that are each green on their own but together fail golangci-lint
// (goconst) or go test never both reach main: the first merges and advances
// main, while the second is refused on the combined merge result (PR + current
// main) and returned to its owner with the failing check.
func TestMergeLaneRunsCIOnTheMergeResultAndReturnsAFailingCombinationToItsOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		baseFiles map[string]string
		pr1Files  map[string]string
		pr2Files  map[string]string
		wantCheck string
	}{
		"goconst across two PRs": {
			baseFiles: map[string]string{
				testGuardBaseFile: "package guard\n\nfunc Base() string { return \"ok\" }\n",
			},
			pr1Files: map[string]string{
				testGuardPR1File: "package guard\n\nfunc One(x string) bool {\n\tif x == \"agy-harness\" {\n\t\treturn true\n\t}\n\treturn x == \"agy-harness\" && Base() != \"\"\n}\n",
			},
			pr2Files: map[string]string{
				testGuardPR2File: "package guard\n\nfunc Two(y string) bool {\n\tif y == \"agy-harness\" {\n\t\treturn true\n\t}\n\treturn y == \"agy-harness\" && Base() != \"\"\n}\n",
			},
			wantCheck: "goconst",
		},
		"go test failure across two PRs": {
			baseFiles: map[string]string{
				testGuardCostFile:    "package guard\n\nvar costA, costB int\n\nfunc TotalCost() int { return costA + costB }\n",
				"guard/cost_test.go": "package guard\n\nimport \"testing\"\n\nfunc TestBoardCostStayWithinBudget(t *testing.T) {\n\tif got := TotalCost(); got > 40 {\n\t\tt.Fatalf(\"cost %d exceeds budget 40\", got)\n\t}\n}\n",
			},
			pr1Files: map[string]string{
				testGuardPR1File: "package guard\n\nfunc init() { costA = 25 }\n",
			},
			pr2Files: map[string]string{
				testGuardPR2File: "package guard\n\nfunc init() { costB = 16 }\n",
			},
			wantCheck: "TestBoardCostStayWithinBudget",
		},
		"compatible PRs both merge": {
			baseFiles: map[string]string{
				testGuardCostFile:    "package guard\n\nvar costA, costB int\n\nfunc TotalCost() int { return costA + costB }\n",
				"guard/cost_test.go": "package guard\n\nimport \"testing\"\n\nfunc TestBoardCostStayWithinBudget(t *testing.T) {\n\tif got := TotalCost(); got > 40 {\n\t\tt.Fatalf(\"cost %d exceeds budget 40\", got)\n\t}\n}\n",
			},
			pr1Files: map[string]string{
				testGuardPR1File: "package guard\n\nfunc init() { costA = 20 }\n",
			},
			pr2Files: map[string]string{
				testGuardPR2File: "package guard\n\nfunc init() { costB = 15 }\n",
			},
			wantCheck: "",
		},
		"conflicting PRs after first merge": {
			baseFiles: map[string]string{
				testGuardBaseFile: "package guard\n\nfunc Name() string { return \"initial\" }\n",
			},
			pr1Files: map[string]string{
				testGuardBaseFile: "package guard\n\nfunc Name() string { return \"from-pr-1\" }\n",
			},
			pr2Files: map[string]string{
				testGuardBaseFile: "package guard\n\nfunc Name() string { return \"from-pr-2\" }\n",
			},
			wantCheck: "conflicts with its base",
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newMergeCheckRepo(t, tc.pr1Files, tc.pr2Files, tc.baseFiles)

			// Prove each PR is green on its own against the initial base.
			for i, head := range []string{repo.head1, repo.head2} {
				c := exec.CommandContext(t.Context(), "git", "checkout", "-q", head) //nolint:gosec // test fixture
				c.Dir = repo.dir
				if err := c.Run(); err != nil {
					t.Fatal(err)
				}
				if reason, code := runReviewedMergeChecks(t.Context(), repo.dir, repo.base, mainBranch, head); code != 0 {
					t.Fatalf("PR %d not green on its own: exit %d, %s", i+1, code, reason)
				}
			}
			c := exec.CommandContext(t.Context(), "git", "checkout", "-q", mainBranch) //nolint:gosec // test fixture
			c.Dir = repo.dir
			if err := c.Run(); err != nil {
				t.Fatal(err)
			}

			r := newLoopRig(t)
			reviewedMergeCheck = checkReviewedMerge
			r.a.cfg.Disk.Checkouts = []string{repo.dir}
			r.a.cfg.Merge.Admin = []string{loopMergeRepo}

			// Set up PR 1 (o/r#7) and PR 2 (o/r#8), each approved at its own green head.
			r.pull = superloop.Pull{
				Ref: loopPR, URL: loopURL, State: superloop.Open,
				Base: mainBranch, Head: repo.head1, Green: true, Author: loopDesk,
			}
			r.pulls[secondPR] = superloop.Pull{
				Ref: secondPR, URL: secondURL, State: superloop.Open,
				Base: mainBranch, Head: repo.head2, Green: true, Author: loopDesk,
			}

			var mergedHeads []string
			r.command = func(argv []string) (string, int) {
				if len(argv) > 3 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == superloop.KindMerge {
					switch argv[3] {
					case "7":
						merged := repo.advanceMain(t, repo.head1)
						r.pull.State = superloop.Merged
						r.pull.MergeCommit = merged
						mergedHeads = append(mergedHeads, repo.head1)
					case "8":
						merged := repo.advanceMain(t, repo.head2)
						p2 := r.pulls[secondPR]
						p2.State = superloop.Merged
						p2.MergeCommit = merged
						r.pulls[secondPR] = p2
						mergedHeads = append(mergedHeads, repo.head2)
					}
				}
				return "", 0
			}
			// Route gate --reviewed-head for either PR #7 or PR #8 to reviewedMerge.
			prevRun := beekeeperRun
			t.Cleanup(func() { beekeeperRun = prevRun })
			beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
				if g := slices.Index(args, gateCmdName); g >= 0 && slices.Contains(args, "8") {
					clean := args[g:]
					r.calls = append(r.calls, clean)
					idx := slices.Index(clean, "--reviewed-head")
					doc := r.a.reviewedMerge(ctx, loopMergeRepo, 8, clean[idx+1])
					b, err := json.Marshal(doc)
					if err != nil {
						t.Fatal(err)
					}
					return string(b), doc.ExitCode, nil
				}
				return prevRun(ctx, args...)
			}

			// Park and approve PR 1, then run the loop to merge PR 1.
			parkMergeCheckPR(t, r, loopWorker, loopPR, loopURL, repo.head1)
			r.pass(t)
			r.verdict("R1", repo.head1, superloop.Approved, "")
			r.pass(t)
			if r.pull.State != superloop.Merged || !slices.Equal(mergedHeads, []string{repo.head1}) {
				t.Fatalf("PR 1 did not merge: state=%s mergedHeads=%v", r.pull.State, mergedHeads)
			}

			// Now park and approve PR 2 at its green head2, then run the loop.
			parkMergeCheckPR(t, r, secondWorker, secondPR, secondURL, repo.head2)
			r.pass(t)
			p2 := r.pulls[secondPR]
			p2.Reviews = append(p2.Reviews, superloop.Review{ID: "R2", Author: loopDesk, Head: repo.head2, Verdict: superloop.Approved, At: time.Now()})
			r.pulls[secondPR] = p2
			r.pass(t)

			if tc.wantCheck == "" {
				if r.pulls[secondPR].State != superloop.Merged || !slices.Equal(mergedHeads, []string{repo.head1, repo.head2}) {
					t.Fatalf("compatible second PR did not merge: state=%s mergedHeads=%v", r.pulls[secondPR].State, mergedHeads)
				}
				if wake := r.called(agentsName, agentWakeName, secondWorker); wake != nil {
					t.Fatalf("compatible second PR woke worker: %v", wake)
				}
				return
			}

			// PR 2 must NOT be merged into main, and must be returned to its owner with the failing check.
			if r.pulls[secondPR].State == superloop.Merged || slices.Contains(mergedHeads, repo.head2) {
				t.Fatalf("second PR merged despite failing combination (%s): mergedHeads=%v", tc.wantCheck, mergedHeads)
			}
			wake := r.called(agentsName, agentWakeName, secondWorker)
			if wake == nil || !strings.Contains(wake[3], tc.wantCheck) {
				t.Fatalf("second PR was not returned to its owner with failing check %q: wake=%v calls=%v", tc.wantCheck, wake, r.calls)
			}
		})
	}
}

func TestCheckReviewedMergeResultStopsWhenDeskStoppedMeanwhile(t *testing.T) {
	r := newLoopRig(t)
	prev := reviewedMergeCheck
	t.Cleanup(func() { reviewedMergeCheck = prev })
	reviewedMergeCheck = func(_ context.Context, a *app, _ superloop.Pull, _ string) (bool, string, int) {
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Stop = &state.Stop{}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		return true, "", 0
	}
	reason, code := r.a.checkReviewedMergeResult(t.Context(), r.pull, loopHead)
	if code != github.SquashNotApplicable || reason != reasonDeskStopped {
		t.Fatalf("got (%q, %d), want (%q, %d)", reason, code, reasonDeskStopped, github.SquashNotApplicable)
	}
	if finding := checkPackageGoConst("..", "./cmd"); finding != "" {
		t.Fatalf("goconst in ./cmd: %s", finding)
	}
}

func TestMergeResultChecksCoverUnchangedAndDependentTests(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		t.Run(fmt.Sprint(dependent), func(t *testing.T) {
			files := map[string]string{testGuardCostFile: "package guard\nvar A, B int\nfunc Cost() int { return A+B }\n"}
			testDir, expression := "guard", "Cost()"
			imports := "import \"testing\"\n"
			if dependent {
				testDir, expression = "consumer", "guard.Cost()"
				imports = "import (\"testing\"; \"example.com/r/guard\")\n"
			}
			// Thirty-one test files must not switch to filename-based selection.
			for i := 0; i < 31; i++ {
				files[fmt.Sprintf("%s/padding%d_test.go", testDir, i)] = "package " + testDir + "\n"
			}
			files[testDir+"/cost_test.go"] = "package " + testDir + "\n" + imports + "func TestUnchangedBudget(t *testing.T) { if " + expression + ">40 { t.Fatal(\"budget exceeded\") } }\n"
			repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: "package guard\nfunc init(){A=25}\n"}, map[string]string{testGuardPR2File: "package guard\nfunc init(){B=16}\n"}, files)
			repo.advanceMain(t, repo.head1)
			r := newLoopRig(t)
			r.a.cfg.Disk.Checkouts = []string{repo.dir}
			_, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
			if code != github.SquashRed || !strings.Contains(reason, "TestUnchangedBudget") {
				t.Fatalf("got %d: %s", code, reason)
			}
		})
	}
}

func TestMergeResultRequiresFreshRefs(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	r := newLoopRig(t)
	r.a.cfg.Disk.Checkouts = []string{repo.dir}
	// A cached origin/main still exists but origin is now unavailable.
	c := exec.CommandContext(t.Context(), "git", "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git")) //nolint:gosec // fixed git command in the test-owned repository
	c.Dir = repo.dir
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	_, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
	if code != github.SquashTooling || !strings.Contains(reason, "fetching") {
		t.Fatalf("got %d: %s", code, reason)
	}
	r.a.cfg.Disk.Checkouts = nil
	checked, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
	if checked || code != 0 || !strings.Contains(reason, "skipped: no checkout") {
		t.Fatalf("missing checkout: %d, %s", code, reason)
	}
}

func TestMergeResultRequiresLintTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	reason, code := runReviewedMergeChecks(t.Context(), dir, "base", mainBranch, "head")
	if code != github.SquashTooling || !strings.Contains(reason, "golangci-lint unavailable") {
		t.Fatalf("got %d: %s", code, reason)
	}
}

func TestReviewedMergeQueueIsRead(t *testing.T) {
	check := readReviewedMergeQueue
	prev := github.RunGH
	t.Cleanup(func() { github.RunGH = prev })
	for _, reply := range []struct {
		body      string
		queue     bool
		wantError bool
	}{
		{body: `{"data":{"repository":{"mergeQueue":null}}}`},
		{body: `{"data":{"repository":{"mergeQueue":{"id":"queue"}}}}`, queue: true},
		{body: `{"data":{"repository":null},"errors":[{"message":"unavailable"}]}`, wantError: true},
		{body: `{"errors":[{"message":"unavailable"}]}`, wantError: true},
		{body: `invalid`, wantError: true},
	} {
		github.RunGH = func(context.Context, ...string) ([]byte, error) { return []byte(reply.body), nil }
		queue, err := check(t.Context(), superloop.Pull{Ref: secondPR, Base: mainBranch})
		if (err != nil) != reply.wantError || queue != reply.queue {
			t.Fatalf("%s: %v, %v", reply.body, queue, err)
		}
	}
}

func TestMergeResultChecksDependencyOnlyChanges(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{"go.mod": "module example.com/r\n\ngo 1.24.0\n"}, map[string]string{testGuardPR2File: testGuardSourceB}, map[string]string{"guard/budget_test.go": "package guard\nimport \"testing\"\nfunc TestDependencyCheck(t *testing.T){t.Fatal(\"dependency regression\")}\n"})
	c := exec.CommandContext(t.Context(), "git", "checkout", "-q", repo.head1) //nolint:gosec // fixed git command in the test-owned repository
	c.Dir = repo.dir
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	reason, code := runReviewedMergeChecks(t.Context(), repo.dir, repo.base, mainBranch, repo.head1)
	if code != github.SquashRed || !strings.Contains(reason, "TestDependencyCheck") {
		t.Fatalf("got %d: %s", code, reason)
	}
}

func TestFetchReviewedMergeRefsDoesNotUseCachedBase(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	want := repo.advanceMain(t, repo.head1)
	c := exec.CommandContext(t.Context(), "git", "update-ref", "refs/remotes/origin/"+mainBranch, repo.base) //nolint:gosec // fixed git command in the test-owned repository
	c.Dir = repo.dir
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := fetchReviewedMergeRefs(t.Context(), repo.dir, mainBranch, repo.head2, 8)
	if err != nil || got != want {
		t.Fatalf("got %s, %v; want %s", got, err, want)
	}
	if _, err := fetchReviewedMergeRefs(t.Context(), repo.dir, mainBranch, "0000000000000000000000000000000000000000", 8); err == nil {
		t.Fatal("unavailable head accepted")
	}
}

func TestMergeResultConfiguredLintFailure(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	lintDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(lintDir, "golangci-lint"), []byte("#!/bin/sh\necho 'configured gosec failure'\nexit 1\n"), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
		t.Fatal(err)
	}
	t.Setenv("PATH", lintDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	reason, code := runReviewedMergeChecks(t.Context(), repo.dir, repo.base, mainBranch, repo.head2)
	if code != github.SquashRed || !strings.Contains(reason, "configured gosec failure") {
		t.Fatalf("got %d: %s", code, reason)
	}
}

func TestMergeResultValidationFailureCannotDelegateAnUnsafeMerge(t *testing.T) {
	r := newLoopRig(t)
	readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) {
		return false, fmt.Errorf("merge queue unavailable")
	}
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	if len(r.cmds) != 0 {
		t.Fatalf("merge submitted despite unavailable queue: %v", r.cmds)
	}
	if call := r.called(agentsName, agentWakeName, loopWorker); call != nil {
		t.Fatalf("unsafe merge delegated: %v", call)
	}
}

func TestMergeResultUnreadableGateCannotDelegateAnUnsafeMerge(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.answer = func(args []string) (string, int) {
		if len(args) > 0 && args[0] == gateCmdName {
			return "unreadable gate result", github.SquashTooling
		}
		return "", 0
	}
	r.pass(t)
	if len(r.cmds) != 0 {
		t.Fatalf("merge submitted without validation: %v", r.cmds)
	}
	if call := r.called(agentsName, agentWakeName, loopWorker); call != nil {
		t.Fatalf("unvalidated gate delegated a worker merge: %v", call)
	}
	if evs := r.logged(t, "supervisor.go"); len(evs) != 0 {
		t.Fatalf("unsafe merge go: %+v", evs)
	}
}

func TestMergeCheckIgnoresHostLocale(t *testing.T) {
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	out, err := runMergeCheckCmd(t.Context(), t.TempDir(), "sh", "-c", `test "$LC_ALL" = C && test "$LANGUAGE" = C`)
	if err != nil {
		t.Fatalf("host locale reached check: %v: %s", err, out)
	}
}

func TestMergeCheckEnvironmentFailures(t *testing.T) {
	for name, args := range map[string][]string{
		"absent executable": {"beekeeper-test-nonexistent-command"},
		"signal":            {"sh", "-c", "kill -KILL $$"},
		"exhausted disk":    {"sh", "-c", "echo 'write: no space left on device'; exit 1"},
		"resource":          {"sh", "-c", "echo 'fork: resource temporarily unavailable'; exit 2"},
		"compiler killed":   {"sh", "-c", "echo 'compile: signal: killed'; exit 1"},
		"exhausted memory":  {"sh", "-c", "echo 'fork: cannot allocate memory'; exit 1"},
		"test failure":      {"sh", "-c", "echo '--- FAIL: TestRegression'; exit 1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runMergeCheckCmd(t.Context(), t.TempDir(), args[0], args[1:]...)
			if err == nil || errors.Is(err, errMergeCheckEnvironment) != (name != "test failure") {
				t.Fatalf("wrong failure classification: %v", err)
			}
		})
	}
}

func TestMergeCheckHostFailureDoesNotRequestBranchRepair(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.answer = func(args []string) (string, int) {
		if len(args) > 0 && args[0] == gateCmdName {
			doc := github.SquashDoc{Route: reviewedMergeRoute, Repo: loopMergeRepo, Number: 7, HeadSha: loopHead, Reason: mergeValidationUnavailable("host resources exhausted")}
			body, _ := json.Marshal(doc)
			return string(body), github.SquashTooling
		}
		return "", 0
	}
	r.pass(t)
	if call := r.called(agentsName, agentWakeName, loopWorker); call != nil {
		t.Fatalf("host failure blamed on branch: %v", call)
	}
}

func TestMergeResultHostFailureIsTooling(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	lintDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(lintDir, "golangci-lint"), []byte("#!/bin/sh\necho 'write: no space left on device'\nexit 1\n"), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
		t.Fatal(err)
	}
	t.Setenv("PATH", lintDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	reason, code := runReviewedMergeChecks(t.Context(), repo.dir, repo.base, mainBranch, repo.head2)
	if code != github.SquashTooling || !strings.HasPrefix(reason, reasonMergeValidationUnavailable) {
		t.Fatalf("host failure blamed on PR: %d: %s", code, reason)
	}
}

func TestMergeResultGitEnvironmentFailureIsNotAConflict(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	repo.advanceMain(t, repo.head1)
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "pre-merge-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
		t.Fatal(err)
	}
	if _, err := runGit(t.Context(), repo.dir, "config", "core.hooksPath", hooks); err != nil {
		t.Fatal(err)
	}
	r := newLoopRig(t)
	r.a.cfg.Disk.Checkouts = []string{repo.dir}
	_, reason, code := checkReviewedMerge(t.Context(), r.a, superloop.Pull{Ref: secondPR, Base: mainBranch}, repo.head2)
	if code != github.SquashTooling || !strings.HasPrefix(reason, reasonMergeValidationUnavailable) {
		t.Fatalf("checker hook failure blamed on branch: %d: %s", code, reason)
	}
}

// Literals the package's tests share.
const (
	testFixWord    = "fix"
	testRunWord    = "run"
	testAgyVersion = "1.4.7"
	testExpiresIn  = "expires_in"
)

const testMergedWord = "merged"

const testStatusWord = "status"
