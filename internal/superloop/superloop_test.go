package superloop

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	overlapIssueA = "o/r#101"
	overlapIssueB = "o/r#102"
	pr            = "o/r#7"
	prURL         = "https://github.com/o/r/pull/7"
	head1         = "1111111111111111111111111111111111111111"
	head2         = "2222222222222222222222222222222222222222"
	head3         = "3333333333333333333333333333333333333333"
	worker        = "worker one"
	release       = "v1.2.0"
	reviewChanges = "fix x"
	// guidePerson is the guide's person.
	guidePerson = "pat"
	// deskAuthor is the login the desk opens its pull requests as.
	deskAuthor = "alice"
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

// Every new head beyond an approval gets its own review.
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
	// A base merge needs no park since the round: the merge's own update of
	// a behind branch, or the worker's, carries no change of the worker's,
	// even when the worker was woken to resolve a conflict and has not
	// parked again.
	in = Input{Workers: parked(t0.Add(-time.Hour)), Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk, BaseMerged: baseMerged(true)}
	if a := only(t, Plan(in), KindGo); a.Head != head2 {
		t.Errorf("go on a base merge after an earlier park = %+v", a)
	}
	in = Input{Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk, BaseMerged: baseMerged(true)}
	if a := only(t, Plan(in), KindGo); a.Head != head2 || a.Agent != worker {
		t.Errorf("go on a base merge without a parked worker = %+v", a)
	}
	in.BaseMerged = baseMerged(false)
	if got := Plan(in); slices.ContainsFunc(got, func(a Action) bool { return a.Kind == KindGo || a.Kind == KindReview }) {
		t.Errorf("a new head without a parked worker got %+v", got)
	}
	in = Input{Workers: parked(t0.Add(-time.Hour)), Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk, BaseMerged: baseMerged(false)}
	if got := Plan(in); slices.ContainsFunc(got, func(a Action) bool { return a.Kind == KindGo || a.Kind == KindReview }) {
		t.Errorf("a new head with an earlier park got %+v", got)
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
	if !strings.Contains(a.Message, "`devctl pr merge o/r 7`") || strings.Contains(a.Message, "--update-branch") || strings.Contains(a.Message, "--match-head-commit") {
		t.Errorf("a devctl repository's go = %q", a.Message)
	}
	in.DevctlOwners = []string{"giantswarm"}
	a = only(t, Plan(in), KindGo)
	if !strings.Contains(a.Message, "`gh pr merge 7 --repo o/r --squash --match-head-commit "+head1+"`") || strings.Contains(a.Message, "devctl") || strings.Contains(a.Message, "--admin") {
		t.Errorf("a gh repository's go = %q", a.Message)
	}
	in.Admin = []string{"O/R"}
	a = only(t, Plan(in), KindGo)
	if !strings.Contains(a.Message, "`gh pr merge 7 --repo o/r --squash --admin --match-head-commit "+head1+"`") || strings.Contains(a.Message, "devctl") {
		t.Errorf("an admin repository's go = %q", a.Message)
	}
	// A repository in merge.admin takes gh pr merge --admin even when its
	// owner is in merge.devctlOwners; another repository never gets --admin.
	in.DevctlOwners = []string{"O"}
	a = only(t, Plan(in), KindGo)
	if !strings.Contains(a.Message, "`gh pr merge 7 --repo o/r --squash --admin --match-head-commit "+head1+"`") || strings.Contains(a.Message, "devctl") {
		t.Errorf("an admin repository under devctlOwners go = %q", a.Message)
	}
	in.Admin = []string{"o/other"}
	a = only(t, Plan(in), KindGo)
	if !strings.Contains(a.Message, "`devctl pr merge o/r 7`") || strings.Contains(a.Message, "--admin") {
		t.Errorf("a non-admin repository under devctlOwners go = %q", a.Message)
	}
}

func TestTheGoUsesConfiguredMergeMethod(t *testing.T) {
	for _, method := range []string{"squash", "rebase", "merge"} {
		t.Run(method, func(t *testing.T) {
			repo, _, _ := strings.Cut(pr, "#")
			in := Input{MergeMethods: map[string]string{repo: method}}
			base := Action{Ref: "o/r#7", Head: head1}
			a := goAction(in, base, "approved")
			if !strings.Contains(a.Message, "--"+method+" --match-head-commit "+head1) {
				t.Fatalf("wrong method: %s", a.Message)
			}
			in.DevctlOwners = []string{"o"}
			a = goAction(in, base, "approved")
			if strings.Contains(a.Message, "--rebase") != (method == "rebase") {
				t.Fatalf("wrong devctl method: %s", a.Message)
			}
		})
	}
}

// An approved round does not use a change-request round; the fixes after
// the final change request get the loop’s go without another review.
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
// them, and its fixes count only after it parked again and a review
// approves the fixed head.
func TestFindingsAreForwardedAfterTheHeadMoved(t *testing.T) {
	changes := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, Body: reviewChanges, At: t1}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	in := Input{Workers: parked(t0), Pulls: pull(head2, true, changes), Rounds: 1, Taken: taken(t, t0, started)}
	if a := only(t, Plan(in), KindForward); !strings.HasSuffix(a.Message, reviewChanges) {
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
	// Told once for that park; still tracked, so its fixes get their review and go.
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
// no go comes; a head with fixes after it gets the next review round.
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
// again, once: a change past MaxReviews gets the loop's go, whatever it
// changes and whatever supervisor.reviewRounds says. No note asks the
// person whether to merge it.
func TestPastTheReviewsTheLoopMergesWithoutAQuestion(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	r2 := Review{ID: "R2", Head: head2, Verdict: Approved, At: t0.Add(2*time.Hour + time.Minute)}
	acts := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker})
	acts = append(acts, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	in := Input{Workers: parked(t0.Add(3 * time.Hour)), Pulls: pull(head3, true, r1, r2), Rounds: 5, Person: guidePerson, Taken: acts}
	got := Plan(in)
	g := only(t, got, KindGo)
	if g.Head != head3 || g.Question != "" || !strings.Contains(g.Message, "2 reviews are used, the last approving head "+short(head2)) {
		t.Errorf("go = %+v", g)
	}
	noQuestion(t, got)
	// Red checks: no go yet, and no note either.
	in.Pulls = pull(head3, false, r1, r2)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a red head past the reviews got %+v", got)
	}
}

