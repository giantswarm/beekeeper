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
	// guidePerson is the guide's person.
	guidePerson = "pat"
	// mergeAnswer is the label of an escalation's releasing option.
	mergeAnswer = "Merge"
)

var (
	t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	// t1 is when a round's reviewer posts its verdict.
	t1 = t0.Add(time.Minute)
)

func parked(at time.Time) []Worker {
	return []Worker{{Name: worker, ReviewRequested: true, ParkedOn: pr, ParkedAt: at}}
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

// A head that only merges the base into the approved one (the gate's
// --update-branch) gets the go without a review; any other new head gets
// its review and no go.
func TestABaseMergeAfterAnApprovalGetsTheGo(t *testing.T) {
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	var asked []string
	baseMerged := func(only bool) func(Pull, string) bool {
		return func(p Pull, from string) bool {
			asked = append(asked, from+".."+p.Head)
			return only
		}
	}
	in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk, BaseMerged: baseMerged(true)}
	a := only(t, Plan(in), KindGo)
	if a.Head != head2 || !strings.Contains(a.Message, "approved head "+short(head1)+"; head "+short(head2)+" only brings the base into it") {
		t.Errorf("go = %+v", a)
	}
	if !slices.Equal(asked, []string{head1 + ".." + head2}) {
		t.Errorf("asked %v", asked)
	}
	// Taken once: the next pass gives no second go.
	in.Taken = append(slices.Clone(tk), taken(t, t0.Add(2*time.Hour), a)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a second pass took %+v", got)
	}
	// Any other change is a new head with its own review.
	in = Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk, BaseMerged: baseMerged(false)}
	if a := only(t, Plan(in), KindReview); a.Head != head2 {
		t.Errorf("review = %+v", a)
	}
	// Red checks on the merged head: no go, and no read of GitHub.
	asked = nil
	in = Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head2, false, approved), Rounds: 1, Taken: tk, BaseMerged: baseMerged(true)}
	if got := Plan(in); len(got) != 0 || len(asked) != 0 {
		t.Errorf("a red base merge got %+v after asking %v", got, asked)
	}
}

// The go names the merge command of the repository's path.
func TestTheGoNamesTheRepositorysMergeCommand(t *testing.T) {
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	in := Input{Workers: parked(t0), Pulls: pull(head1, true, approved), Rounds: 1, Taken: tk, DevctlOwners: []string{"O"}}
	a := only(t, Plan(in), KindGo)
	if !strings.Contains(a.Message, "`devctl pr merge o/r 7 --update-branch`") || !strings.Contains(a.Message, "merging the base alone") || strings.Contains(a.Message, "--match-head-commit") {
		t.Errorf("a devctl repository's go = %q", a.Message)
	}
	in.DevctlOwners = []string{"giantswarm"}
	a = only(t, Plan(in), KindGo)
	if !strings.Contains(a.Message, "`gh pr merge 7 --repo o/r --squash --match-head-commit "+head1+"`") || strings.Contains(a.Message, "devctl") {
		t.Errorf("a gh repository's go = %q", a.Message)
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

// https://github.com/giantswarm/beekeeper/issues/1795: used-up review
// rounds are the loop's inner loop. Findings that stand past the last round
// go back to the worker to rework, never to the person as a merge question.
func TestOpenFindingsPastTheLastRoundGoBackToTheWorker(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	acts := []Action{
		{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker},
	}
	// Parked before the forward: nothing yet.
	if got := Plan(Input{Workers: parked(t0.Add(-time.Minute)), Pulls: pull(head1, true, rev), Rounds: 1, Taken: taken(t, t0, acts...)}); len(got) != 0 {
		t.Errorf("a park from before the forward got %+v", got)
	}
	in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head1, true, rev), Rounds: 1, Person: guidePerson, Taken: taken(t, t0, acts...)}
	rw := only(t, Plan(in), KindRework)
	if rw.Ref != pr || rw.Head != head1 || rw.Agent != worker || rw.Question != "" ||
		!strings.Contains(rw.Message, prURL) || !strings.Contains(rw.Message, "fix every finding") || !strings.Contains(rw.Message, "1 of 1") {
		t.Errorf("rework = %+v", rw)
	}
	// Told once for that park; still tracked, so its fixes get their go.
	in.Taken = append(in.Taken, taken(t, t0.Add(2*time.Hour), rw)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a rework told already got %+v", got)
	}
	if got := Tracked(in.Workers, in.Taken, nil, Desk{}); !slices.Equal(got, []string{pr}) {
		t.Errorf("a reworked pull request is not tracked: %v", got)
	}
	// Parked again on the unchanged head: told again, never escalated.
	in.Workers = parked(t0.Add(3 * time.Hour))
	if again := only(t, Plan(in), KindRework); again.Key == rw.Key {
		t.Errorf("the second park's rework has the first's key %q", again.Key)
	}
	// Its fixes pushed and parked on: the merge go for the new head.
	in.Pulls = pull(head2, true, rev)
	if g := only(t, Plan(in), KindGo); g.Head != head2 {
		t.Errorf("go = %+v, want the fixed head", g)
	}
}

