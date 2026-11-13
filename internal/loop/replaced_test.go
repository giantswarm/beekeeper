package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	// replacedJudged is the head a superseded round's reviewer judged.
	replacedJudged = "1111111111111111111111111111111111111111"
	// replacedPushed is the head its author pushed past it.
	replacedPushed = "2222222222222222222222222222222222222222"
)

// A superseded reviewer's late request for changes on the head it was
// given, which its author had replaced when it came, holds no go the next
// round's approval earned: the merge reads it as the planner does, as no
// verdict. Posted before the push, on a head that only merged the base into
// the judged one, or with the base merge unread, it holds the go.
// https://github.com/giantswarm/beekeeper/issues/3843
func TestAReviewOfAReplacedHeadHoldsNoMerge(t *testing.T) {
	read := ReadBaseMerge
	t.Cleanup(func() { ReadBaseMerge = read })
	covers, readErr := false, error(nil)
	ReadBaseMerge = func(context.Context, superloop.Pull, string) (bool, error) { return covers, readErr }
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	approval := superloop.Review{ID: "R2", Head: replacedPushed, Commit: replacedPushed, Verdict: superloop.Approved, At: at}
	stale := superloop.Review{ID: "R1", Head: replacedJudged, Commit: replacedPushed, Verdict: superloop.ChangesRequested, At: at.Add(time.Minute)}
	p := superloop.Pull{Ref: "o/r#7", Head: replacedPushed, Reviews: []superloop.Review{approval, stale}}
	act := superloop.Action{Ref: p.Ref, Head: p.Head, ReviewID: approval.ID, ReviewVerdict: approval.Verdict}
	holds := func() bool {
		m := MarkReplaced(t.Context(), p)
		return LatestReviewRequestsChanges(m) || reviewOvertookDecision(m, act)
	}
	if holds() {
		t.Error("a review of a replaced head held the go")
	}
	if p.Reviews[1].Replaced {
		t.Error("MarkReplaced marked the caller's reviews")
	}
	covers = true
	if !holds() {
		t.Error("a review attached to a base merge of its head did not hold the go")
	}
	covers, readErr = false, errors.New("gh: 502")
	if !holds() {
		t.Error("a review whose base merge is unread did not hold the go")
	}
	readErr = nil
	p.Reviews[1].Commit = replacedJudged
	if !holds() {
		t.Error("a review posted before the push did not hold the go")
	}
}

// A replay answers each base-merge read with the fact recorded for its pull
// request head: superloop.Replaced reads one judged head against the head a
// review came on and against the current one.
func TestAReplayReadsABaseMergeAgainstItsHead(t *testing.T) {
	var rec LoopRecording
	in := rec.Capture(superloop.Input{BaseMerged: func(p superloop.Pull, _ string) bool { return p.Head == replacedPushed }})
	then := superloop.Pull{Ref: "o/r#7", Head: replacedPushed}
	now := then
	now.Head = "3333333333333333333333333333333333333333"
	if !in.BaseMerged(then, replacedJudged) || in.BaseMerged(now, replacedJudged) {
		t.Fatal("the capture changed the answers")
	}
	replay := rec.ReplayInput()
	if !replay.BaseMerged(then, replacedJudged) || replay.BaseMerged(now, replacedJudged) {
		t.Errorf("replayed facts %+v answer one head for another", rec.Facts)
	}
}