// Past MaxReviews a last reviewer that finished without a verdict leaves
// an earlier round's verdict standing: the go, no question. With no
// verdict at all, no go comes and no person is asked: a leave line says
// why.
func TestPastTheReviewsWithoutAVerdictNoPersonIsAsked(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	acts := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	acts = append(acts, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	done := Event{At: t0.Add(3 * time.Hour), Verb: VerbDone, Agent: ReviewerName(pr, 2)}
	in := Input{Workers: parked(t0.Add(4 * time.Hour)), Pulls: pull(head3, true, r1), Rounds: 1, Person: guidePerson, Taken: acts, Events: []Event{done}}
	// The reviewer that reported done is retired besides.
	plan := func() []Action {
		return slices.DeleteFunc(Plan(in), func(a Action) bool { return a.Kind == KindDone })
	}
	got := plan()
	if g := only(t, got, KindGo); g.Head != head3 || !strings.Contains(g.Message, "round 1 approved head "+short(head1)) {
		t.Errorf("go = %+v", g)
	}
	noQuestion(t, got)
	in.Pulls = pull(head3, true)
	got = plan()
	if l := only(t, got, KindLeave); l.Key != leaveKey(pr, leftNoVerdict(head3)) || !strings.Contains(l.Message, "none gave a verdict") {
		t.Errorf("leave = %+v", l)
	}
	noQuestion(t, got)
}

// A verdictless review cannot erase earlier findings on an unchanged head
// or on a head that only updates its base. Real fixes still get the loop's go.
func TestVerdictlessReviewDoesNotMergeUnfixedEarlierFindings(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, Body: "fix the panic", At: t1}
	acts := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	acts = append(acts, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	done := Event{At: t0.Add(3 * time.Hour), Verb: VerbDone, Agent: ReviewerName(pr, 2)}
	for _, tc := range []struct {
		name     string
		head     string
		baseOnly bool
		want     string
	}{
		{"unchanged findings", head1, false, KindRework},
		{"base update only", head3, true, KindRework},
		{"fixed findings", head3, false, KindGo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Workers: parked(t0.Add(4 * time.Hour)), Pulls: pull(tc.head, true, r1), Rounds: 1, Person: guidePerson, Taken: acts, Events: []Event{done},
				BaseMerged: func(Pull, string) bool { return tc.baseOnly }}
			got := slices.DeleteFunc(Plan(in), func(a Action) bool { return a.Kind == KindDone })
			a := only(t, got, tc.want)
			if tc.want == KindRework && !strings.Contains(a.Message, r1.Body) {
				t.Fatalf("rework lost the findings: %+v", a)
			}
			noQuestion(t, got)
		})
	}
}

// noQuestion fails when any of acts would put a question to a person.
func noQuestion(t *testing.T, acts []Action) {
	t.Helper()
	for _, a := range acts {
		if a.Question != "" || a.Kind == KindOwnership {
			t.Errorf("a person was asked: %+v", a)
		}
	}
}

// The fixes of a second review merge without a third review.
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

// A later changes-requested review in the same round overrides an earlier
// approval on that round's head or on a conflict-resolved head.
func TestALaterChangeRequestOverridesAnEarlierApproval(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	r2 := Review{ID: "R2", Head: head2, Verdict: ChangesRequested, Body: "dropped guard in conflict resolution", At: t0.Add(2 * time.Hour)}
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: goKey(pr, head1), Kind: KindGo, Ref: pr, Agent: worker})
	in := Input{
		Workers:    parked(t0.Add(90 * time.Minute)),
		Pulls:      pull(head2, true, r1, r2),
		Rounds:     1,
		Taken:      tk,
		BaseMerged: func(Pull, string) bool { return true },
	}
	f := only(t, Plan(in), KindForward)
	if !strings.Contains(f.Message, "dropped guard in conflict resolution") {
		t.Errorf("forward = %+v", f)
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

// Findings are forwarded once; green fixes get the merge go.
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
	g := only(t, Plan(in), KindGo)
	if g.Head != head2 {
		t.Errorf("go = %+v", g)
	}
	// If the approved fixed head later conflicts with main and the worker
	// rebases or merges main into it, the new head gets its go once green,
	// without a new review or a new park.
	in.Taken = append(in.Taken, taken(t, t0.Add(3*time.Hour), g)...)
	in.Workers = nil
	in.BaseMerged = func(p Pull, from string) bool { return from == head2 && p.Head == head3 }
	in.Pulls = pull(head3, false, changes)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a rebased fixed head with pending checks got %+v", got)
	}
	in.Pulls = pull(head3, true, changes)
	if g2 := only(t, Plan(in), KindGo); g2.Head != head3 || g2.Agent != worker {
		t.Errorf("go on rebased fixed head = %+v", g2)
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

// An escalation an earlier loop logged, and the person's answer to it,
// are read as nothing: the loop's rounds go on, and its rework too.
func TestAnEarlierLoopsEscalationIsReadAsNothing(t *testing.T) {
	rev := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, At: t1}
	tk := taken(t, t0,
		Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker},
		Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker})
	if _, ok := ParseTaken(t0.Add(2*time.Hour), "supervisor.escalate", pr, "[escalate "+pr+"@"+head1+"] "+worker+": to the guide"); ok {
		t.Fatal("an escalation line was read back")
	}
	answers := []Answer{{Ref: pr, Note: 4, Question: "Merge " + prURL + " at head " + short(head1) + " without a third review, or have it reworked?", Text: "Merge", By: guidePerson, At: t0.Add(3 * time.Hour)}}
	in := Input{Workers: parked(t0.Add(time.Hour)), Pulls: pull(head1, true, rev), Rounds: 1, Taken: tk, Answers: answers}
	only(t, Plan(in), KindRework)
	in.Pulls = pull(head2, true, rev)
	in.Workers = parked(t0.Add(4 * time.Hour))
	if g := only(t, Plan(in), KindGo); g.Head != head2 || strings.Contains(g.Message, "note #4") {
		t.Errorf("go = %+v", g)
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
}

