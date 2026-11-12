package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// deskPullJSON is gh pr view's answer to the loop's read (loop.ReadLoopPull) of
// an open, approved, green pull request, as GitHub gave it on 2026-11-03 for
// the pull requests the merge-result check refused, with o/r#8 for the
// repository and head.
func deskPullJSON(head string) string {
	return `{"author":{"id":"MDQ6VXNlcjMyNDg4","is_bot":false,"login":"` + loopDesk + `"},` +
		`"baseRefName":"main","changedFiles":1,"closingIssuesReferences":[],` +
		`"files":[{"path":"guard/pr2.go","additions":2,"deletions":0}],` +
		`"headRefName":"beekeeper-execution/3dbc3f90-ac11-4a25-83e7-a1126de64dd6/1",` +
		`"headRefOid":"` + head + `","isDraft":false,"mergeCommit":null,` +
		`"mergeStateStatus":"BLOCKED","mergeable":"MERGEABLE","reviews":[],"state":"OPEN",` +
		`"statusCheckRollup":[{"__typename":"CheckRun","completedAt":"2026-11-03T16:34:37Z","conclusion":"SUCCESS",` +
		`"detailsUrl":"https://github.com/o/r/actions/runs/41675539468/job/126055029092","name":"chart-tests / helm-test",` +
		`"startedAt":"2026-11-03T16:34:16Z","status":"COMPLETED","workflowName":"chart-tests"}],` +
		`"title":"fix: a change","url":"` + secondURL + `"}`
}

// useDeskRoots gives r the desk's configuration of 2026-11-03: no
// disk.checkouts, and agents.dir and agents.roots written with ~/, the
// checkouts living in ~/projects/<name>. It returns the home folder.
func useDeskRoots(t *testing.T, r *loopRig) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	r.a.cfg.Disk.Checkouts = nil
	r.a.cfg.Agents.Dir = "~/projects"
	r.a.cfg.Agents.Roots = []string{"~/projects", "~/.local/state/beekeeper/worktrees"}
	if err := os.MkdirAll(filepath.Join(home, "projects"), 0o750); err != nil {
		t.Fatal(err)
	}
	return home
}

