package superloop

import (
	"strings"
	"testing"
	"time"
)

// orphanSince is when the sweep's pull request lost its owner.
var orphanSince = t0.Add(time.Hour)

// sweepInput is a pass over pulls with no worker at all, OrphanWait plus
// wait after the loop first saw pr without an owner.
func sweepInput(pulls map[string]Pull, wait time.Duration) Input {
	return Input{Pulls: pulls, Rounds: 1, Now: orphanSince.Add(wait), Ownerless: map[string]time.Time{pr: orphanSince}}
}

// with is pulls' pr changed by f.
func with(pulls map[string]Pull, f func(*Pull)) map[string]Pull {
	p := pulls[pr]
	f(&p)
	pulls[pr] = p
	return pulls
}

// An open pull request with no saved review park, no review history and
// no worker is ownerless; once OrphanWait has passed the sweep starts its
// review, as the work of the worker its ref names, and not before.
func TestAnOwnerlessGreenPullGetsAReviewerAfterTheWait(t *testing.T) {
	in := sweepInput(pull(head1, true), OrphanWait-time.Minute)
	if got := Ownerless(in); len(got) != 1 || got[0] != pr {
		t.Fatalf("ownerless = %v, want %s", got, pr)
	}
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("before the wait the sweep took %+v", acts)
	}
	in.Now = orphanSince.Add(OrphanWait)
	a := only(t, Plan(in), KindReview)
	if a.Key != reviewKey(pr, head1) || a.Round != 1 || a.Agent != PullWorkerName(pr) || a.Head != head1 {
		t.Errorf("review = %+v", a)
	}
	// Its round is a round like any other: the next pass starts none
	// again, and the verdict goes as the rounds say.
	in.Taken = taken(t, in.Now, a)
	in.Ownerless = nil
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("after the review started the loop took %+v", acts)
	}
	in.Pulls = pull(head1, true, Review{ID: "R1", Head: head1, Verdict: Approved, At: in.Now.Add(time.Minute)})
	if g := only(t, Plan(in), KindGo); g.Head != head1 || g.Agent != PullWorkerName(pr) {
		t.Errorf("go = %+v", g)
	}
}

// Activity on GitHub (a push, a review) restarts the wait: somebody works
// on it.
func TestActivityOnThePullRestartsTheWait(t *testing.T) {
	in := sweepInput(with(pull(head1, true), func(p *Pull) { p.Updated = orphanSince.Add(10 * time.Minute) }), OrphanWait)
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("a pull updated within the wait took %+v", acts)
	}
	in.Now = orphanSince.Add(10*time.Minute + OrphanWait)
	only(t, Plan(in), KindReview)
}

// A live owner keeps the sweep away: a worker parked on it, one serving
// an issue it closes, one named for it, the worker the loop acts for, a
// reviewer of its round. A saved review park, and a worker parked on it
// waiting for another worker's pull request, are none.
func TestALiveOwnerKeepsTheSweepAway(t *testing.T) {
	const issue = "o/r#3"
	pulls := with(pull(head1, true), func(p *Pull) { p.Closes = []string{issue} })
	review := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	for name, tc := range map[string]struct {
		w     Worker
		taken []Action
	}{
		"review park":       {w: Worker{Name: "x", ReviewRequested: true, ParkedOn: pr, ParkedAt: t0}},
		"serves its issue":  {w: Worker{Name: "x", Serves: issue}},
		"parked on issue":   {w: Worker{Name: "x", ParkedOn: issue, ParkedAt: t0}},
		"named for it":      {w: Worker{Name: PullWorkerName(pr)}},
		"its task names it": {w: Worker{Name: "x", Mentions: []string{"o/r#1", pr}}},
		"its worker":        {w: Worker{Name: worker}, taken: []Action{review}},
		"its reviewer":      {w: Worker{Name: ReviewerName(pr, 1)}, taken: []Action{review}},
	} {
		t.Run(name, func(t *testing.T) {
			in := sweepInput(pulls, OrphanWait)
			in.Workers = []Worker{tc.w}
			in.Taken = taken(t, t0, tc.taken...)
			if got := Ownerless(in); len(got) != 0 {
				t.Errorf("ownerless = %v with %+v", got, tc.w)
			}
			// The pass is the one it is without the sweep.
			without := in
			without.Ownerless = nil
			if got, want := Plan(in), Plan(without); len(got) != len(want) {
				t.Errorf("with a live owner the sweep added to %+v: %+v", want, got)
			}
		})
	}
	for name, w := range map[string]Worker{
		"saved review park": {Name: worker, ReviewRequested: true, ParkedOn: pr, ParkedAt: t0, Saved: true, Serves: issue},
		"overlap waiter":    {Name: "other", ParkedOn: pr, ParkedAt: t0, Waits: "overlap with " + pr, Serves: "o/r#9"},
	} {
		t.Run(name, func(t *testing.T) {
			in := sweepInput(pulls, OrphanWait)
			in.Workers = []Worker{w}
			if got := Ownerless(in); len(got) != 1 {
				t.Errorf("ownerless = %v with %+v, want %s", got, w, pr)
			}
		})
	}
}