func TestAReplacedParkIsNoDecision(t *testing.T) {
	in := Input{Workers: []Worker{{Name: worker, ParkedAt: t1}}, Events: []Event{{At: t0, Verb: VerbPark, Agent: worker, Detail: worker + ": parked: CI pending for " + prURL}}}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("replaced park became a decision: %+v", got)
	}
}

// Findings on a changed head still reach the worker, but an approval may
// cover only the head assigned to its round.
func TestAHeadPushedDuringTheRoundCannotBeApproved(t *testing.T) {
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	tk := taken(t, t0, started)
	approved := Review{ID: "R1", Head: head2, Verdict: Approved, At: t1}
	for _, at := range []time.Time{t0.Add(-time.Minute), t0.Add(30 * time.Second)} {
		in := Input{Workers: parked(at), Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk}
		if a := only(t, Plan(in), KindLeave); !strings.Contains(a.Message, "no merge go") {
			t.Errorf("parked %s: action = %+v", at, a)
		}
	}
	// No verdict yet on either head: the round runs.
	if got := Plan(Input{Workers: parked(t0.Add(30 * time.Second)), Pulls: pull(head2, true), Rounds: 1, Taken: tk}); len(got) != 0 {
		t.Errorf("a running round got %+v", got)
	}
	// Findings on the pushed head go to the worker and count as round 1's.
	changes := Review{ID: "R1", Head: head2, Verdict: ChangesRequested, Body: reviewChanges, At: t1}
	in := Input{Workers: parked(t0.Add(30 * time.Second)), Pulls: pull(head2, true, changes), Rounds: 2, Taken: tk}
	if a := only(t, Plan(in), KindForward); !strings.Contains(a.Message, "changes on head "+head2) || !strings.HasSuffix(a.Message, reviewChanges) {
		t.Errorf("forward = %+v", a)
	}
	in.Taken = append(slices.Clone(tk), taken(t, t0.Add(2*time.Minute), Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker})...)
	in.Workers = parked(t0.Add(time.Hour))
	in.Pulls = pull(head3, true, changes)
	if a := only(t, Plan(in), KindReview); a.Round != 2 || a.Head != head3 {
		t.Errorf("round two = %+v", a)
	}
	// Parked again on the head the findings are about: unfixed.
	in.Pulls, in.Rounds = pull(head2, true, changes), 1
	if a := only(t, Plan(in), KindRework); a.Head != head2 {
		t.Errorf("rework = %+v", a)
	}
}

// A verdict on a head between the round's and the one the worker parks on
// now is no verdict of the round: once the round's reviewer reports done
// without one on the newest head, the newest head gets its round.
func TestAVerdictOnAnOlderPushedHeadIsFollowedByARoundForTheParkedHead(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	approved := Review{ID: "R1", Head: head2, Verdict: Approved, At: t1}
	in := Input{Workers: parked(t0.Add(30 * time.Second)), Pulls: pull(head3, true, approved), Rounds: 1, Taken: tk}
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a running round got %+v", got)
	}
	// The reviewer's done is the loop's to take as well.
	in.Events = []Event{{At: t1.Add(time.Minute), Verb: VerbDone, Agent: ReviewerName(pr, 1)}}
	got := Plan(in)
	i := slices.IndexFunc(got, func(a Action) bool { return a.Kind == KindReview })
	if i < 0 || got[i].Round != 2 || got[i].Head != head3 {
		t.Fatalf("want round 2 on head 3, got %+v", got)
	}
	a := got[i]
	// Round 2 runs on head 3: a late verdict on round 1's head is not its.
	in.Taken = append(slices.Clone(tk), taken(t, t0.Add(2*time.Hour), a)...)
	late := Review{ID: "R2", Head: head1, Verdict: Approved, At: t0.Add(3 * time.Hour)}
	in.Events = nil
	in.Pulls = pull(head3, true, approved, late)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("round 1's head's verdict gave round 2 %+v", got)
	}
	// Round 2's own verdict is the go.
	in.Pulls = pull(head3, true, approved, late, Review{ID: "R3", Head: head3, Verdict: Approved, At: t0.Add(4 * time.Hour)})
	if g := only(t, Plan(in), KindGo); g.Head != head3 {
		t.Errorf("go = %+v", g)
	}
}

// A verdict on a head from before the round (a teammate's late review, a
// stale bot's) is nothing while the round runs: no forward, no cycle, and
// the round's reviewer stays.
func TestAPreRoundHeadsVerdictLeavesTheRoundRunning(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})
	workers := append(parked(t0.Add(-time.Minute)), Worker{Name: ReviewerName(pr, 1), Started: true})
	stale := Review{ID: "S1", Head: head1, Verdict: ChangesRequested, Body: "old", At: t0.Add(5 * time.Minute)}
	in := Input{Workers: workers, Pulls: pull(head2, true, stale), Rounds: 2, Taken: tk}
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a pre-round head's verdict got %+v", got)
	}
}

// A green head past its approval whose park proved another head from before
// the round gets a leave line with the reason, once, never silence.
func TestAnUnreviewedHeadWithAnOldParkIsLeftWithAReason(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	in := Input{Workers: parkedOn(head1, t0.Add(-time.Minute)), Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk}
	a := only(t, Plan(in), KindLeave)
	if a.Key != leaveKey(pr, leftUnreviewed(head2)) || a.Agent != worker || !strings.Contains(a.Message, "park on the pull request again") {
		t.Errorf("leave = %+v", a)
	}
	in.Taken = append(in.Taken, taken(t, t0.Add(time.Hour), a)...)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a second pass took %+v", got)
	}
	// Parked again: the new head's review.
	in.Workers = parked(t0.Add(2 * time.Hour))
	if r := only(t, Plan(in), KindReview); r.Head != head2 {
		t.Errorf("review = %+v", r)
	}
}

