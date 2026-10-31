package superloop

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	pr      = "o/r#7"
	prURL   = "https://github.com/o/r/pull/7"
	head1   = "1111111111111111111111111111111111111111"
	head2   = "2222222222222222222222222222222222222222"
	head3   = "3333333333333333333333333333333333333333"
	worker  = "worker one"
	release = "v1.2.0"
)

var (
	t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	// t1 is when a round's reviewer posts its verdict.
	t1 = t0.Add(time.Minute)
)

func parked(at time.Time) []Worker {
	return []Worker{{Name: worker, ParkedOn: pr, ParkedAt: at}}
}

func pull(head string, green bool, reviews ...Review) map[string]Pull {
	return map[string]Pull{pr: {Ref: pr, URL: prURL, State: Open, Head: head, Green: green, Reviews: reviews}}
}

// taken is what the loop's log says of acts, as ParseTaken reads it back.
func taken(t *testing.T, at time.Time, acts ...Action) []Taken {
	t.Helper()
	var out []Taken
	for _, a := range acts {
		tk, ok := ParseTaken(at, "supervisor."+a.Kind, a.Ref, Detail(a, "done"))
		if !ok {
			t.Fatalf("the log line of %+v does not read back", a)
		}
		out = append(out, tk)
	}
	return out
}

func only(t *testing.T, acts []Action, kind string) Action {
	t.Helper()
	if len(acts) != 1 || acts[0].Kind != kind {
		t.Fatalf("want one %s, got %+v", kind, acts)
	}
	return acts[0]
}

func TestAGreenParkedHeadStartsAReview(t *testing.T) {
	a := only(t, Plan(Input{Workers: parked(t0), Pulls: pull(head1, true), Rounds: 1}), KindReview)
	if a.Agent != worker || a.Head != head1 || a.Round != 1 || a.URL != prURL {
		t.Errorf("review = %+v", a)
	}
	if acts := Plan(Input{Workers: parked(t0), Pulls: pull(head1, false), Rounds: 1}); len(acts) != 0 {
		t.Errorf("a head whose checks are not green started %+v", acts)
	}
	if acts := Plan(Input{Pulls: pull(head1, true), Rounds: 1}); len(acts) != 0 {
		t.Errorf("a pull request nobody parked on started %+v", acts)
	}
	// Started once: a restarted loop reads the key back.
	if acts := Plan(Input{Workers: parked(t0), Pulls: pull(head1, true), Rounds: 1, Taken: taken(t, t0, a)}); len(acts) != 0 {
		t.Errorf("a second pass took %+v again", acts)
	}
}

func TestASelfHostedWorkersReviewAsksForAFrontierReviewer(t *testing.T) {
	w := parked(t0)
	w[0].SelfHosted = true
	if a := only(t, Plan(Input{Workers: w, Pulls: pull(head1, true), Rounds: 1}), KindReview); !a.SelfHosted {
		t.Errorf("review = %+v, want SelfHosted", a)
	}
}

func TestAnApprovedHeadGetsTheMergeGo(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: Approved, At: t0.Add(time.Minute)}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	a := only(t, Plan(Input{Workers: parked(t0), Pulls: pull(head1, true, rev), Rounds: 1, Taken: taken(t, t0, started)}), KindGo)
	if a.Agent != worker || a.Head != head1 || !strings.Contains(a.Message, "--match-head-commit "+head1) {
		t.Errorf("go = %+v", a)
	}
	// The worker woke and is no longer parked: the go still finds it.
	if a := only(t, Plan(Input{Pulls: pull(head1, true, rev), Rounds: 1, Taken: taken(t, t0, started)}), KindGo); a.Agent != worker {
		t.Errorf("go = %+v", a)
	}
}

// Every merge go needs every check on its head green.
func TestAnApprovalGivesNoGoWithoutGreenChecks(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	// pull's green false stands for a failed, a pending and an absent
	// check alike: parseLoopPull reads each as not green.
	if acts := Plan(Input{Workers: parked(t0), Pulls: pull(head1, false, rev), Rounds: 1, Taken: taken(t, t0, started)}); len(acts) != 0 {
		t.Errorf("an approved head with red checks got %+v", acts)
	}
	// Nor the fixes of the last round.
	changes := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	acts := []Action{started, {Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker}}
	if got := Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, false, changes), Rounds: 1, Taken: taken(t, t0, acts...)}); len(got) != 0 {
		t.Errorf("a fixed head with red checks got %+v", got)
	}
}