// A head that only merges the base into the head a review asked changes on
// carries no fixes: the worker reworks, with or without a round left, and
// no go comes; a head with fixes after it is judged as before.
func TestABaseMergeAfterAChangeRequestGetsNoGo(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	tk := taken(t, t0,
		Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker})
	baseMerged := func(p Pull, from string) bool { return from == head1 && p.Head == head2 }
	for rounds, next := range map[int]string{1: "the merge go for the new head follows", 2: "the next review round starts then"} {
		p := pull(head2, true, rev)
		pp := p[pr]
		pp.Base = "main"
		p[pr] = pp
		in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: p, Rounds: rounds, Taken: tk, BaseMerged: baseMerged}
		rw := only(t, Plan(in), KindRework)
		if rw.Head != head2 || !strings.Contains(rw.Message, "only brings main into head "+short(head1)) || !strings.Contains(rw.Message, "stand unfixed") || !strings.Contains(rw.Message, next) {
			t.Errorf("rounds %d: rework = %+v", rounds, rw)
		}
		// Its fixes on top: the go (no round left) or the next round.
		in.Pulls = pull(head3, true, rev)
		want := map[int]string{1: KindGo, 2: KindReview}[rounds]
		if a := only(t, Plan(in), want); a.Head != head3 {
			t.Errorf("rounds %d: %s = %+v", rounds, want, a)
		}
	}
}

// An approved head the worker moved past with more commits is reviewed
// again, once: a change past MaxReviews is the person's decision (merge it
// without a third review, or rework), asked once per head, whatever
// supervisor.reviewRounds says.
func TestAThirdReviewIsThePersonsDecision(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	r2 := Review{ID: "R2", Head: head2, Verdict: Approved, At: t0.Add(2*time.Hour + time.Minute)}
	acts := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker})
	acts = append(acts, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	in := Input{Workers: parked(t0.Add(3 * time.Hour)), Pulls: pull(head3, true, r1, r2), Rounds: 5, Person: guidePerson, Taken: acts}
	a := only(t, Plan(in), KindEscalate)
	if a.Head != head3 || a.Question != EscalationQuestion(prURL, head3) || !strings.Contains(a.Message, "2 reviews are used") {
		t.Errorf("escalation = %+v", a)
	}
	// The answer carries an option's label: Merge releases, Rework not.
	opts := EscalationOptions(head3)
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i], _, _ = strings.Cut(o, ":")
	}
	if len(labels) != 2 || labels[0] != mergeAnswer || !Releases(labels[0]) || Releases(labels[1]) {
		t.Errorf("options %q", opts)
	}
	// Asked once: the next pass, and one with the person's answer still
	// out, ask nothing again.
	in.Taken = append(in.Taken, taken(t, t0.Add(4*time.Hour), a)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a second pass took %+v", got)
	}
	// The person releases it: the go for that head.
	in.Answers = []Answer{{Ref: pr, Note: 9, Question: a.Question, Text: mergeAnswer, By: guidePerson, At: t0.Add(5 * time.Hour)}}
	if g := only(t, Plan(in), KindGo); g.Head != head3 || !strings.Contains(g.Message, "note #9") {
		t.Errorf("go = %+v", g)
	}
	// A rework answer goes to the worker; its next head is asked about
	// again, never reviewed a third time.
	in.Answers[0].Text = "Rework — split it out"
	an := only(t, Plan(in), KindAnswer)
	in.Taken = append(in.Taken, taken(t, t0.Add(6*time.Hour), an)...)
	in.Workers = parked(t0.Add(7 * time.Hour))
	in.Pulls = pull("4444444444444444444444444444444444444444", true, r1, r2)
	if e := only(t, Plan(in), KindEscalate); e.Head != "4444444444444444444444444444444444444444" {
		t.Errorf("escalation of the reworked head = %+v", e)
	}
}