// A verified park of the head now stands whatever its age: its worker, stopped
// on it, cannot park again, so the head gets its round.
func TestAnOldParkOfTheCurrentHeadGetsItsReview(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	old := parkedOn(head2, t0.Add(-time.Minute))
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	in := Input{Workers: old, Pulls: pull(head2, true, approved), Rounds: 1, Taken: tk}
	if r := only(t, Plan(in), KindReview); r.Head != head2 || r.Round != 2 {
		t.Errorf("after an approval of the older head: review = %+v", r)
	}
	// Its reviewer done without a verdict: the same head, the same round.
	in.Pulls = pull(head2, true)
	if got := Plan(in); len(got) != 0 {
		t.Errorf("a running round got %+v", got)
	}
	// The loop takes the reviewer's done as well.
	in.Events = []Event{{At: t1, Verb: VerbDone, Agent: ReviewerName(pr, 1)}}
	reviews := func() []Action {
		return slices.DeleteFunc(Plan(in), func(a Action) bool { return a.Kind != KindReview })
	}
	if r := only(t, reviews(), KindReview); r.Head != head2 || r.Round != 2 {
		t.Errorf("after a done without a verdict: review = %+v", r)
	}
	// A red head still waits, and a park of another head is still stale.
	in.Pulls = pull(head2, false)
	if got := reviews(); len(got) != 0 {
		t.Errorf("a head not green got %+v", got)
	}
	in.Pulls = pull(head3, true)
	if got := reviews(); len(got) != 0 {
		t.Errorf("a park of another head got %+v", got)
	}
}

// A reviewer that left the roster without a verdict, and without a done (its
// remote start failed), holds the parked head no longer: OrphanWait after its
// round opened, the same head gets the next round under a key of its own,
// once, and past MaxReviews the leave says no verdict came.
// https://github.com/giantswarm/beekeeper/issues/3757
func TestAGoneReviewersHeadGetsTheNextRound(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker, Head: head1, Round: 1})
	in := Input{Workers: parkedOn(head1, t0.Add(-time.Minute)), Pulls: pull(head1, true), Rounds: 1, Taken: tk, Now: t0.Add(OrphanWait - time.Second)}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("before OrphanWait the loop took %+v", got)
	}
	in.Now = t0.Add(OrphanWait)
	// A reviewer still on the roster runs its round, done or not.
	for _, done := range []bool{false, true} {
		in.Workers = append(parkedOn(head1, t0.Add(-time.Minute)), Worker{Name: ReviewerName(pr, 1), Done: done})
		if got := Plan(in); len(got) != 0 {
			t.Fatalf("a reviewer on the roster (done %v) got %+v", done, got)
		}
	}
	in.Workers = parkedOn(head1, t0.Add(-time.Minute))
	in.Pulls = pull(head1, false)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("a head not green got %+v", got)
	}
	in.Pulls = pull(head1, true)
	r := only(t, Plan(in), KindReview)
	if r.Key != retryKey(pr, head1, 2) || r.Head != head1 || r.Round != 2 || r.Agent != worker || KeyHead(r.Key) != head1 || IsSweep(r.Key) {
		t.Fatalf("next round = %+v", r)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, r)...)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("a second pass took %+v", got)
	}
	// Its reviewer gone without a verdict too: the reviews are used.
	in.Now = in.Now.Add(OrphanWait)
	l := only(t, Plan(in), KindLeave)
	if l.Key != leaveKey(pr, leftNoVerdict(head1)) || !strings.Contains(l.Message, "none gave a verdict") {
		t.Errorf("leave = %+v", l)
	}
	// When round 2 asks for changes on head1, only round 2's cycle counts.
	r2 := Review{ID: "R2", Head: head1, Verdict: ChangesRequested, Body: reviewChanges, At: in.Now}
	in.Pulls = pull(head2, true, r2)
	in.Rounds = 2
	in.Taken = append(in.Taken, taken(t, in.Now, Action{Key: forwardKey(pr, "R2"), Kind: KindForward, Ref: pr, Agent: worker})...)
	in.Workers = parkedOn(head2, in.Now.Add(time.Minute))
	if g := only(t, Plan(in), KindGo); g.Head != head2 || !strings.Contains(g.Message, "1 of 2 change-request round(s) used") {
		t.Errorf("go = %+v", g)
	}
}