// Only the verdict of the loop's last round, on its head and posted after
// it started, counts.
func TestAnOldVerdictAuthorizesNothing(t *testing.T) {
	// A verdict of nobody's round: the loop reviews before any go.
	stray := Review{ID: "R0", Head: head1, Verdict: Approved, At: t0.Add(-time.Hour)}
	if a := only(t, Plan(Input{Workers: parked(t0), Pulls: pull(head1, true, stray), Rounds: 1}), KindReview); a.Round != 1 {
		t.Errorf("review = %+v", a)
	}
	// Round 1 approved head 1; the head moved and round 2 runs on head 2.
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	acts := []Action{
		{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
	}
	tk := taken(t, t0, acts...)
	tk = append(tk, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	if got := Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved), Rounds: 2, Taken: tk}); len(got) != 0 {
		t.Errorf("round 1's approval gave the in-flight round's head %+v", got)
	}
	// A verdict on head 2 from before round 2 started is no verdict of it.
	early := Review{ID: "R2", Head: head2, Verdict: Approved, At: t0.Add(time.Hour)}
	if got := Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved, early), Rounds: 2, Taken: tk}); len(got) != 0 {
		t.Errorf("a verdict from before round 2 gave %+v", got)
	}
	late := Review{ID: "R3", Head: head2, Verdict: Approved, At: t0.Add(3 * time.Hour)}
	if a := only(t, Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved, late), Rounds: 2, Taken: tk}), KindGo); a.Head != head2 {
		t.Errorf("go = %+v", a)
	}
}

// A head that moved past an approval is a new review, whatever
// supervisor.reviewRounds says: the cap counts change-request rounds.
func TestANewHeadAfterAnApprovalGetsItsOwnReview(t *testing.T) {
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	for _, rounds := range []int{1, 2} {
		in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved), Rounds: rounds, Taken: tk}
		if a := only(t, Plan(in), KindReview); a.Round != 2 || a.Head != head2 {
			t.Errorf("rounds %d: review = %+v", rounds, a)
		}
	}
}

// An approved round leaves the change-request round for the next head's
// review: its findings go back with one more round, the fixes then merge.
func TestAnApprovedRoundIsNoChangeRequestRound(t *testing.T) {
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	changes := Review{ID: "R2", Head: head2, Verdict: ChangesRequested, Body: "fix y", At: t0.Add(3 * time.Hour)}
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved, changes), Rounds: 1, Taken: tk}
	if a := only(t, Plan(in), KindForward); !strings.Contains(a.Message, "no further review runs") {
		t.Errorf("forward = %q", a.Message)
	}
	tk = append(tk, taken(t, t0.Add(4*time.Hour), Action{Key: forwardKey(pr, "R2"), Kind: KindForward, Ref: pr, Agent: worker})...)
	in = Input{Workers: parked(t0.Add(5 * time.Hour)), Pulls: pull(head3, true, approved, changes), Rounds: 1, Taken: tk}
	if a := only(t, Plan(in), KindGo); a.Head != head3 || !strings.Contains(a.Message, "1 of 1 change-request round(s) used") {
		t.Errorf("go = %+v", a)
	}
}

// Findings reach the worker even when its head moved before the loop saw
// them, and its fixes count only after it parked again.
func TestFindingsAreForwardedAfterTheHeadMoved(t *testing.T) {
	changes := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, Body: "fix x", At: t1}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	in := Input{Workers: parked(t0), Pulls: pull(head2, true, changes), Rounds: 1, Taken: taken(t, t0, started)}
	if a := only(t, Plan(in), KindForward); !strings.HasSuffix(a.Message, "fix x") {
		t.Errorf("forward = %+v", a)
	}
	fwd := Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker}
	in.Taken = append(in.Taken, taken(t, t0.Add(2*time.Minute), fwd)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a park from before the forward got %+v", got)
	}
	in.Workers = parked(t0.Add(time.Hour))
	if a := only(t, Plan(in), KindGo); a.Head != head2 {
		t.Errorf("go = %+v", a)
	}
}

