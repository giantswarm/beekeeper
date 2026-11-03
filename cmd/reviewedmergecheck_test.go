package cmd

import (
	"context"
	"encoding/json"
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
	secondPR     = "o/r#8"
	secondURL    = "https://github.com/o/r/pull/8"
	secondWorker = "worker two"
)

// mergeCheckRepo is a real git repository in a scratch directory backing
// loopMergeRepo ("o/r") with a Go module and two PR branches from main.
type mergeCheckRepo struct {
	dir   string
	base  string
	head1 string
	head2 string
}

func newMergeCheckRepo(t *testing.T, pr1Files, pr2Files map[string]string, baseFiles map[string]string) *mergeCheckRepo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "r")
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
	git("init", "-q", "-b", mainBranch)
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	git("remote", loopAdd, "origin", "https://github.com/"+loopMergeRepo+".git")

	write("go.mod", "module example.com/r\n\ngo 1.24\n")
	for k, v := range baseFiles {
		write(k, v)
	}
	git(loopAdd, ".")
	git("commit", "-q", "-m", "initial")
	base := git("rev-parse", "HEAD")

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
	return &mergeCheckRepo{dir: dir, base: base, head1: head1, head2: head2}
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
				"guard/base.go": "package guard\n\nfunc Base() string { return \"ok\" }\n",
			},
			pr1Files: map[string]string{
				"guard/pr1.go": "package guard\n\nfunc One(x string) bool {\n\tif x == \"agy-harness\" {\n\t\treturn true\n\t}\n\treturn x == \"agy-harness\" && Base() != \"\"\n}\n",
			},
			pr2Files: map[string]string{
				"guard/pr2.go": "package guard\n\nfunc Two(y string) bool {\n\tif y == \"agy-harness\" {\n\t\treturn true\n\t}\n\treturn y == \"agy-harness\" && Base() != \"\"\n}\n",
			},
			wantCheck: "goconst",
		},
		"go test failure across two PRs": {
			baseFiles: map[string]string{
				"guard/cost.go":      "package guard\n\nvar costA, costB int\n\nfunc TotalCost() int { return costA + costB }\n",
				"guard/cost_test.go": "package guard\n\nimport \"testing\"\n\nfunc TestBoardCostStayWithinBudget(t *testing.T) {\n\tif got := TotalCost(); got > 40 {\n\t\tt.Fatalf(\"cost %d exceeds budget 40\", got)\n\t}\n}\n",
			},
			pr1Files: map[string]string{
				"guard/pr1.go": "package guard\n\nfunc init() { costA = 25 }\n",
			},
			pr2Files: map[string]string{
				"guard/pr2.go": "package guard\n\nfunc init() { costB = 16 }\n",
			},
			wantCheck: "TestBoardCostStayWithinBudget",
		},
		"compatible PRs both merge": {
			baseFiles: map[string]string{
				"guard/cost.go":      "package guard\n\nvar costA, costB int\n\nfunc TotalCost() int { return costA + costB }\n",
				"guard/cost_test.go": "package guard\n\nimport \"testing\"\n\nfunc TestBoardCostStayWithinBudget(t *testing.T) {\n\tif got := TotalCost(); got > 40 {\n\t\tt.Fatalf(\"cost %d exceeds budget 40\", got)\n\t}\n}\n",
			},
			pr1Files: map[string]string{
				"guard/pr1.go": "package guard\n\nfunc init() { costA = 20 }\n",
			},
			pr2Files: map[string]string{
				"guard/pr2.go": "package guard\n\nfunc init() { costB = 15 }\n",
			},
			wantCheck: "",
		},
		"conflicting PRs after first merge": {
			baseFiles: map[string]string{
				"guard/base.go": "package guard\n\nfunc Name() string { return \"initial\" }\n",
			},
			pr1Files: map[string]string{
				"guard/base.go": "package guard\n\nfunc Name() string { return \"from-pr-1\" }\n",
			},
			pr2Files: map[string]string{
				"guard/base.go": "package guard\n\nfunc Name() string { return \"from-pr-2\" }\n",
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
			beekeeperRun = func(ctx context.Context, args ...string) (string, int, error) {
				clean := args
				if len(clean) >= 2 && clean[0] == "--as" {
					clean = clean[2:]
				}
				if len(clean) > 0 && clean[0] == gateCmdName && slices.Contains(clean, "8") {
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
			p2.Reviews = append(p2.Reviews, superloop.Review{ID: "R2", Head: repo.head2, Verdict: superloop.Approved, At: time.Now()})
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