// Past MaxReviews a last reviewer gone without a done (its start failed)
// reviewed nothing: round 1's findings give no go on the fixes nobody
// reviewed, only the leave. One that reported done leaves the go as before.
func TestAGoneLastReviewerGivesNoGoOnAnEarlierVerdict(t *testing.T) {
	r1 := Review{ID: "R1", Head: head1, Verdict: ChangesRequested, Body: "fix the panic", At: t1}
	acts := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	acts = append(acts, taken(t, t0.Add(2*time.Hour), Action{Key: reviewKey(pr, head2), Kind: KindReview, Ref: pr, Agent: worker})...)
	in := Input{Workers: parkedOn(head2, t0.Add(time.Hour)), Pulls: pull(head2, true, r1), Rounds: 1, Taken: acts, Now: t0.Add(2*time.Hour + OrphanWait)}
	l := only(t, Plan(in), KindLeave)
	if l.Key != leaveKey(pr, leftNoVerdict(head2)) || !strings.Contains(l.Message, "is gone without a verdict on head "+short(head2)) {
		t.Errorf("leave = %+v", l)
	}
	// The reviewer that reported done is retired besides.
	in.Events = []Event{{At: t0.Add(3 * time.Hour), Verb: VerbDone, Agent: ReviewerName(pr, 2)}}
	got := slices.DeleteFunc(Plan(in), func(a Action) bool { return a.Kind == KindDone })
	if g := only(t, got, KindGo); g.Head != head2 || !strings.Contains(g.Message, "the last reviewer finished without a verdict") {
		t.Errorf("go = %+v", g)
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
	const task1 = "personal/tasks#1"
	in := Input{
		Board: true,
		Desk:  Desk{Orgs: []string{"personal", "org"}},
		Workers: []Worker{
			{Name: worker, Started: true, Serves: task1},
		},
		Events: []Event{
			{At: t1, Verb: VerbDone, Agent: worker, Detail: worker + " done: finished"},
			{At: t1, Verb: VerbProblem, Agent: worker, Detail: worker + ": filed https://github.com/org/engine/issues/42 for the blocker"},
			{At: t1, Verb: VerbReport, Agent: worker, Detail: worker + ": closed https://github.com/personal/tasks/issues/1. Filed follow-up https://github.com/org/engine.io/issues/44 after v0.174.0; Problems found: none"},
		},
		Filed: []FiledIssue{
			{Ref: "org/engine#43", URL: "https://github.com/org/engine/issues/43", Agent: "guide", From: task1},
			{Ref: "stranger/repo#9", URL: "https://github.com/stranger/repo/issues/9", Agent: "guide"},
		},
	}
	acts := Plan(in)
	var keys, origins []string
	for _, a := range acts {
		if a.Kind == KindIntake {
			keys = append(keys, a.Key)
			origins = append(origins, a.Message)
		}
	}
	if !slices.Equal(keys, []string{"intake org/engine#42", "intake org/engine.io#44", "intake org/engine#43"}) ||
		!slices.Equal(origins, []string{task1, task1, task1}) {
		t.Fatalf("intake actions = %+v", acts)
	}
	// Once done is taken, intake still precedes KindNext in the pass.
	i := slices.IndexFunc(acts, func(a Action) bool { return a.Kind == KindDone })
	if i < 0 {
		t.Fatalf("no done action in %+v", acts)
	}
	in.Taken = taken(t, t1, acts[i])
	acts = Plan(in)
	if len(acts) < 4 || acts[0].Kind != KindIntake || acts[1].Kind != KindIntake || acts[2].Kind != KindIntake || acts[3].Kind != KindNext {
		t.Fatalf("order = %+v, want intake actions before next", acts)
	}
}

// The current park wins over two older merged pull requests and their
// ended tracking records, even when both post-merge windows have elapsed.
func TestTwoMergedPullsDoNotRetireTheWorkerOfAThird(t *testing.T) {
	const second, current = "o/r#8", "o/r#9"
	w := parked(t0)
	w[0].Started = true
	w[0].ParkedOn = current
	in := Input{Workers: w, Now: t1.Add(3 * time.Hour), Open: func(ref string) bool { return ref == current }, Pulls: map[string]Pull{
		pr:      {Ref: pr, URL: prURL, State: Merged},
		second:  {Ref: second, URL: "https://github.com/o/r/pull/8", State: Merged},
		current: {Ref: current, URL: "https://github.com/o/r/pull/9", State: Open, Head: head3, Green: true},
	}}
	for _, ref := range []string{pr, second} {
		in.Taken = append(in.Taken, taken(t, t0, Action{Key: goKey(ref, head1), Kind: KindGo, Ref: ref, Agent: worker}, Action{Key: endKey(ref), Kind: KindEnd, Ref: ref})...)
	}
	for _, a := range Plan(in) {
		if a.Kind == KindDone {
			t.Fatalf("retired worker of pending third pull: %+v", a)
		}
	}
	acts := Plan(in)
	i := slices.IndexFunc(acts, func(a Action) bool { return a.Kind == KindReview && a.Ref == current })
	if i < 0 {
		t.Fatalf("pending review lost: %+v", acts)
	}
}

const (
	overlapPRB    = "o/r#8"
	overlapPRBURL = "https://github.com/o/r/pull/8"
	overlapPRA    = "o/r#5"
	overlapWaiter = "worker A"
	overlapOwnerB = "r 8"
)

func TestWorkerAParksOnPRBWhoseOwnerIsGoneStartsOwnerForB(t *testing.T) {
	workers := []Worker{{
		Name:     overlapWaiter,
		Serves:   overlapPRA,
		ParkedOn: overlapPRB,
		ParkedAt: t0,
		Waits:    "waiting on overlapping pull request",
		Started:  true,
	}}
	if got := Tracked(workers, nil, nil, Desk{}); !slices.Equal(got, []string{overlapPRB}) {
		t.Fatalf("Tracked = %v, want [%s]", got, overlapPRB)
	}
	in := Input{
		Workers: workers,
		Now:     t0.Add(time.Minute),
		Pulls: map[string]Pull{
			overlapPRB: {Ref: overlapPRB, URL: overlapPRBURL, State: Open, Head: head1, Green: true},
		},
	}
	act := only(t, Plan(in), KindOwner)
	if act.Ref != overlapPRB || act.Agent != overlapOwnerB || act.Block != overlapPRA || act.Head != head1 {
		t.Fatalf("owner action = %+v, want Ref=%s Agent=%s Block=%s Head=%s", act, overlapPRB, overlapOwnerB, overlapPRA, head1)
	}
	// Once taken, the same head and park do not start a second owner.
	in.Taken = taken(t, t1, act)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("repeated owner action after taken: %+v", got)
	}
}

func TestOverlapWaitEscalatesAfter30MinutesOfNoProgress(t *testing.T) {
	workers := []Worker{
		{
			Name:            overlapOwnerB,
			ParkedOn:        overlapPRB,
			ParkedAt:        t0,
			ReviewRequested: true,
			ParkedHead:      head1,
		},
		{
			Name:     overlapWaiter,
			Serves:   overlapPRA,
			ParkedOn: overlapPRB,
			ParkedAt: t0.Add(time.Minute),
			Waits:    "waiting on stalled overlap",
			Started:  true,
		},
	}
	// PR B is waiting on CI (Green: false, no red checks), so no review starts yet.
	in := Input{
		Workers: workers,
		Now:     t0.Add(15 * time.Minute),
		Pulls: map[string]Pull{
			overlapPRB: {Ref: overlapPRB, URL: overlapPRBURL, State: Open, Head: head1, Green: false},
		},
	}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("before 30m: got %+v, want no action", got)
	}
	// After 30 minutes with no progress on PR B, the wait escalates to the loop,
	// which gives PR B an owner.
	in.Now = t0.Add(time.Minute + OverlapWait)
	act := only(t, Plan(in), KindOwner)
	if act.Ref != overlapPRB || act.Agent != overlapOwnerB || act.Block != overlapPRA || !strings.Contains(act.Message, "no progress") {
		t.Fatalf("after 30m: got %+v", act)
	}
}