func TestAReviewAskingForChangesIsForwardedWordForWord(t *testing.T) {
	body := "Review: changes requested\nHead reviewed: `" + head1 + "`\n\n- cmd/x.go:12: the nil map panics"
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, Body: body, At: t1}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	a := only(t, Plan(Input{Workers: parked(t0), Pulls: pull(head1, true, rev), Rounds: 1, Taken: taken(t, t0, started)}), KindForward)
	if !strings.HasSuffix(a.Message, body) || !strings.Contains(a.Message, "no further review runs") {
		t.Errorf("forward on the last round = %q", a.Message)
	}
	a = only(t, Plan(Input{Workers: parked(t0), Pulls: pull(head1, true, rev), Rounds: 2, Taken: taken(t, t0, started)}), KindForward)
	if !strings.Contains(a.Message, "the next review round starts then") {
		t.Errorf("forward with a round left = %q", a.Message)
	}
}

func TestTheNextRoundStartsOnTheFixedHead(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	acts := []Action{
		{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker},
	}
	a := only(t, Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, rev), Rounds: 2, Taken: taken(t, t0, acts...)}), KindReview)
	if a.Round != 2 || a.Head != head2 {
		t.Errorf("round two = %+v", a)
	}
}

func TestTheFixesOfTheLastRoundMergeWithoutAnotherReview(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	acts := []Action{
		{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker},
	}
	a := only(t, Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, rev), Rounds: 1, Taken: taken(t, t0, acts...)}), KindGo)
	if a.Head != head2 {
		t.Errorf("go = %+v, want the fixed head", a)
	}
	if acts := Plan(Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, false, rev), Rounds: 1, Taken: taken(t, t0, acts...)}); len(acts) != 0 {
		t.Errorf("a fixed head with red checks got %+v", acts)
	}
}

func TestOpenFindingsPastTheLastRoundGoToTheGuide(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	acts := []Action{
		{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker},
	}
	// Parked before the forward: nothing yet.
	if got := Plan(Input{Workers: parked(t0.Add(-time.Minute)), Pulls: pull(head1, true, rev), Rounds: 1, Taken: taken(t, t0, acts...)}); len(got) != 0 {
		t.Errorf("a park from before the forward escalated %+v", got)
	}
	in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head1, true, rev), Rounds: 1, Taken: taken(t, t0, acts...)}
	esc := only(t, Plan(in), KindEscalate)
	if esc.Ref != pr || esc.Head != head1 || esc.Question != EscalationQuestion(prURL, head1) ||
		!strings.Contains(esc.Question, "findings unfixed") || !strings.Contains(esc.Message, "without a new head") {
		t.Errorf("escalate = %+v", esc)
	}
	// The loop stops for that pull request: no more reads, no more actions.
	in.Taken = append(in.Taken, taken(t, t0.Add(2*time.Hour), esc)...)
	in.Pulls = pull(head2, true, rev)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("an escalated pull request got %+v", got)
	}
	if got := Tracked(in.Workers, in.Taken, nil); len(got) != 0 {
		t.Errorf("an escalated pull request is still tracked: %v", got)
	}
}

func TestAMergedPullRequestEndsItsTracking(t *testing.T) {
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	tk := taken(t, t0, started)
	if got := Tracked(nil, tk, nil); len(got) != 1 || got[0] != pr {
		t.Fatalf("tracked = %v", got)
	}
	p := pull(head1, true)
	p[pr] = Pull{Ref: pr, URL: prURL, State: Merged, Head: head1}
	end := only(t, Plan(Input{Pulls: p, Rounds: 1, Taken: tk}), KindEnd)
	if got := Tracked(nil, append(tk, taken(t, t0, end)...), nil); len(got) != 0 {
		t.Errorf("tracked after the end = %v", got)
	}
}

func TestADoneWorkerLeavesTheRosterThenTheNextStarts(t *testing.T) {
	done := Event{At: t0, Verb: VerbDone, Agent: worker, Detail: worker + " done: fix it"}
	report := Event{At: t0, Verb: VerbReport, Agent: worker, Detail: worker + ": merged"}
	in := Input{Events: []Event{done, report}}
	d := only(t, Plan(in), KindDone)
	if d.Agent != worker {
		t.Errorf("done = %+v", d)
	}
	in.Taken = taken(t, t0, d)
	n := only(t, Plan(in), KindNext)
	in.Taken = append(in.Taken, taken(t, t0, n)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("after the next start: %+v", got)
	}
}