// https://github.com/giantswarm/beekeeper/issues/2230: with the loop's real
// pull request values and the desk's real roots, the check finds the
// checkout in ~/projects/<name> and validates the merge result; it refused
// every merge with "merge-result validation unavailable" as it searched the
// unexpanded ~/projects.
func TestMergeResultCheckFindsTheCheckoutUnderTheDesksHomeRoots(t *testing.T) {
	repo := newMergeCheckRepo(t, map[string]string{testGuardPR1File: testGuardSourceA}, map[string]string{testGuardPR2File: testGuardSourceB}, nil)
	r := newLoopRig(t)
	home := useDeskRoots(t, r)
	checkout := filepath.Join(home, "projects", "r")
	if err := os.Rename(repo.dir, checkout); err != nil {
		t.Fatal(err)
	}
	repo.dir = checkout
	p, err := loop.ParseLoopPull(secondPR, []byte(deskPullJSON(repo.head2)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Base != mainBranch || p.Head != repo.head2 || !p.Green {
		t.Fatalf("the loop's pull: %+v", p)
	}
	if dir, searched := r.a.findReviewedRepoDir(t.Context(), loopMergeRepo); dir != checkout {
		t.Fatalf("checkout %q not found, searched %v", checkout, searched)
	}
	// The base is an ancestor of the head: the PR's own checks stand.
	if checked, reason, code := checkReviewedMerge(t.Context(), r.a, p, p.Head); checked || code != 0 || reason != "" {
		t.Fatalf("up-to-date head: checked %v, exit %d, %s", checked, code, reason)
	}
	// The base moved on: the merge result is built and checked.
	repo.advanceMain(t, repo.head1)
	if checked, reason, code := checkReviewedMerge(t.Context(), r.a, p, p.Head); !checked || code != 0 {
		t.Fatalf("behind head: checked %v, exit %d, %s", checked, code, reason)
	}
}

// A repository with no checkout on the desk (a repository without Go code
// alike) is not validated locally: its merge is not refused, the reason says
// the check was skipped and where it looked, and the PR's own checks stand.
func TestMergeResultCheckWithoutACheckoutSaysSoAndDoesNotBlock(t *testing.T) {
	r := newLoopRig(t)
	home := useDeskRoots(t, r)
	p, err := loop.ParseLoopPull(secondPR, []byte(deskPullJSON("67abfd7cdf165a0c4ff9988278c63cba0c1c057f")))
	if err != nil {
		t.Fatal(err)
	}
	checked, reason, code := checkReviewedMerge(t.Context(), r.a, p, p.Head)
	if checked || code != 0 {
		t.Fatalf("missing checkout blocked the merge: exit %d, %s", code, reason)
	}
	for _, want := range []string{"merge-result validation skipped", "no checkout of " + loopMergeRepo, filepath.Join(home, "projects", "r")} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
}

// Every refusal names the condition that failed.
func TestMergeResultValidationUnavailableNamesTheCondition(t *testing.T) {
	r := newLoopRig(t)
	for _, tc := range []struct {
		p    superloop.Pull
		head string
		want string
	}{
		{superloop.Pull{Ref: secondPR}, loopHead, "has no base branch"},
		{superloop.Pull{Ref: secondPR, Base: mainBranch}, "", "has no reviewed head"},
		{superloop.Pull{Ref: "not a ref", Base: mainBranch}, loopHead, `"not a ref" does not parse`},
	} {
		_, reason, code := checkReviewedMerge(t.Context(), r.a, tc.p, tc.head)
		if code != github.SquashTooling || !strings.HasPrefix(reason, reasonMergeValidationUnavailable+": ") || !strings.Contains(reason, tc.want) {
			t.Fatalf("%+v: exit %d, %q; want %q", tc.p, code, reason, tc.want)
		}
	}
}

// A base branch with a merge queue is merged through the queue, never with
// --admin; one without a queue, GitHub's answer for both repositories of
// https://github.com/giantswarm/beekeeper/issues/2230, merges after the
// lane's own check, with --admin where the desk merges with it. A skipped
// check's note reaches the merge's document.
func TestReviewedMergeUsesTheQueueOnlyWhereTheBaseHasOne(t *testing.T) {
	const skipped = "merge-result validation skipped: no checkout of o/r in /x; the pull request's own checks stand"
	for name, tc := range map[string]struct {
		queue, admin, wantAdmin bool
	}{
		"queue, admin":       {queue: true, admin: true},
		"queue":              {queue: true},
		"no queue, admin":    {admin: true, wantAdmin: true},
		"no queue, no admin": {},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBehindRig(t)
			b.pull.MergeState = emergencyBlocked
			b.pull.ReviewDecision = reviewRequiredDecision
			b.a.cfg.Merge.DevctlOwners = []string{"o"}
			if tc.admin {
				b.a.cfg.Merge.Admin = []string{loopMergeRepo}
			}
			readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) { return tc.queue, nil }
			reviewedMergeCheck = func(context.Context, *app, superloop.Pull, string) (bool, string, int) { return false, skipped, 0 }
			b.claim(t)
			doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
			if doc.ExitCode != 0 || doc.MergeCommitSha != loopFixed {
				t.Fatalf("merge: %+v, commands %v", doc, b.cmds)
			}
			if !strings.Contains(doc.Reason, skipped) {
				t.Fatalf("the skipped check is not said: %q", doc.Reason)
			}
			m := b.cmds
			if len(m) != 1 || slices.Contains(m[0], "--admin") != tc.wantAdmin {
				t.Fatalf("merge command %v, want --admin %v", m, tc.wantAdmin)
			}
			if (m[0][0] == devctlBinary) == tc.wantAdmin || !slices.Contains(m[0], loopHead) {
				t.Fatalf("wrong identity or missing exact head: %v", m[0])
			}
			events, err := b.a.store.Events(0, func(e state.Event) bool { return e.Verb == "merge.admin" })
			if err != nil || (len(events) == 1) != tc.wantAdmin {
				t.Fatalf("admin audit: %v, %v", events, err)
			}
			if tc.wantAdmin && !strings.Contains(events[0].Detail, loopHead) {
				t.Fatalf("admin audit omits approved head: %v", events)
			}
		})
	}
}

func TestReviewedAdminMergeRequiresAuditBeforeMutation(t *testing.T) {
	b := newBehindRig(t)
	b.a.cfg.Merge.Admin = []string{loopMergeRepo}
	readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) { return false, nil }
	b.claim(t)
	b.failRecord = func(e state.Event) bool { return e.Verb == "merge.admin" }
	doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != github.SquashTooling || len(b.cmds) != 0 {
		t.Fatalf("unaudited admin mutation: %+v, %v", doc, b.cmds)
	}
}

// An unreadable merge-queue answer still refuses the merge.
func TestReviewedMergeRefusesAnUnreadableQueue(t *testing.T) {
	b := newBehindRig(t)
	b.pull.MergeState = mergeStateClean
	readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) {
		return false, fmt.Errorf("merge queue unavailable")
	}
	b.claim(t)
	doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != github.SquashTooling || len(b.merges()) != 0 {
		t.Fatalf("merge despite an unreadable queue: %+v, commands %v", doc, b.cmds)
	}
}

// A merge-result check that overruns its deadline ends with everything it
// started, grandchildren too, and refuses as tooling, never as red: no lint
// or test run outlives the merge that started it.
func TestMergeCheckTimeoutEndsTheWholeProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process groups are checked on linux")
	}
	was := mergeCheckTimeout
	t.Cleanup(func() { mergeCheckTimeout = was })
	mergeCheckTimeout = 300 * time.Millisecond
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	_, err := runMergeCheckCmd(t.Context(), dir, "sh", "-c", "sleep 60 & echo $! > "+pidFile+"; wait")
	if !errors.Is(err, errMergeCheckTimeout) {
		t.Fatalf("timeout not reported: %v", err)
	}
	raw, err := os.ReadFile(pidFile) //nolint:gosec // test-owned file
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for proc.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if proc.Alive(pid) {
		t.Fatalf("grandchild %d outlived the check", pid)
	}
}
