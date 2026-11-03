package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

// An approved, green branch that is only behind its base merges at the
// reviewed head as it stands: no branch update, no wait for another CI run.
// The merge queue tests it against the current base; admin configuration
// must never bypass that server-side protection.
func TestReviewedMergeMergesABehindBranchAsItStands(t *testing.T) {
	for name, tc := range map[string]struct {
		method string
		admin  bool
		// strict is a base that requires up-to-date branches: GitHub
		// reports the merge state BEHIND; else only the comparison with
		// the base shows the commits the head lacks.
		strict bool
	}{
		"admin configuration still uses queue": {admin: true, strict: true},
		"no up-to-date requirement":            {},
		"no up-to-date requirement, admin":     {admin: true},
		"rebase, admin":                        {method: mergeMethodRebase, admin: true, strict: true},
		"rebase, no up-to-date requirement":    {method: mergeMethodRebase},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBehindRig(t)
			if tc.method != "" {
				b.a.cfg.Merge.Methods = map[string]string{loopMergeRepo: tc.method}
			}
			if tc.admin {
				b.a.cfg.Merge.Admin = []string{loopMergeRepo}
			}
			if !tc.strict {
				b.pull.MergeState = mergeStateClean
			}
			was := readReviewedBehind
			t.Cleanup(func() { readReviewedBehind = was })
			readReviewedBehind = func(context.Context, superloop.Pull) (int, error) { return 3, nil }
			b.claim(t)
			doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
			if doc.ExitCode != 0 || doc.HeadSha != loopHead || doc.MergeCommitSha != loopFixed {
				t.Fatalf("behind merge: %+v, commands %v", doc, b.cmds)
			}
			if len(b.updates) != 0 || b.pauses != 0 {
				t.Fatalf("the merge updated the branch (%v) or waited for checks (%d pauses)", b.updates, b.pauses)
			}
			m := b.merges()
			if len(m) != 1 || !slices.Contains(m[0], "--match-head-commit") || !slices.Contains(m[0], loopHead) || b.lastMerged != loopHead {
				t.Fatalf("merge of the reviewed head: %v", b.cmds)
			}
			if slices.Contains(m[0], "--admin") {
				t.Fatalf("queue bypassed: %v", m[0])
			}
			if tc.method != "" && !slices.Contains(m[0], "--"+tc.method) {
				t.Fatalf("method %s lost: %v", tc.method, m[0])
			}
		})
	}
}

// The loop's go on an approved head that fell behind its base ends merged
// at that very head in the same pass: no branch update, no repair wake, no
// new review and no second CI run.
func TestLoopMergesAnApprovedBehindHeadAsItStands(t *testing.T) {
	b := newBehindRig(t)
	r := b.loopRig
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.pull.MergeState = ""
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pull.MergeState = mergeStateBehind
	r.pass(t)
	if r.pull.State != superloop.Merged || r.lastMerged != loopHead {
		t.Fatalf("not merged at the reviewed head: %+v; commands %v; calls %v", r.pull, r.cmds, r.calls)
	}
	if len(b.updates) != 0 || b.pauses != 0 {
		t.Fatalf("the merge updated the branch (%v) or waited for checks (%d pauses)", b.updates, b.pauses)
	}
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("the merge woke the worker: %v", c)
	}
	if got := r.logged(t, "supervisor.merge"); len(got) != 1 || !strings.Contains(got[0].Detail, loopHead) {
		t.Fatalf("merge records %v", got)
	}
}