func TestAReviewerLeavesOnceItsVerdictIsActedOn(t *testing.T) {
	reviewer := ReviewerName(pr, 1)
	if reviewer != "Review r 7 r1" {
		t.Fatalf("reviewer name = %q", reviewer)
	}
	workers := append(parked(t0), Worker{Name: reviewer, Started: true})
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	in := Input{Workers: workers, Pulls: pull(head1, true, Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}), Rounds: 1, Taken: tk}
	g := only(t, Plan(in), KindGo)
	in.Taken = append(in.Taken, taken(t, t1, g)...)
	d := only(t, Plan(in), KindDone)
	if d.Agent != reviewer || d.Ref != pr || d.Event != nil {
		t.Errorf("done = %+v", d)
	}
	in.Taken = append(in.Taken, taken(t, t1, d)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("after the reviewer left: %+v", got)
	}
	// A reviewer its person started stays.
	in.Workers = append(parked(t0), Worker{Name: reviewer})
	in.Taken = append(tk, taken(t, t1, g)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a person's session was retired: %+v", got)
	}
}

func TestAForwardedVerdictRetiresItsReviewer(t *testing.T) {
	reviewer := ReviewerName(pr, 1)
	workers := append(parked(t0), Worker{Name: reviewer, Started: true})
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	in := Input{Workers: workers, Pulls: pull(head1, true, Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}), Rounds: 2, Taken: tk}
	f := only(t, Plan(in), KindForward)
	in.Taken = append(in.Taken, taken(t, t1, f)...)
	if d := only(t, Plan(in), KindDone); d.Agent != reviewer {
		t.Errorf("done = %+v", d)
	}
}

func TestAMergedWorkerLeavesWhetherItReportedOrNot(t *testing.T) {
	w := parked(t0)
	w[0].Started = true
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t1, Action{Key: goKey(pr, head1), Kind: KindGo, Ref: pr, Agent: worker})...)
	merged := map[string]Pull{pr: {Ref: pr, URL: prURL, State: Merged, Head: head1}}
	in := Input{Workers: w, Pulls: merged, Rounds: 1, Taken: tk}
	acts := Plan(in)
	i := slices.IndexFunc(acts, func(a Action) bool { return a.Kind == KindDone })
	if len(acts) != 2 || i < 0 || acts[i].Agent != worker || acts[i].Ref != pr {
		t.Fatalf("want the end and the worker's done, got %+v", acts)
	}
	// Its turn ran on past the end: the merged pull request stays read
	// until the worker left.
	in.Taken = append(in.Taken, taken(t, t1, acts[1-i])...)
	if got := Tracked(w, in.Taken, nil); len(got) != 1 || got[0] != pr {
		t.Fatalf("tracked after the end = %v", got)
	}
	d := only(t, Plan(in), KindDone)
	in.Taken = append(in.Taken, taken(t, t1, d)...)
	if got := Tracked(w, in.Taken, nil); len(got) != 0 {
		t.Errorf("tracked after the worker left = %v", got)
	}
	// A worker its person started stays; one that reported done leaves by
	// its own done.
	if got := Plan(Input{Workers: parked(t0), Pulls: merged, Rounds: 1, Taken: tk}); len(got) != 1 || got[0].Kind != KindEnd {
		t.Errorf("a person's worker: %+v", got)
	}
	done := Event{At: t1, Verb: VerbDone, Agent: worker, Detail: worker + " done: merged"}
	acts = Plan(Input{Workers: w, Pulls: merged, Rounds: 1, Taken: tk, Events: []Event{done}})
	if n := slices.IndexFunc(acts, func(a Action) bool { return a.Kind == KindDone && a.Event == nil }); n >= 0 {
		t.Errorf("a reported worker was retired too: %+v", acts)
	}
	// A worker parked on anything but its pull request still works (its
	// release, a timer); one parked on the pull request is done.
	busy := parked(t0)
	busy[0].Started, busy[0].ParkedOn = true, "release v1.2.3"
	if got := Plan(Input{Workers: busy, Pulls: merged, Rounds: 1, Taken: tk}); len(got) != 1 || got[0].Kind != KindEnd {
		t.Errorf("a worker parked on its release: %+v", got)
	}
	if got := Tracked(busy, append(tk, taken(t, t1, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr})...), nil); len(got) != 0 {
		t.Errorf("tracked for a worker parked on its release = %v", got)
	}
	free := parked(t0)
	free[0].Started, free[0].ParkedOn = true, ""
	if d := Plan(Input{Workers: free, Pulls: merged, Rounds: 1, Taken: tk}); !slices.ContainsFunc(d, func(a Action) bool { return a.Kind == KindDone && a.Agent == worker }) {
		t.Errorf("an idle worker stays: %+v", d)
	}
	// No go, no retirement.
	if got := Plan(Input{Workers: w, Pulls: merged, Rounds: 1, Taken: tk[:1]}); len(got) != 1 || got[0].Kind != KindEnd {
		t.Errorf("without the go: %+v", got)
	}
}