func TestMutualOverlappingPullsGivesFirstALiveOwner(t *testing.T) {
	workers := []Worker{
		{
			Name:     overlapWaiter,
			Serves:   overlapPRA,
			ParkedOn: overlapPRB,
			ParkedAt: t0,
			Waits:    "overlap with o/r#8",
			Started:  true,
		},
		{
			Name:     "worker B",
			Serves:   overlapPRB,
			ParkedOn: overlapPRA,
			ParkedAt: t0,
			Waits:    "overlap with o/r#5",
			Started:  true,
		},
	}
	in := Input{
		Workers: workers,
		Now:     t0.Add(time.Minute),
		Pulls: map[string]Pull{
			overlapPRA: {Ref: overlapPRA, URL: "https://github.com/o/r/pull/5", State: Open, Head: head1, Green: true},
			overlapPRB: {Ref: overlapPRB, URL: overlapPRBURL, State: Open, Head: head2, Green: true},
		},
	}
	act := only(t, Plan(in), KindOwner)
	if act.Ref != overlapPRA || act.Block != overlapPRB || act.Agent != overlapWaiter {
		t.Fatalf("overlap action = %+v, want Ref=%s Block=%s Agent=%s", act, overlapPRA, overlapPRB, overlapWaiter)
	}
}

func TestMutualOverlapRequiresObservedOwnedTarget(t *testing.T) {
	const targetRef = "o/a#5"
	for _, tc := range []struct {
		name   string
		change func(*Input, *Pull)
	}{
		{"allowed", func(*Input, *Pull) {}},
		{"unobserved", func(in *Input, _ *Pull) { delete(in.Pulls, targetRef) }},
		{"closed", func(_ *Input, p *Pull) { p.State = Closed }},
		{"merged", func(_ *Input, p *Pull) { p.State = Merged }},
		{"missing head", func(_ *Input, p *Pull) { p.Head = "" }},
		{"other author", func(_ *Input, p *Pull) { p.Author = "other-author" }},
		{"other team", func(_ *Input, p *Pull) { p.Team = teamB }},
		{"unknown teams", func(in *Input, _ *Pull) { in.Desk.TeamsUnknown = true }},
		{"allowed named never", func(_ *Input, p *Pull) { p.Title = "update o/tool" }},
		{"allowed incomplete files", func(_ *Input, p *Pull) { p.ChangedFiles = 1 }},
		{"never repository", func(in *Input, _ *Pull) { in.Desk.Never = append(in.Desk.Never, repoOf(targetRef)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{
				Desk: desk(), Now: t0.Add(time.Minute),
				Workers: []Worker{
					{Name: overlapWaiter, Serves: targetRef, ParkedOn: overlapPRB, ParkedAt: t0},
					{Name: "worker B", Serves: overlapPRB, ParkedOn: targetRef, ParkedAt: t0},
				},
				Pulls: map[string]Pull{
					targetRef:  {Ref: targetRef, URL: "target-url", State: Open, Head: head1, Author: me, Team: ourTeam},
					overlapPRB: {Ref: overlapPRB, URL: "visited-url", State: Open, Head: head2, Author: me, Team: ourTeam},
				},
			}
			// Keep the visited PR allowed even when team discovery is unavailable.
			in.Desk.Review = []string{"o/r"}
			p := in.Pulls[targetRef]
			tc.change(&in, &p)
			if _, observed := in.Pulls[targetRef]; observed {
				in.Pulls[targetRef] = p
			}
			var acts []Action
			planOverlap(in, ledger{}, overlapPRB, in.Pulls[overlapPRB], nil, func(string) bool { return false }, func(a Action) { acts = append(acts, a) })
			if !strings.HasPrefix(tc.name, "allowed") {
				if len(acts) != 0 {
					t.Fatalf("unsafe handoff: %+v", acts)
				}
				return
			}
			a := only(t, acts, KindOwner)
			if a.Ref != targetRef || a.Head != head1 || a.URL != p.URL || a.Block != overlapPRB || a.Agent != overlapWaiter {
				t.Fatalf("handoff must use target identity: %+v", a)
			}
		})
	}
}

func TestOverlapClosingIssuesMutual(t *testing.T) {
	in := Input{
		Now: t0.Add(time.Minute),
		Workers: []Worker{
			{Name: "issue worker A", Serves: overlapIssueA, ParkedOn: overlapPRB, ParkedAt: t0, Started: true},
			{Name: "issue worker B", Serves: overlapIssueB, ParkedOn: overlapPRA, ParkedAt: t0, Started: true},
		},
		Pulls: map[string]Pull{
			overlapPRA: {Ref: overlapPRA, URL: "https://github.com/o/r/pull/5", Closes: []string{overlapIssueA}, State: Open, Head: head1},
			overlapPRB: {Ref: overlapPRB, URL: overlapPRBURL, Closes: []string{overlapIssueB}, State: Open, Head: head2},
		},
	}
	act := only(t, Plan(in), KindOwner)
	if act.Ref != overlapPRA || act.Block != overlapPRB || act.Agent != "issue worker A" || act.Head != head1 {
		t.Fatalf("mutual issue workers: %+v", act)
	}
	if act.OwnerIssue != overlapIssueA || act.BlockIssue != overlapIssueB {
		t.Fatalf("dependency must use closing issues: %+v", act)
	}
	// Keep the mutual wait explicit while changing closing-issue metadata.
	in.Workers[0].Serves, in.Workers[1].Serves = overlapPRA, overlapPRB
	for _, closes := range [][]string{nil, {overlapIssueA}} {
		p := in.Pulls[overlapPRB]
		p.Closes = closes
		in.Pulls[overlapPRB] = p
		a := only(t, Plan(in), KindOwner)
		if a.OwnerIssue != "" || a.BlockIssue != "" {
			t.Fatalf("issue-less or self dependency must be skipped: %+v", a)
		}
		if a.Ref != act.Ref || a.Agent != act.Agent {
			t.Fatalf("skipping dependency changed ownership: %+v", a)
		}
	}
	p := in.Pulls[overlapPRB]
	p.Closes = []string{overlapIssueB}
	in.Pulls[overlapPRB] = p
	in.Workers[0].Serves, in.Workers[1].Serves = overlapIssueA, overlapIssueB

	// An active issue worker is the live PR owner, even with a different name.
	in.Workers[0].ParkedOn = ""
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("active closing-issue owner missed: %+v", got)
	}
	// Once that owner leaves, the ordinary missing-owner acceptance still holds.
	in.Workers = in.Workers[1:]
	act = only(t, Plan(in), KindOwner)
	if act.Ref != overlapPRA || act.Agent != PullWorkerName(overlapPRA) {
		t.Fatalf("missing owner: %+v", act)
	}
}