// A head that conflicts, is red, or is behind a base its merge does not
// update gets a fixer, not a review; behind where the go's merge updates
// the branch, it gets its review. Checks still running wait.
func TestTheSweepGivesAFixerToABrokenHead(t *testing.T) {
	for name, tc := range map[string]struct {
		f    func(*Pull)
		want string
	}{
		"conflicting": {func(p *Pull) { p.Mergeable = mergeableConflicting }, "conflicts with"},
		"dirty":       {func(p *Pull) { p.MergeState = mergeStateDirty; p.Green = false }, "conflicts with"},
		"red":         {func(p *Pull) { p.Green = false; p.Red = []Check{{Name: lintCheck, URL: lintURL}} }, lintCheck + " " + lintURL},
		"behind":      {func(p *Pull) { p.MergeState = "BEHIND" }, "is behind"},
	} {
		t.Run(name, func(t *testing.T) {
			in := sweepInput(with(pull(head1, true), tc.f), OrphanWait)
			a := only(t, Plan(in), KindRework)
			if !IsSweep(a.Key) || a.Agent != PullWorkerName(pr) || a.Head != head1 || !strings.Contains(a.Message, tc.want) || !strings.Contains(a.Message, "agents park --on "+pr) {
				t.Errorf("fix = %+v", a)
			}
		})
	}
	in := sweepInput(with(pull(head1, true), func(p *Pull) { p.MergeState = "BEHIND" }), OrphanWait)
	in.DevctlOwners = []string{"o"}
	if a := only(t, Plan(in), KindReview); a.Key != reviewKey(pr, head1) {
		t.Errorf("behind, gated: %+v", a)
	}
	if acts := Plan(sweepInput(pull(head1, false), OrphanWait)); len(acts) != 0 {
		t.Errorf("running checks took %+v", acts)
	}
	draft := sweepInput(with(pull(head1, true), func(p *Pull) { p.Draft = true }), OrphanWait)
	if got, acts := Ownerless(draft), Plan(draft); len(got) != 0 || len(acts) != 0 {
		t.Errorf("a draft: ownerless %v, took %+v", got, acts)
	}
}

// A review's changes request whose worker ended unfixed gets a fixer told
// the review word for word.
func TestTheSweepGivesUnfixedFindingsAFixer(t *testing.T) {
	rv := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker, Round: 1}
	changes := Review{ID: "R1", URL: prURL + "#pullrequestreview-1", Head: head1, Verdict: ChangesRequested, Body: "fix the nil map", At: t1}
	fwd := Action{Key: forwardKey(pr, "R1"), Kind: KindForward, Ref: pr, Agent: worker}
	in := sweepInput(pull(head1, true, changes), OrphanWait)
	in.Taken = append(taken(t, t0, rv), taken(t, t1, fwd)...)
	a := only(t, Plan(in), KindRework)
	if a.Agent != worker || !IsSweep(a.Key) || !strings.Contains(a.Message, "fix the nil map") || !strings.Contains(a.Message, changes.URL) {
		t.Errorf("fix = %+v", a)
	}
}