func TestANewerReleaseIsInstalledOnce(t *testing.T) {
	a := only(t, Plan(Input{Release: release}), KindRelease)
	if a.Version != release {
		t.Errorf("release = %+v", a)
	}
	if got := Plan(Input{Release: release, Taken: taken(t, t0, a)}); len(got) != 0 {
		t.Errorf("installed twice: %+v", got)
	}
}

func TestWhatTheRulesCannotPlaceIsClassified(t *testing.T) {
	for name, e := range map[string]Event{
		"a report without done": {At: t0, Verb: VerbReport, Agent: worker, Detail: worker + ": stuck on the lab"},
		"a problem":             {At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": CI is broken"},
		"a park on nothing":     {At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: the person's call"},
	} {
		a := only(t, Plan(Input{Events: []Event{e}}), KindClassify)
		if a.Event == nil || a.Event.Detail != e.Detail {
			t.Errorf("%s: classify = %+v", name, a)
		}
	}
	for name, e := range map[string]Event{
		"a park on a pull request": {At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked on " + pr + ": desk review"},
		"a park on a note":         {At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked on #12: an answer"},
	} {
		if got := Plan(Input{Events: []Event{e}}); len(got) != 0 {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestAWorkersQuestionForSomeoneElseGoesToTheGuide(t *testing.T) {
	// Asked whenever: the open note decides, no event needed.
	in := Input{Person: "pat", Workers: []Worker{{Name: worker}}, Notes: []Note{{ID: 4, For: "supervisor", By: worker, Text: "which lab?"}}}
	a := only(t, Plan(in), KindQuestion)
	if a.Note != 4 {
		t.Errorf("question = %+v", a)
	}
	in.Notes[0].For = "Pat"
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a note for the guide's person already: %+v", got)
	}
	in.Notes[0].For, in.Notes[0].By = "supervisor", "someone off the roster"
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a note of no worker: %+v", got)
	}
}

func TestVerdicts(t *testing.T) {
	for _, c := range []struct{ state, body, want string }{
		{"APPROVED", "", Approved},
		{"CHANGES_REQUESTED", "", ChangesRequested},
		{stateCommented, "Desk review (model): approved\n\nfine", Approved},
		{stateCommented, "Review: Changes requested.\n- x", ChangesRequested},
		{stateCommented, "a remark: approved by whom?\n", ""},
		{stateCommented, "nice", ""},
	} {
		if got := Verdict(c.state, c.body); got != c.want {
			t.Errorf("Verdict(%s, %q) = %q, want %q", c.state, c.body, got, c.want)
		}
	}
	if got := ReviewedHead("x\nHead reviewed: `1234567`\n", head1); got != "1234567" {
		t.Errorf("ReviewedHead = %q", got)
	}
	if got := ReviewedHead("x", head1); got != head1 {
		t.Errorf("ReviewedHead without a line = %q", got)
	}
	if !sameHead("1111111", head1) || sameHead("2222222", head1) {
		t.Error("sameHead does not match a short head")
	}
}

// agents idle --done logs its done and its report a moment apart.
func TestAReportLoggedWithItsDoneIsNotClassified(t *testing.T) {
	done := Event{At: t0, Verb: VerbDone, Agent: worker, Detail: worker + " done: fix it"}
	report := Event{At: t0.Add(12 * time.Microsecond), Verb: VerbReport, Agent: worker, Detail: worker + ": merged"}
	only(t, Plan(Input{Events: []Event{done, report}}), KindDone)
	late := Event{At: t0.Add(time.Hour), Verb: VerbReport, Agent: worker, Detail: worker + ": stuck"}
	if acts := Plan(Input{Events: []Event{done, late}}); len(acts) != 2 || acts[1].Kind != KindClassify {
		t.Errorf("a later report = %+v", acts)
	}
}

// The party that logs an agent's event carries its session's title; the
// roster name is the detail's.
func TestEventAgent(t *testing.T) {
	for _, c := range []struct{ verb, detail, want string }{
		{VerbDone, worker + " done: fix it", worker},
		{VerbReport, worker + ": merged: v1", worker},
		{VerbPark, worker + ": parked on " + pr + ": review", worker},
		{"lease.claim", "lab-1: for x", "title"},
		{VerbDone, "garbled", "title"},
	} {
		if got := EventAgent(c.verb, c.detail, "title"); got != c.want {
			t.Errorf("EventAgent(%s, %q) = %q, want %q", c.verb, c.detail, got, c.want)
		}
	}
}

// escalated is the pull request of TestOpenFindingsPastTheLastRoundGoToTheGuide
// handed to the guide on head1, and the person's answer an hour later.
func escalated(t *testing.T, words string) Input {
	t.Helper()
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	tk := taken(t, t0,
		Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t0.Add(2*time.Hour), Action{Key: escalateKey(pr, head1), Kind: KindEscalate, Ref: pr, Agent: worker})...)
	in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head1, true, rev), Rounds: 1, Taken: tk}
	if words != "" {
		in.Answers = []Answer{{Ref: pr, Note: 4, Question: EscalationQuestion(prURL, head1), Text: words, By: "person", At: t0.Add(3 * time.Hour)}}
	}
	return in
}

// The person's release of an escalation is the loop's merge go for the
// head it named.
func TestAnAnswerReleasingAnEscalationIsTheMergeGo(t *testing.T) {
	if got := Plan(escalated(t, "")); len(got) != 0 {
		t.Errorf("an unanswered escalation got %+v", got)
	}
	for _, words := range []string{"Merge", "Merge — the findings are cosmetic", "you can merge", "ok, ship it"} {
		in := escalated(t, words)
		if got := Tracked(in.Workers, in.Taken, in.Answers); len(got) != 1 || got[0] != pr {
			t.Errorf("%q: an answered escalation is not tracked: %v", words, got)
		}
		a := only(t, Plan(in), KindGo)
		if a.Head != head1 || !strings.Contains(a.Message, "--match-head-commit "+head1) || !strings.Contains(a.Message, "note #4") {
			t.Errorf("%q: go = %+v", words, a)
		}
		// Red checks hold the go back.
		in.Pulls = pull(head1, false, in.Pulls[pr].Reviews...)
		if got := Plan(in); len(got) != 0 {
			t.Errorf("%q: a red head got %+v", words, got)
		}
	}
	// An answer to an older escalation, or to another note, counts not.
	in := escalated(t, "Merge")
	in.Answers[0].At = t0.Add(time.Hour)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("an answer from before the escalation got %+v", got)
	}
	in = escalated(t, "Merge")
	in.Answers[0].Question = "Decide what worker one does next about its problem (blocker)?"
	if got := Plan(in); len(got) != 0 {
		t.Errorf("an answer to another note got %+v", got)
	}
}