// Regression test for after-the-fact review of https://github.com/giantswarm/beekeeper/pull/2164:
// planOverlap matches pull request refs and default pull worker names
// case-insensitively.
func TestPlanOverlapMatchesRefsAndDefaultOwnerCaseInsensitively(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
	const (
		prRef  = "GiantSwarm/Beekeeper#42"
		prLow  = "giantswarm/beekeeper#42"
		waitPR = "GiantSwarm/Beekeeper#43"
	)
	in := Input{
		Now:  t0.Add(OverlapWait + time.Minute),
		Desk: Desk{Authors: []string{deskAuthor}},
		Workers: []Worker{
			{Name: "beekeeper 42", Serves: prLow, ParkedOn: "giantswarm/beekeeper#43", ParkedAt: t0, Waits: "waiting on #43"},
			{Name: "beekeeper 43", Serves: "giantswarm/beekeeper#43", ParkedOn: prLow, ParkedAt: t0, Waits: "waiting on #42"},
		},
		Pulls: map[string]Pull{
			prRef:  {Ref: prRef, URL: "https://github.com/GiantSwarm/Beekeeper/pull/42", Author: deskAuthor, State: Open, Head: head1},
			waitPR: {Ref: waitPR, URL: "https://github.com/GiantSwarm/Beekeeper/pull/43", Author: deskAuthor, State: Open, Head: head2},
		},
	}
	acts := Plan(in)
	var ownerAct *Action
	for i := range acts {
		if acts[i].Kind == KindOwner {
			ownerAct = &acts[i]
			break
		}
	}
	if ownerAct == nil {
		t.Fatalf("expected KindOwner action, got %+v", acts)
	}
	// The mutual wait orders #42 first: it gets the owner, #43 is blocked
	// by it, whatever the case of the refs the workers serve and park on.
	if ownerAct.Ref != prRef || ownerAct.Head != head1 || ownerAct.Block != waitPR {
		t.Fatalf("unexpected KindOwner action: %+v", *ownerAct)
	}
}

func TestParseAbsorbedPullsAndPlanWithdrawsRescueWorkers(t *testing.T) {
	const (
		origA    = "o/r#401"
		origB    = "o/r#402"
		combined = "o/r#400"
	)
	got := ParseAbsorbedPulls(
		combined,
		"fix: combine #401 and #402",
		"Supersedes https://github.com/o/r/pull/401 and o/r#402.\nCloses https://github.com/o/r/issues/50\nFixes #51\n",
	)
	if want := []string{origA, origB}; !slices.Equal(got, want) {
		t.Fatalf("ParseAbsorbedPulls = %v, want %v", got, want)
	}
	for _, tc := range []struct {
		name  string
		title string
		body  string
		want  []string
	}{
		{
			name:  "generic batch keyword in title does not absorb related PR",
			title: "fix: batch scheduler",
			body:  "Related: https://github.com/o/r/pull/401\nSee also o/r#402",
			want:  nil,
		},
		{
			name:  "combine keyword in title without PRs does not leak to body",
			title: "fix: combine metrics",
			body:  "Related: https://github.com/o/r/pull/401\nReplaces old handler from https://github.com/o/r/pull/402",
			want:  nil,
		},
		{
			name:  "combine clause stops before related clause on same line",
			title: "fix: cleanup",
			body:  "Combines https://github.com/o/r/pull/401 and #402. Related: https://github.com/o/r/pull/403",
			want:  []string{origA, origB},
		},
		{
			name:  "explicit bullet list under Combines heading",
			title: "fix: rollup",
			body:  "Combines:\n- https://github.com/o/r/pull/401\n- o/r#402\n\nRelated:\n- https://github.com/o/r/pull/403",
			want:  []string{origA, origB},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if actual := ParseAbsorbedPulls(combined, tc.title, tc.body); !slices.Equal(actual, tc.want) {
				t.Fatalf("ParseAbsorbedPulls(%q, %q) = %v, want %v", tc.title, tc.body, actual, tc.want)
			}
		})
	}

	t0 := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	in := Input{
		Now:  t0.Add(OrphanWait + time.Minute),
		Desk: Desk{Authors: []string{deskAuthor}},
		Workers: []Worker{
			{Name: PullWorkerName(origA), Started: true},
			{Name: ReviewerName(origB, 1), Started: true},
			{Name: "saved fixer", ParkedOn: origB, ParkedAt: t0, ParkedHead: head1, ReviewRequested: true, Saved: true},
		},
		Taken: []Taken{
			{Key: "review " + origB + "@" + head1, Kind: KindReview, Ref: origB, Agent: PullWorkerName(origB), At: t0},
		},
		Ownerless: map[string]time.Time{
			origA: t0,
			origB: t0,
		},
		Pulls: map[string]Pull{
			origA:    {Ref: origA, URL: "https://github.com/o/r/pull/401", Author: deskAuthor, State: Open, Head: head1, Red: []Check{{Name: "test"}}},
			origB:    {Ref: origB, URL: "https://github.com/o/r/pull/402", Author: deskAuthor, State: Open, Head: head1, Green: true},
			combined: {Ref: combined, URL: "https://github.com/o/r/pull/400", Author: deskAuthor, State: Merged, Head: head2, Absorbs: []string{origA, origB}},
		},
	}
	if gotOwnerless := Ownerless(in); len(gotOwnerless) != 0 {
		t.Fatalf("Ownerless kept absorbed PRs: %v", gotOwnerless)
	}
	acts := Plan(in)
	var doneAgents []string
	var endedRefs []string
	for _, a := range acts {
		switch a.Kind {
		case KindDone:
			doneAgents = append(doneAgents, a.Agent)
		case KindEnd:
			endedRefs = append(endedRefs, a.Ref)
		case KindRework, KindReview, KindForward, KindRed, KindOwner:
			t.Fatalf("planned %s for absorbed PR: %+v", a.Kind, a)
		}
	}
	slices.Sort(doneAgents)
	if want := []string{ReviewerName(origB, 1), PullWorkerName(origA)}; !slices.Equal(doneAgents, want) {
		t.Fatalf("retired agents = %v, want %v (actions: %+v)", doneAgents, want, acts)
	}
	if !slices.Equal(endedRefs, []string{origB}) {
		t.Fatalf("ended refs = %v, want [%s]", endedRefs, origB)
	}
}

