package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// mergeStateClean is GitHub's merge state of an up-to-date, mergeable branch.
const mergeStateClean = "CLEAN"

// The checks of the rig's reviewed head.
const behindBuild, behindUnit = "build", "unit"

// behindRig is a loop rig whose pull request, approved at loopHead, is
// behind its base: GitHub's update-branch moves it to loopFixed with its
// checks pending, and every pause of the wait for them runs onPause.
type behindRig struct {
	*loopRig
	updates []string
	pauses  int
	onPause func(n int)
}

func newBehindRig(t *testing.T) *behindRig {
	t.Helper()
	b := &behindRig{loopRig: newLoopRig(t)}
	r := b.loopRig
	r.pull.Base = mainBranch
	r.pull.MergeState = mergeStateBehind
	r.pull.Checks = []string{behindBuild, behindUnit}
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
		return approved == loopHead && p.Head == loopFixed, nil
	}
	merge := r.command
	r.command = func(argv []string) (string, int) {
		if slices.Contains(argv, "repos/"+loopMergeRepo+"/pulls/7/update-branch") {
			b.updates = append(b.updates, argv[len(argv)-1])
			r.pull.Head, r.pull.MergeState, r.pull.Green, r.pull.Checks = loopFixed, mergeStateClean, false, nil
			return "", 0
		}
		return merge(argv)
	}
	b.onPause = func(int) { r.pull.Green, r.pull.Checks = true, []string{behindBuild, behindUnit} }
	pause, wait := reviewedPause, reviewedUpdateWait
	t.Cleanup(func() { reviewedPause, reviewedUpdateWait = pause, wait })
	reviewedUpdateWait = time.Hour
	reviewedPause = func(context.Context, time.Duration) error {
		b.pauses++
		b.onPause(b.pauses)
		return nil
	}
	return b
}

// claim records the loop's exact-head merge claim of loopHead.
func (b *behindRig) claim(t *testing.T) {
	t.Helper()
	if err := b.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
}

// merges are the gh pr merge commands the rig ran.
func (b *behindRig) merges() [][]string {
	var out [][]string
	for _, argv := range b.cmds {
		if len(argv) > 2 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == superloop.KindMerge {
			out = append(out, argv)
		}
	}
	return out
}

// An approved branch behind a base that requires up-to-date branches, which
// the desk merges without --admin, is brought up to date by the merge
// itself, pinned to the reviewed head, and the updated head merges once
// every check the reviewed head had is green on it and the approval covers
// it: nobody but the merge moves an approved head.
func TestReviewedMergeUpdatesABehindBranchAndMergesTheCoveredHead(t *testing.T) {
	b := newBehindRig(t)
	b.claim(t)
	// The first read after the update shows only part of the checks.
	b.onPause = func(n int) {
		b.pull.Green, b.pull.Checks = true, []string{behindBuild}
		if n > 1 {
			b.pull.Checks = []string{behindBuild, behindUnit}
		}
	}
	doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != 0 || doc.HeadSha != loopHead || doc.MergeCommitSha != loopFixed {
		t.Fatalf("behind merge: %+v, commands %v", doc, b.cmds)
	}
	if !slices.Equal(b.updates, []string{"expected_head_sha=" + loopHead}) {
		t.Fatalf("update not pinned to the reviewed head: %v", b.updates)
	}
	if b.pauses != 2 {
		t.Fatalf("merged after %d pauses, want 2: the merge waits for every check", b.pauses)
	}
	m := b.merges()
	if len(m) != 1 || !slices.Contains(m[0], "--match-head-commit") || !slices.Contains(m[0], loopFixed) {
		t.Fatalf("merge of the updated head: %v", b.cmds)
	}
	if !slices.Equal(b.baseReads, []string{loopHead + ".." + loopFixed}) {
		t.Fatalf("approval coverage read %v", b.baseReads)
	}
}