// A second review that asks for changes is forwarded; its fixed head gets
// the go, never a third review, whatever supervisor.reviewRounds says.
func TestFindingsOfASecondReviewAreFixedWithoutAThird(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	r2 := Review{ID: "R2", Head: head2, Verdict: ChangesRequested, At: t0.Add(2*time.Hour + time.Minute)}
	acts := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	acts = append(acts, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: forwardKey(pr, "R2"), Kind: KindForward, Ref: pr, Agent: worker})...)
	in := Input{Workers: parked(t0.Add(3 * time.Hour)), Pulls: pull(head3, true, r1, r2), Rounds: 5, Taken: acts}
	if g := only(t, Plan(in), KindGo); g.Head != head3 || !strings.Contains(g.Message, "2 of 2 review(s)") {
		t.Errorf("go = %+v", g)
	}
	// Parked again on the head the findings were about: rework, with no
	// next round promised.
	in.Pulls = pull(head2, true, r1, r2)
	if rw := only(t, Plan(in), KindRework); !strings.Contains(rw.Message, "the merge go for the new head follows") {
		t.Errorf("rework = %+v", rw)
	}
}

// One review per pull request: a merge that fails after its go and the
// worker's conflict resolution, again and again, never starts a new
// reviewer; each covered head gets its go.
func TestAnApprovedHeadResolvedAgainAndAgainGetsNoNewReview(t *testing.T) {
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	in := Input{Workers: parked(t0), Pulls: pull(head1, true), Rounds: 1, BaseMerged: func(Pull, string) bool { return true }}
	r := only(t, Plan(in), KindReview)
	in.Taken = taken(t, t0, r)
	in.Pulls = pull(head1, true, approved)
	in.Taken = append(in.Taken, taken(t, t0.Add(time.Hour), only(t, Plan(in), KindGo))...)
	for i, head := range []string{head2, head3, "4444444444444444444444444444444444444444"} {
		at := t0.Add(time.Duration(2+i) * time.Hour)
		in.Workers = parked(at)
		in.Pulls = pull(head, true, approved)
		g := only(t, Plan(in), KindGo)
		if g.Head != head {
			t.Errorf("head %d: go = %+v", i+2, g)
		}
		in.Taken = append(in.Taken, taken(t, at.Add(time.Minute), g)...)
	}
	starts := 0
	for _, tk := range in.Taken {
		if tk.Kind == KindReview {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("%d review starts, want 1", starts)
	}
}

// Findings are forwarded once and the worker's fixed head gets the go on
// green checks, with no second review, under the default rounds.
func TestForwardedFindingsGetTheGoOnTheFixedHead(t *testing.T) {
	changes := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	in := Input{Workers: parked(t0), Pulls: pull(head1, true), Taken: nil}
	in.Taken = taken(t, t0, only(t, Plan(in), KindReview))
	in.Pulls = pull(head1, true, changes)
	in.Taken = append(in.Taken, taken(t, t0.Add(time.Hour), only(t, Plan(in), KindForward))...)
	in.Workers = parked(t0.Add(2 * time.Hour))
	in.Pulls = pull(head2, false, changes)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a fixed head with red checks got %+v", got)
	}
	in.Pulls = pull(head2, true, changes)
	if g := only(t, Plan(in), KindGo); g.Head != head2 {
		t.Errorf("go = %+v", g)
	}
}