func TestParseClosingIssuesAndAppendClosingKeywords(t *testing.T) {
	const selfRef = "o/r#400"
	body := "Combines https://github.com/o/r/pull/401 and https://github.com/o/r/pull/402.\n" +
		"Closes https://github.com/o/r/issues/50, /51 and #52\n" +
		"Fixes:\n- https://github.com/o/r/issues/53\n- o/r#54\n\n" +
		"Related: https://github.com/o/r/issues/99\n" +
		"This does not fix https://github.com/o/r/issues/95\n" +
		"- Does not close https://github.com/o/r/issues/96\n" +
		"Do not resolve #97\n" +
		"Partially fixes https://github.com/o/r/issues/98\n" +
		"Use `Closes https://github.com/o/r/issues/90` in the footer.\n" +
		"Closes `https://github.com/o/r/issues/91` when ready.\n" +
		"> Closes https://github.com/o/r/issues/92\n" +
		"    Closes https://github.com/o/r/issues/93\n" +
		"```markdown\nCloses https://github.com/o/r/issues/94\nFixes #89\n```\n" +
		"~~~\nResolves https://github.com/o/r/issues/88\n~~~\n"
	got := ParseClosingIssues(selfRef, body)
	want := []string{"o/r#50", "o/r#51", "o/r#52", "o/r#53", "o/r#54"}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseClosingIssues = %v, want %v", got, want)
	}
	appended := AppendClosingKeywords(selfRef, "Combines https://github.com/o/r/pull/401.\nThis does not fix https://github.com/o/r/issues/51.\n```markdown\nCloses https://github.com/o/r/issues/52\n```\n\nCloses https://github.com/o/r/issues/50", []string{"o/r#50", "o/r#51", "https://github.com/o/r/issues/52"})
	for _, wantLine := range []string{
		"Closes https://github.com/o/r/issues/50",
		"Closes https://github.com/o/r/issues/51",
		"Closes https://github.com/o/r/issues/52",
	} {
		if !strings.Contains(appended, wantLine) {
			t.Fatalf("AppendClosingKeywords missing %q in:\n%s", wantLine, appended)
		}
	}
	if strings.Count(appended, "Closes https://github.com/o/r/issues/50") != 1 {
		t.Fatalf("AppendClosingKeywords duplicated existing issue:\n%s", appended)
	}
}

func TestReview3325MarkdownSafety(t *testing.T) {
	const selfRef = "o/r#400"
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "multiline inline code span closing",
			body: "Example:\n`code start\nCloses https://github.com/o/r/issues/55\nend`",
		},
		{
			name: "lazy blockquote continuation closing",
			body: "> Quoted comment\nCloses https://github.com/o/r/issues/55",
		},
		{
			name: "lazy blockquote list closing",
			body: "> Closes:\n- https://github.com/o/r/issues/55",
		},
		{
			name: "parenthesized negation closing",
			body: "Does not (fix https://github.com/o/r/issues/55).",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseClosingIssues(selfRef, tc.body); len(got) != 0 {
				t.Fatalf("ParseClosingIssues(%q) = %v, want empty", tc.body, got)
			}
			appended := AppendClosingKeywords(selfRef, tc.body, []string{"o/r#55"})
			if !strings.Contains(appended, "\n\nCloses https://github.com/o/r/issues/55") {
				t.Fatalf("AppendClosingKeywords failed to append missing closing line to:\n%s", appended)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		title string
		body  string
	}{
		{
			name: "multiline inline code span absorb",
			body: "Example:\n`code start\nCombines https://github.com/o/r/pull/55\nend`",
		},
		{
			name: "lazy blockquote continuation absorb",
			body: "> Quoted comment\nCombines https://github.com/o/r/pull/55",
		},
		{
			name: "lazy blockquote list absorb",
			body: "> Combines:\n- https://github.com/o/r/pull/55",
		},
		{
			name: "parenthesized negation absorb",
			body: "Do not (combine https://github.com/o/r/pull/55).",
		},
		{
			name:  "parenthesized negation absorb in title",
			title: "fix: do not (supersede #55)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseAbsorbedPulls(selfRef, tc.title, tc.body); len(got) != 0 {
				t.Fatalf("ParseAbsorbedPulls(%q, %q) = %v, want empty", tc.title, tc.body, got)
			}
		})
	}
}
