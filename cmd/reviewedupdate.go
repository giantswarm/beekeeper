package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// mergeStateBehind is GitHub's merge state of a branch behind its base.
const mergeStateBehind = "BEHIND"

// reviewedUpdateWait is how long a reviewed merge waits for the checks of
// the head its branch update made, devctl pr merge's wait for green.
var reviewedUpdateWait = squashTimeout

// reviewedPause waits d, or until ctx ends; a seam for the tests.
var reviewedPause = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// updateReviewedBranch brings p, approved at head and behind its base, up to
// date at merge time, as the gate's --update-branch does: GitHub merges the
// base into the branch, pinned to the head it is at, so that nobody but the
// merge moves an approved head. It then waits for the new head's checks,
// every check the approved head had and all of them green, and confirms that
// the approval covers the new head (readBaseMerge). A base that moved on
// meanwhile leaves that head behind again: it updates once more, pinned to
// it, within the same wait. It returns the pull request at an up-to-date
// head, or a squash exit code and the reason it is not to be merged now.
func updateReviewedBranch(ctx context.Context, p superloop.Pull, head string) (superloop.Pull, string, int) {
	deadline := time.Now().Add(reviewedUpdateWait)
	for from := head; ; {
		q, reason, code := updateOnce(ctx, p, head, from, deadline)
		if code != github.SquashMerged {
			return q, reason, code
		}
		behind := q.MergeState == mergeStateBehind
		if !behind {
			n, err := readReviewedBehind(ctx, q)
			if err != nil {
				return q, "base comparison unavailable: " + err.Error(), github.SquashTooling
			}
			behind = n > 0
		}
		if !behind {
			return q, "", github.SquashMerged
		}
		if !time.Now().Before(deadline) {
			return q, fmt.Sprintf("the base moved on while the checks of the updated head %s ran, past the wait of %s", shortSHA(q.Head), reviewedUpdateWait), github.SquashTimeout
		}
		from = q.Head
	}
}

// updateOnce merges the base into p's branch at from, then waits until
// deadline for the new head's checks and the approval of head to cover it.
func updateOnce(ctx context.Context, p superloop.Pull, head, from string, deadline time.Time) (superloop.Pull, string, int) {
	r, ok := parseRef(p.Ref)
	if !ok {
		return p, "invalid pull request", github.SquashTooling
	}
	argv := []string{"gh", "api", "-X", "PUT", fmt.Sprintf("repos/%s/pulls/%d/update-branch", r.Repo, r.N), "-f", "expected_head_sha=" + from}
	out, stderr, code, err := loopCommand(ctx, "", argv)
	switch {
	case err != nil:
		return p, "updating the branch with its base: " + err.Error(), github.SquashTooling
	case code != 0:
		return p, "updating the branch with its base: " + oneLine(out+stderr), github.SquashNotApplicable
	}
	for {
		q, err := readLoopPull(ctx, p.Ref)
		switch {
		case err != nil:
			return p, err.Error(), github.SquashTooling
		case q.State != superloop.Open:
			return q, fmt.Sprintf("the pull request is %s while its branch was updated", strings.ToLower(q.State)), github.SquashNotApplicable
		case q.Head != from && len(q.Red) > 0:
			names := make([]string, 0, len(q.Red))
			for _, c := range q.Red {
				names = append(names, c.Name)
			}
			return q, fmt.Sprintf("checks of the updated head %s are red: %s", shortSHA(q.Head), strings.Join(names, ", ")), github.SquashRed
		case q.Head != from && q.Green && hasChecks(q.Checks, p.Checks):
			covered, err := readBaseMerge(ctx, q, head)
			if err != nil {
				return q, err.Error(), github.SquashTooling
			}
			if !covered {
				return q, fmt.Sprintf("the updated head %s changes more than the base merged into the reviewed head %s", shortSHA(q.Head), shortSHA(head)), github.SquashNotApplicable
			}
			return q, "", github.SquashMerged
		}
		if !time.Now().Before(deadline) {
			return q, fmt.Sprintf("the checks of the branch updated from head %s did not all pass within %s", shortSHA(from), reviewedUpdateWait), github.SquashTimeout
		}
		if err := reviewedPause(ctx, squashPoll); err != nil {
			return q, err.Error(), github.SquashTooling
		}
	}
}

// hasChecks reports whether have names every check of want.
func hasChecks(have, want []string) bool {
	for _, c := range want {
		if !slices.Contains(have, c) {
			return false
		}
	}
	return true
}
