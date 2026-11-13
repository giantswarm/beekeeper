package superloop

import (
	"slices"
	"testing"
	"time"
)

// claimElsewhere is another desk's claim on an issue pr serves, as the loop
// says it (Input.Elsewhere).
const claimElsewhere = `desk work (session "w-1") since 2026-11-13 04:00 UTC on o/r#3`

// The pull request of another desk's worker gets the first review of its
// green head in the pass that sees it without an owner, right after its
// worker's last edit on GitHub; the desk's own ownerless pull request waits
// OrphanWait for it. Once that review started, nothing awaits a first review.
//
// control: none: Input.Elsewhere and AwaitingFirstReview are new, so the base cannot build this test
func TestAnotherDesksGreenPullIsReviewedWithoutTheWait(t *testing.T) {
	in := sweepInput(with(pull(head1, true), func(p *Pull) { p.Updated = orphanSince }), 0)
	if got := AwaitingFirstReview(in); !slices.Equal(got, []string{pr}) {
		t.Fatalf("awaiting first review = %v, want %s", got, pr)
	}
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("the desk's own ownerless pull request took %+v before the wait", acts)
	}
	in.Elsewhere = map[string]string{pr: claimElsewhere}
	a := only(t, Plan(in), KindReview)
	if a.Key != reviewKey(pr, head1) || a.Round != 1 || a.Head != head1 || a.URL != prURL {
		t.Errorf("review = %+v", a)
	}
	in.Taken = taken(t, in.Now, a)
	if got := AwaitingFirstReview(in); len(got) != 0 {
		t.Errorf("awaiting first review after its start = %v", got)
	}
	if acts := Plan(in); len(acts) != 0 {
		t.Errorf("the pass after the review started took %+v", acts)
	}
}

// Only a first review skips the wait: another desk's pull request whose
// head is red, conflicts or still runs its checks, or whose head's review
// already started, waits for the sweep as the desk's own does.
//
// control: none: Input.Elsewhere and AwaitingFirstReview are new, so the base cannot build this test
func TestAnotherDesksPullWaitsUnlessItsGreenHeadAwaitsAFirstReview(t *testing.T) {
	for name, f := range map[string]func(*Pull){
		"red check":   func(p *Pull) { p.Green = false; p.Red = []Check{{Name: lintCheck, URL: lintURL}} },
		"conflicting": func(p *Pull) { p.Mergeable = mergeableConflicting },
		"running":     func(p *Pull) { p.Green = false },
		"draft":       func(p *Pull) { p.Draft = true },
	} {
		t.Run(name, func(t *testing.T) {
			in := sweepInput(with(pull(head1, true), f), time.Minute)
			in.Elsewhere = map[string]string{pr: claimElsewhere}
			if got := AwaitingFirstReview(in); len(got) != 0 {
				t.Errorf("awaiting first review = %v", got)
			}
			if acts := Plan(in); len(acts) != 0 {
				t.Errorf("took %+v within the wait", acts)
			}
		})
	}
	in := sweepInput(pull(head1, true), time.Minute)
	in.Elsewhere = map[string]string{pr: claimElsewhere}
	in.Taken = taken(t, orphanSince, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: PullWorkerName(pr), Round: 1})
	if got := AwaitingFirstReview(in); len(got) != 0 {
		t.Errorf("awaiting first review with its round started = %v", got)
	}
}

// A pull request serves the issues it closes and the ones its "Part of"
// lines name, each once; prose that only mentions an issue names none.
//
// control: none: PullIssues and ParsePartOf are new, so the base cannot build this test
func TestPullIssuesAreTheClosedAndThePartOfIssues(t *testing.T) {
	body := "## Change\n\nPart of https://github.com/o/r/issues/3\n" +
		"- Part of: o/x#8, #9 and https://github.com/o/r/pull/5\n" +
		"**Part of** #7\n" +
		"This is part of #10 in prose.\n"
	p := Pull{Closes: []string{"o/r#3"}, Body: body}
	if got, want := PullIssues(pr, p), []string{"o/r#3", "o/x#8", "o/r#9"}; !slices.Equal(got, want) {
		t.Errorf("issues = %v, want %v", got, want)
	}
	if got := ParsePartOf(pr, "Closes #3\n"); len(got) != 0 {
		t.Errorf("a closing line names %v", got)
	}
}