// An update that cannot be merged now goes back as the child's structured
// result, and nothing merges.
func TestReviewedMergeOfAnUpdatedBranchStopsShort(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		want string
		set  func(*behindRig)
	}{
		reviewedRedCase: {github.SquashRed, "red: " + behindBuild, func(b *behindRig) {
			b.onPause = func(int) { b.pull.Red = []superloop.Check{{Name: behindBuild}} }
		}},
		"uncovered": {github.SquashNotApplicable, "changes more than the base", func(b *behindRig) {
			b.baseMerge = func(superloop.Pull, string) (bool, error) { return false, nil }
		}},
		"checks too slow": {github.SquashTimeout, "did not all pass", func(b *behindRig) {
			reviewedUpdateWait = 0
			b.onPause = func(int) {}
		}},
		"update refused": {github.SquashNotApplicable, "merge conflict", func(b *behindRig) {
			b.command = func([]string) (string, int) { return "merge conflict", 1 }
		}},
		"stopped meanwhile": {github.SquashRefused, reasonDeskStopped, func(b *behindRig) {
			b.onPause = func(int) {
				b.pull.Green, b.pull.Checks = true, []string{behindBuild, behindUnit}
				if err := b.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Stop = &state.Stop{}; return nil, nil }); err != nil {
					t.Fatal(err)
				}
			}
		}},
		"changes requested on source head meanwhile": {github.SquashRefused, "requested changes", func(b *behindRig) {
			b.onPause = func(int) {
				b.pull.Green, b.pull.Checks = true, []string{behindBuild, behindUnit}
				b.verdict("R2", loopHead, superloop.ChangesRequested, "Review: changes requested\n\nThe source head still needs its guard fixed")
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBehindRig(t)
			b.claim(t)
			tc.set(b)
			doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
			if doc.ExitCode != tc.code || !strings.Contains(doc.Reason, tc.want) || len(b.merges()) != 0 {
				t.Fatalf("doc %+v, commands %v", doc, b.cmds)
			}
		})
	}
}

// The update is a mutation: an unclaimed merge never makes it.
func TestReviewedMergeUpdatesNoBranchWithoutItsClaim(t *testing.T) {
	b := newBehindRig(t)
	doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode == 0 || len(b.cmds) != 0 {
		t.Fatalf("unclaimed update: %+v %v", doc, b.cmds)
	}
}

// The loop's go on an approved head behind a base that requires up-to-date
// branches, which the desk merges without --admin, ends merged at the
// updated head, without a repair wake or a new review: the merged head only
// merges the base into the reviewed one.
func TestLoopMergesAnApprovedBehindHeadAfterUpdatingIt(t *testing.T) {
	b := newBehindRig(t)
	r := b.loopRig
	r.pull.MergeState = ""
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pull.MergeState = mergeStateBehind
	r.pass(t)
	if r.pull.State != superloop.Merged || r.lastMerged != loopFixed {
		t.Fatalf("not merged at the updated head: %+v; commands %v; calls %v", r.pull, r.cmds, r.calls)
	}
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("the merge woke the worker: %v", c)
	}
	if got := r.logged(t, "supervisor.merge"); len(got) != 1 || !strings.Contains(got[0].Detail, testMergedWord) {
		t.Fatalf("merge records %v", got)
	}
	r.pass(t)
	if n := len(b.merges()); n != 1 {
		t.Fatalf("%d merges, want 1", n)
	}
}

// A base that moves on while the updated head's checks run leaves that head
// behind: the merge updates again, pinned to it, and never merges a head
// that lacks the current base where the base requires up-to-date branches
// and the desk merges without --admin.
func TestReviewedMergeUpdatesAgainWhenTheBaseMovesDuringTheWait(t *testing.T) {
	const moved = "3333333333333333333333333333333333333333"
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("past the wait ", late), func(t *testing.T) {
			b := newBehindRig(t)
			if late {
				reviewedUpdateWait = time.Millisecond
			}
			b.claim(t)
			b.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
				return approved == loopHead && (p.Head == loopFixed || p.Head == moved), nil
			}
			merge := b.command
			b.command = func(argv []string) (string, int) {
				if !slices.Contains(argv, "repos/"+loopMergeRepo+"/pulls/7/update-branch") {
					return merge(argv)
				}
				b.updates = append(b.updates, argv[len(argv)-1])
				next := loopFixed
				if b.pull.Head == loopFixed {
					next = moved
				}
				b.pull.Head, b.pull.MergeState, b.pull.Green, b.pull.Checks = next, mergeStateClean, false, nil
				return "", 0
			}
			// The checks of the first update pass while the base moves on.
			b.onPause = func(int) {
				b.pull.Green, b.pull.Checks = true, []string{behindBuild, behindUnit}
				if b.pull.Head == loopFixed {
					b.pull.MergeState = mergeStateBehind
					if late {
						time.Sleep(2 * reviewedUpdateWait)
					}
				}
			}
			doc := b.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
			m := b.merges()
			if late {
				if doc.ExitCode != github.SquashTimeout || len(m) != 0 || len(b.updates) != 1 {
					t.Fatalf("a stale head past the wait: %+v, commands %v", doc, b.cmds)
				}
				return
			}
			if doc.ExitCode != 0 || !slices.Equal(b.updates, []string{"expected_head_sha=" + loopHead, "expected_head_sha=" + loopFixed}) {
				t.Fatalf("second update: %+v, updates %v, commands %v", doc, b.updates, b.cmds)
			}
			if len(m) != 1 || !slices.Contains(m[0], moved) || slices.Contains(m[0], loopFixed) || slices.Contains(m[0], "--admin") {
				t.Fatalf("merged a stale head: %v", m)
			}
		})
	}
}