// Any other answer goes to the worker, once, and the rounds go on.
func TestAnAnswerKeepingAnEscalationGoesToTheWorker(t *testing.T) {
	for _, words := range []string{"Rework", "Rework — fix the panic first", "do not merge yet", "why is x needed?"} {
		in := escalated(t, words)
		a := only(t, Plan(in), KindAnswer)
		if a.Agent != worker || !strings.Contains(a.Message, words) || !strings.Contains(a.Message, "rework") {
			t.Errorf("%q: answer = %+v", words, a)
		}
		in.Taken = append(in.Taken, taken(t, t0.Add(4*time.Hour), a)...)
		if got := Plan(in); len(got) != 0 {
			t.Errorf("%q: told twice or escalated again: %+v", words, got)
		}
		// The reworked head carries the fixes: the merge go.
		in.Workers = parked(t0.Add(5 * time.Hour))
		in.Pulls = pull(head2, true, in.Pulls[pr].Reviews...)
		if g := only(t, Plan(in), KindGo); g.Head != head2 {
			t.Errorf("%q: go = %+v", words, g)
		}
	}
}

func TestReleases(t *testing.T) {
	for words, want := range map[string]bool{
		"Merge": true, "merge — fine": true, "you can merge": true, "LGTM": true, "approved": true,
		"Rework": false, "Rework — merge after the fix": false, "don't merge": false, "do not merge": false,
		"not yet": false, "wait for x, then merge": false, "": false, "why?": false, "can't merge this": false,
	} {
		if got := Releases(words); got != want {
			t.Errorf("Releases(%q) = %v, want %v", words, got, want)
		}
	}
}