// A go given for a head that never merged, its worker gone, is given
// again by the sweep, under its own key.
func TestTheSweepRerunsAGoNeverCarriedOut(t *testing.T) {
	rv := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker, Round: 1}
	approved := Review{ID: "R1", Head: head1, Verdict: Approved, At: t1}
	g := Action{Key: goKey(pr, head1), Kind: KindGo, Ref: pr, Agent: worker, Head: head1}
	in := sweepInput(pull(head1, true, approved), OrphanWait)
	in.Taken = append(taken(t, t0, rv), taken(t, t1, g)...)
	a := only(t, Plan(in), KindGo)
	if !IsSweep(a.Key) || !strings.HasPrefix(a.Key, goKey(pr, head1)+" ") || a.Head != head1 || KeyHead(a.Key) != head1 {
		t.Errorf("go = %+v", a)
	}
	// Read back, it is the go of head1: no review round, no other go.
	in.Taken = append(in.Taken, taken(t, orphanSince, a)...)
	l := newLedger(in.Taken, nil)
	if l.goHead[pr] != head1 || len(l.rounds[pr]) != 1 {
		t.Errorf("ledger: goHead %q, rounds %+v", l.goHead[pr], l.rounds[pr])
	}
}

// Each stretch without an owner gets one action; a head the sweep gave
// MaxSweeps owners goes to the guide, once.
func TestTheSweepStartsOncePerStretchAndGivesUp(t *testing.T) {
	conflicting := func(p *Pull) { p.Mergeable = mergeableConflicting }
	in := sweepInput(with(pull(head1, true), conflicting), OrphanWait)
	first := only(t, Plan(in), KindRework)
	in.Taken = taken(t, in.Now, first)
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("the same stretch took %+v again", acts)
	}
	// Its fixer ended without a push: a new stretch, a new fixer.
	later := in.Now.Add(time.Hour)
	in.Ownerless = map[string]time.Time{pr: later}
	in.Now = later.Add(OrphanWait)
	second := only(t, Plan(in), KindRework)
	if second.Key == first.Key {
		t.Fatalf("second fixer reused %q", first.Key)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, second)...)
	last := in.Now.Add(time.Hour)
	in.Ownerless = map[string]time.Time{pr: last}
	in.Now = last.Add(OrphanWait)
	leave := only(t, Plan(in), KindLeave)
	if !strings.Contains(leave.Key, leftSwept(head1)) || !strings.Contains(leave.Message, "guide") {
		t.Errorf("leave = %+v", leave)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, leave)...)
	if acts := Plan(in); len(acts) != 0 {
		t.Errorf("after giving up the sweep took %+v", acts)
	}
	// A new head is a new start.
	in.Pulls = with(pull(head2, true), conflicting)
	if a := only(t, Plan(in), KindRework); a.Head != head2 {
		t.Errorf("new head: %+v", a)
	}
}

// A review the sweep started that ended without a verdict gets one more
// round; legacy escalation records no longer hold the sweep.
func TestTheSweepReviewsAgainAfterAVerdictlessRound(t *testing.T) {
	rv := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: PullWorkerName(pr), Round: 1}
	in := sweepInput(pull(head1, true), OrphanWait)
	in.Taken = taken(t, t0, rv)
	a := only(t, Plan(in), KindReview)
	if !IsSweep(a.Key) || a.Round != 2 || KeyHead(a.Key) != head1 {
		t.Errorf("second review = %+v", a)
	}
	in.Taken = append(in.Taken, taken(t, t1, a)...)
	in.Now = in.Now.Add(time.Hour)
	in.Ownerless = map[string]time.Time{pr: in.Now.Add(-OrphanWait)}
	a = only(t, Plan(in), KindReview)
	if a.Round != 3 || KeyHead(a.Key) != head1 {
		t.Errorf("third review = %+v", a)
	}
	if _, ok := ParseTaken(t1, "supervisor.escalate", pr, "[escalate "+pr+"@"+head1+"] worker: old question"); ok {
		t.Fatal("legacy escalation was read back")
	}
}