func TestAMergedPullRequestEndsItsTracking(t *testing.T) {
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	tk := taken(t, t0, started)
	if got := Tracked(nil, tk, nil, Desk{}); len(got) != 1 || got[0] != pr {
		t.Fatalf("tracked = %v", got)
	}
	p := pull(head1, true)
	p[pr] = Pull{Ref: pr, URL: prURL, State: Merged, Head: head1}
	end := only(t, Plan(Input{Pulls: p, Rounds: 1, Taken: tk}), KindEnd)
	if got := Tracked(nil, append(tk, taken(t, t0, end)...), nil, Desk{}); len(got) != 0 {
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

func TestAMergedWorkerWaitsForItsReportOrInactivityWindow(t *testing.T) {
	w := parked(t0)
	w[0].Started = true
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t1, Action{Key: goKey(pr, head1), Kind: KindGo, Ref: pr, Agent: worker})...)
	merged := map[string]Pull{pr: {Ref: pr, URL: prURL, State: Merged, Head: head1, MergeCommit: head2}}
	in := Input{Workers: w, Pulls: merged, Taken: tk, Now: t1}
	end := only(t, Plan(in), KindEnd)
	in.Taken = append(in.Taken, taken(t, t1, end)...)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("retired at merge: %+v", got)
	}
	for _, park := range []string{pr, "", "release v1.2.3", "#12"} {
		in.Workers[0].ParkedOn = park
		if got := Tracked(in.Workers, in.Taken, nil, Desk{}); len(got) != 1 {
			t.Fatalf("lost retained worker on %q: %v", park, got)
		}
		if got := ReleaseWorkers(in); len(got) != 1 || got[0].Head != head2 {
			t.Fatalf("no release recipient: %+v", got)
		}
	}
	in.Now = t1.Add(2*time.Hour - time.Second)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("retired before window: %+v", got)
	}
	in.Now = t1.Add(2 * time.Hour)
	d := only(t, Plan(in), KindDone)
	if !strings.Contains(d.Message, "no report after merge") || !strings.Contains(d.Message, prURL) {
		t.Fatalf("missing evidence: %+v", d)
	}
	// Activity extends the window, including across loop restarts.
	in.Workers[0].LastActive = t1.Add(time.Hour)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("retired active worker: %+v", got)
	}
	in.PostMergeWindow = 30 * time.Minute
	only(t, Plan(in), KindDone)
	// A done report retires immediately through the normal report path.
	in.Events = []Event{{At: in.Now, Verb: VerbDone, Agent: worker, Detail: worker + " done: proven"}}
	d = only(t, Plan(in), KindDone)
	if d.Event == nil {
		t.Fatal("ignored worker report")
	}
	if got := ReleaseWorkers(in); len(got) != 0 {
		t.Fatalf("wake after done: %+v", got)
	}
	in.Events = nil
	in.Taken = append(in.Taken, taken(t, in.Now, Action{Key: retireKey(pr, retiredWorker), Kind: KindDone, Ref: pr, Agent: worker})...)
	if got := Tracked(in.Workers, in.Taken, nil, Desk{}); len(got) != 0 {
		t.Fatalf("tracked retired worker: %v", got)
	}
	// No merge go and no beekeeper start mean no automatic retirement.
	if got := Plan(Input{Workers: parked(t0), Pulls: merged, Taken: tk, Now: in.Now}); len(got) != 1 || got[0].Kind != KindEnd {
		t.Fatalf("person's worker: %+v", got)
	}
	if got := Plan(Input{Workers: w, Pulls: merged, Taken: tk[:1], Now: in.Now}); len(got) != 1 || got[0].Kind != KindEnd {
		t.Fatalf("no merge go: %+v", got)
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
		"a park on nothing":     {At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: the person's call"},
	} {
		ws := []Worker{{Name: worker, ParkedAt: t0}}
		a := only(t, Plan(Input{Workers: ws, Events: []Event{e}}), KindClassify)
		if a.Event == nil || a.Event.Detail != e.Detail {
			t.Errorf("%s: classify = %+v", name, a)
		}
	}
	for name, e := range map[string]Event{
		"a park on a pull request": {At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked on " + pr + ": desk review"},
		"a park on a note":         {At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked on #12: an answer"},
		"a problem filed":          {At: t0, Verb: VerbProblem, Agent: worker, Detail: worker + ": CI is broken, https://github.com/o/r/issues/9"},
	} {
		if got := Plan(Input{Events: []Event{e}}); len(got) != 0 {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestAWorkersQuestionForSomeoneElseGoesToTheGuide(t *testing.T) {
	// Asked whenever: the open note decides, no event needed.
	in := Input{Person: guidePerson, Workers: []Worker{{Name: worker}}, Notes: []Note{{ID: 4, For: "supervisor", By: worker, Text: "which lab?"}}}
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

// A problem of a done report reaches whoever files it: one naming its
// issue is filed and rides with the done; one filed nowhere goes to the
// guide's feed to file first, and the done follows.
func TestAProblemOfADoneReportReachesItsFiler(t *testing.T) {
	done := Event{At: t0, Verb: VerbDone, Agent: worker, Detail: worker + " done: fix it"}
	filed := Event{At: t0.Add(20 * time.Microsecond), Verb: VerbProblem, Agent: worker, Detail: worker + ": CI flakes in o/r, filed as https://github.com/o/r/issues/9"}
	a := only(t, Plan(Input{Events: []Event{done, filed}}), KindDone)
	if !strings.Contains(a.Message, "1 problem(s) filed") || !strings.Contains(a.Message, "issues/9") {
		t.Errorf("done message = %q", a.Message)
	}
	unfiled := Event{At: t0.Add(30 * time.Microsecond), Verb: VerbProblem, Agent: worker, Detail: worker + ": the lab's gate refuses a green head, o/r"}
	in := Input{Events: []Event{done, filed, unfiled}}
	f := only(t, Plan(in), KindFile)
	if f.Message != "the lab's gate refuses a green head, o/r" || f.Event == nil || f.Event.Verb != VerbDone {
		t.Errorf("file = %+v", f)
	}
	in.Taken = taken(t, t1, f)
	a = only(t, Plan(in), KindDone)
	if !strings.Contains(a.Message, "1 problem(s) filed nowhere are in the guide's feed") {
		t.Errorf("done message after the file = %q", a.Message)
	}
	late := Event{At: t0.Add(time.Hour), Verb: VerbProblem, Agent: worker, Detail: worker + ": the lab is gone"}
	if acts := Plan(Input{Events: []Event{done, late}}); len(acts) != 2 || acts[1].Kind != KindFile || acts[1].Event.Verb != VerbProblem {
		t.Errorf("a problem logged later = %+v", acts)
	}
	if !Filed("see https://github.com/o/r/issues/9") || Filed("see https://github.com/o/r/pull/9") || Filed("o/r#9") {
		t.Error("Filed takes only an issue's URL")
	}
}

// A reviewer's problem that matches the changes request of its round the
// loop forwards is the forward's, in the pass that forwards it and after;
// one that does not match is the rules' to place.
func TestAReviewersForwardedFindingIsNoNote(t *testing.T) {
	reviewer := ReviewerName(pr, 1)
	rv := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	ask := Review{ID: "r1", Head: head1, Verdict: ChangesRequested, Body: "1. The nil check of the parser is missing in Parse.", At: t1}
	problem := Event{At: t1, Verb: VerbProblem, Agent: reviewer, Detail: reviewer + ": the parser's nil check is missing in Parse"}
	other := Event{At: t1, Verb: VerbProblem, Agent: reviewer, Detail: reviewer + ": the build slot queue timed out twice locally"}
	in := Input{Workers: parked(t0), Pulls: pull(head1, true, ask), Taken: rv, Events: []Event{problem}}
	only(t, Plan(in), KindForward)
	in.Taken = append(rv, taken(t, t1.Add(time.Minute), Action{Key: forwardKey(pr, "r1"), Kind: KindForward, Ref: pr, Agent: worker})...)
	for _, a := range Plan(in) {
		if a.Kind == KindClassify || a.Kind == KindFile {
			t.Errorf("a forwarded finding is filed or classified: %+v", a)
		}
	}
	in.Events = []Event{other}
	if !slices.ContainsFunc(Plan(in), func(a Action) bool { return a.Kind == KindFile && a.Message == problemText(other) }) {
		t.Error("a reviewer's problem the review does not name is dropped")
	}
	// With its done, the matching one is the forward's, the other is to
	// file.
	done := Event{At: t1, Verb: VerbDone, Agent: reviewer, Detail: reviewer + " done: review"}
	in.Events = []Event{done, problem}
	if !slices.ContainsFunc(Plan(in), func(a Action) bool { return a.Kind == KindDone && a.Agent == reviewer }) {
		t.Errorf("a reviewer's done with a forwarded finding = %+v", Plan(in))
	}
	in.Events = []Event{done, problem, other}
	if !slices.ContainsFunc(Plan(in), func(a Action) bool { return a.Kind == KindFile && a.Message == problemText(other) }) {
		t.Errorf("a reviewer's done with its own problem = %+v", Plan(in))
	}
	// Without a forward (no changes request) the reviewer's problem is
	// still one to file.
	in = Input{Workers: parked(t0), Pulls: pull(head1, true), Taken: rv, Events: []Event{problem}}
	only(t, Plan(in), KindFile)
}

// A park on nothing that waits on a timer or a note is a park.
func TestAParkOnATimerOrANoteIsNoDecision(t *testing.T) {
	park := Event{At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: the rollout's timer"}
	for name, in := range map[string]Input{
		"a timer wakes it":     {Workers: []Worker{{Name: worker, ParkedAt: t0, Waking: true}}},
		"its wait names #12":   {Workers: []Worker{{Name: worker, ParkedAt: t0}}, Events: []Event{{At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: the answer of #12"}}},
		"it asked a note":      {Workers: []Worker{{Name: worker, ParkedAt: t0}}, Notes: []Note{{ID: 3, For: guidePerson, By: worker}}, Person: guidePerson},
		"it is parked no more": {Workers: []Worker{{Name: worker}}},
		"it left the roster":   {},
	} {
		if in.Events == nil {
			in.Events = []Event{park}
		}
		if got := Plan(in); len(got) != 0 {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// A wait on owner/repo#n names no note.
	in := Input{Workers: []Worker{{Name: worker, ParkedAt: t0}}, Events: []Event{{At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: the person's call on o/r#12"}}}
	only(t, Plan(in), KindClassify)
}

// escalated is the pull request of TestOpenFindingsPastTheLastRoundGoBackToTheWorker
// as an earlier loop handed it to the guide on head1, and the person's
// answer an hour later.
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
	// One nobody answered holds nothing: the loop's rework goes on.
	unanswered := escalated(t, "")
	only(t, Plan(unanswered), KindRework)
	if got := Tracked(unanswered.Workers, unanswered.Taken, nil, Desk{}); !slices.Equal(got, []string{pr}) {
		t.Errorf("an unanswered escalation is not tracked: %v", got)
	}
	for _, words := range []string{mergeAnswer, "Merge — the findings are cosmetic", "you can merge", "OK, merge it."} {
		in := escalated(t, words)
		if got := Tracked(in.Workers, in.Taken, in.Answers, Desk{}); len(got) != 1 || got[0] != pr {
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
	// A head that only merges the base into the released one has the go.
	in := escalated(t, mergeAnswer)
	in.Pulls = pull(head2, true, in.Pulls[pr].Reviews...)
	in.BaseMerged = func(Pull, string) bool { return true }
	if a := only(t, Plan(in), KindGo); a.Head != head2 || !strings.Contains(a.Message, "only merges the base into the released head "+short(head1)) {
		t.Errorf("a base merge of the released head: go = %+v", a)
	}
	// An answer to an older escalation, or to another note, counts not.
	in = escalated(t, mergeAnswer)
	in.Answers[0].At = t0.Add(time.Hour)
	if got := only(t, Plan(in), KindRework); got.Head != head1 {
		t.Errorf("an answer from before the escalation got %+v", got)
	}
	in = escalated(t, mergeAnswer)
	in.Answers[0].Question = "Decide what worker one does next about its problem (blocker)?"
	if got := only(t, Plan(in), KindRework); got.Head != head1 {
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
		"You can merge.": true, "ship it!": true, "Merge it": true,
		"fix the findings first, then merge": false, "merge main into the branch and fix the findings": false,
		"ship it after fixing finding 2": false, "yes": false, "merge after the fix": false,
	} {
		if got := Releases(words); got != want {
			t.Errorf("Releases(%q) = %v, want %v", words, got, want)
		}
	}
}

// An escalation logged before its key named a head is on the last
// round's head: its release is the go, and no second note follows.
func TestAnEscalationWithoutAHeadIsOnTheLastRoundsHead(t *testing.T) {
	in := escalated(t, mergeAnswer)
	in.Taken[len(in.Taken)-1].Key = "escalate " + pr
	if a := only(t, Plan(in), KindGo); a.Head != head1 {
		t.Errorf("go = %+v", a)
	}
	in = escalated(t, "Rework")
	in.Taken[len(in.Taken)-1].Key = "escalate " + pr
	a := only(t, Plan(in), KindAnswer)
	in.Taken = append(in.Taken, taken(t, t0.Add(4*time.Hour), a)...)
	in.Workers = parked(t0.Add(5 * time.Hour))
	if got := Plan(in); len(got) != 0 {
		t.Errorf("the same head after the answer got %+v", got)
	}
}

// Two desks may share the same GitHub author. A park without publication
// evidence is a wait even with green CI or an old round in this desk's log.
func TestPlainPullWaitNeverReviewsOrForwards(t *testing.T) {
	w := parked(t0)
	w[0].ReviewRequested = false
	in := Input{Workers: w, Pulls: pull(head1, true), Rounds: 1}
	if got := Tracked(w, nil, nil, Desk{}); len(got) != 0 {
		t.Fatalf("plain wait tracked: %v", got)
	}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("plain wait: %+v", got)
	}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	in.Taken = taken(t, t0, started)
	for _, verdict := range []string{Approved, ChangesRequested} {
		in.Pulls = pull(head1, true, Review{ID: "R1", Head: head1, Verdict: verdict, At: t1})
		if got := Plan(in); len(got) != 0 {
			t.Fatalf("plain wait with prior %s: %+v", verdict, got)
		}
	}
}

func TestPlainWaiterCannotTakeAnotherWorkersReview(t *testing.T) {
	in := Input{Workers: append(parked(t0), Worker{Name: "waiter", ParkedOn: pr, ParkedAt: t1}), Pulls: pull(head1, true)}
	a := only(t, Plan(in), KindReview)
	if a.Agent != worker {
		t.Fatalf("review routed to %+v", a)
	}
	in.Taken = taken(t, t0, a)
	in.Pulls = pull(head1, true, Review{ID: "R1", Head: head1, Verdict: Approved, At: t1})
	if a := only(t, Plan(in), KindGo); a.Agent != worker {
		t.Fatalf("go routed to %+v", a)
	}
}

// A plain waiter on another worker's PR cannot hold up a review already
// started for that owner, even while the owner is no longer parked.
func TestPlainWaiterDoesNotBlockAnUnparkedOwnersRound(t *testing.T) {
	workers := []Worker{{Name: worker, Started: true}, {Name: "waiter", ParkedOn: pr, ParkedAt: t1}}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	for _, tc := range []struct{ verdict, kind string }{{Approved, KindGo}, {ChangesRequested, KindForward}} {
		t.Run(tc.kind, func(t *testing.T) {
			in := Input{Workers: workers, Pulls: pull(head1, true, Review{ID: "R1", Head: head1, Verdict: tc.verdict, At: t1}), Rounds: 1, Taken: taken(t, t0, started)}
			if a := only(t, Plan(in), tc.kind); a.Agent != worker {
				t.Fatalf("action routed to %+v, want the round's owner", a)
			}
		})
	}
	for _, tc := range []struct{ answer, kind string }{{"Rework — address the remaining findings", KindAnswer}, {"Merge — the answer releases this head", KindGo}} {
		t.Run("answer/"+tc.answer, func(t *testing.T) {
			in := escalated(t, tc.answer)
			in.Workers = workers
			if a := only(t, Plan(in), tc.kind); a.Agent != worker {
				t.Fatalf("answer routed to %+v, want the round's owner", a)
			}
		})
	}
}

func TestAReplacedParkIsNoDecision(t *testing.T) {
	in := Input{Workers: []Worker{{Name: worker, ParkedAt: t1}}, Events: []Event{{At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: CI pending for " + prURL}}}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("replaced park became a decision: %+v", got)
	}
}

// TestAMultiPullWorkerStaysWhileItServesOpenWork: a merged pull request's
// tracking ends, but its worker stays while it serves an open issue or is
// parked on another open pull request; its done, the close of that work or
// a day's quiet with no park or wake timer retires it.
func TestAMultiPullWorkerStaysWhileItServesOpenWork(t *testing.T) {
	const issue, next = "o/r#70", "o/r#90"
	open := map[string]bool{issue: true, next: true}
	w := parked(t0)
	w[0].Started = true
	w[0].Serves = issue
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t1, Action{Key: goKey(pr, head1), Kind: KindGo, Ref: pr, Agent: worker})...)
	merged := map[string]Pull{pr: {Ref: pr, URL: prURL, State: Merged, Head: head1, MergeCommit: head2}}
	in := Input{Workers: w, Pulls: merged, Taken: tk, Now: t1, Open: func(ref string) bool { return open[ref] }}
	end := only(t, Plan(in), KindEnd)
	in.Taken = append(in.Taken, taken(t, t1, end)...)
	in.Now = t1.Add(3 * time.Hour)

	// Its first pull request merged; it parks on its second, still open.
	in.Workers[0].ParkedOn, in.Workers[0].ParkedAt = next, t1
	keep := only(t, Plan(in), KindLeave)
	if keep.Agent != worker || !strings.Contains(keep.Message, next) {
		t.Fatalf("keep = %+v", keep)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, keep)...)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("kept worker: %+v", got)
	}
	// Plain waits on its open issue keep it too.
	in.Workers[0].ParkedOn = "#12"
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("worker serving an open issue: %+v", got)
	}
	// Quiet a day with no park or wake timer: it goes.
	in.Workers[0].ParkedOn, in.Workers[0].ParkedAt = "", time.Time{}
	in.Now = t1.Add(25 * time.Hour)
	if d := only(t, Plan(in), KindDone); d.Agent != worker {
		t.Fatalf("stale worker: %+v", d)
	}
	in.Workers[0].Waking = true
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("worker with a wake timer: %+v", got)
	}
	in.Workers[0].Waking = false
	in.StaleAfter = 48 * time.Hour
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("agents.staleAfter ignored: %+v", got)
	}
	// Its served issue closes, its second pull request merged: it goes.
	in.Workers[0].ParkedOn, in.Workers[0].ParkedAt = next, t1
	open[issue], open[next] = false, false
	if d := only(t, Plan(in), KindDone); d.Agent != worker || !strings.Contains(d.Message, "no report after merge") {
		t.Fatalf("served work closed: %+v", d)
	}
	// Its done report retires it at once, whatever it serves.
	open[issue], open[next] = true, true
	in.Events = []Event{{At: in.Now, Verb: VerbDone, Agent: worker, Detail: worker + " done: proven"}}
	if d := only(t, Plan(in), KindDone); d.Event == nil {
		t.Fatalf("done report: %+v", d)
	}
	in.Events = nil

	// A single-pull-request worker serves nothing else: retired as before,
	// a serve record naming its own pull request included.
	for _, serves := range []string{"", pr} {
		in.Workers[0].Serves, in.Workers[0].ParkedOn = serves, pr
		if d := only(t, Plan(in), KindDone); d.Agent != worker {
			t.Fatalf("single pull request worker serving %q: %+v", serves, d)
		}
	}
}

// TestAReviewerIsRetiredWhateverItsWorkerServes: a reviewer the loop
// started for one round goes once its verdict is acted on.
func TestAReviewerIsRetiredWhateverItsWorkerServes(t *testing.T) {
	reviewer := ReviewerName(pr, 1)
	workers := append(parked(t0), Worker{Name: reviewer, Started: true, Serves: "o/r#70", ParkedOn: "o/r#90"})
	workers[0].Serves = "o/r#70"
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	in := Input{Workers: workers, Pulls: pull(head1, true, Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}), Rounds: 2, Taken: tk,
		Open: func(string) bool { return true }}
	f := only(t, Plan(in), KindForward)
	in.Taken = append(in.Taken, taken(t, t1, f)...)
	if d := only(t, Plan(in), KindDone); d.Agent != reviewer {
		t.Errorf("done = %+v", d)
	}
}

func TestPlanIntakesFiledIssuesBeforeStartingNext(t *testing.T) {
	in := Input{
		Board: true,
		Desk:  Desk{Orgs: []string{"personal", "org"}},
		Workers: []Worker{
			{Name: worker, Started: true, Serves: "personal/tasks#1"},
		},
		Events: []Event{
			{At: t1, Verb: VerbDone, Agent: worker, Detail: worker + " done: finished"},
			{At: t1, Verb: VerbProblem, Agent: worker, Detail: worker + ": filed https://github.com/org/engine/issues/42 for the blocker"},
		},
		Filed: []FiledIssue{
			{Ref: "org/engine#43", URL: "https://github.com/org/engine/issues/43", Agent: "guide", From: "personal/tasks#1"},
			{Ref: "stranger/repo#9", URL: "https://github.com/stranger/repo/issues/9", Agent: "guide"},
		},
	}
	acts := Plan(in)
	var kinds, keys, origins []string
	for _, a := range acts {
		if a.Kind == KindIntake {
			kinds = append(kinds, a.Kind)
			keys = append(keys, a.Key)
			origins = append(origins, a.Message)
		}
	}
	if !slices.Equal(keys, []string{"intake org/engine#42", "intake org/engine#43"}) ||
		!slices.Equal(origins, []string{"personal/tasks#1", "personal/tasks#1"}) {
		t.Fatalf("intake actions = %+v", acts)
	}
	// Once done is taken, intake still precedes KindNext in the pass.
	i := slices.IndexFunc(acts, func(a Action) bool { return a.Kind == KindDone })
	if i < 0 {
		t.Fatalf("no done action in %+v", acts)
	}
	in.Taken = taken(t, t1, acts[i])
	acts = Plan(in)
	if len(acts) < 3 || acts[0].Kind != KindIntake || acts[1].Kind != KindIntake || acts[2].Kind != KindNext {
		t.Fatalf("order = %+v, want intake actions before next", acts)
	}
}
