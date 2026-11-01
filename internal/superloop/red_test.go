package superloop

import (
	"strings"
	"testing"
	"time"
)

const (
	lintCheck = "pre-commit"
	lintURL   = "https://github.com/o/r/actions/runs/1/job/11"
	buildURL  = "https://github.com/o/r/actions/runs/1/job/12"
)

// parkedOn is a worker whose review request proved head, parked at at.
func parkedOn(head string, at time.Time) []Worker {
	w := parked(at)
	w[0].ParkedHead = head
	return w
}

// redPull is pr at head with the red checks red, none of them green.
func redPull(head string, red ...Check) map[string]Pull {
	p := pull(head, false)
	pp := p[pr]
	pp.Red = red
	p[pr] = pp
	return p
}

// A worker parked on a head whose checks run is woken once a check on it
// goes red, with the check's name and URL, and gets no review.
func TestARedCheckWakesTheParkedWorker(t *testing.T) {
	in := Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1), Rounds: 1}
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("pending checks took %+v", acts)
	}
	in.Pulls = redPull(head1, Check{Name: lintCheck, URL: lintURL})
	a := only(t, Plan(in), KindRed)
	if a.Agent != worker || a.Ref != pr || a.Head != head1 {
		t.Errorf("red = %+v", a)
	}
	for _, want := range []string{prURL, lintCheck + " " + lintURL, short(head1), "agents park --on " + pr} {
		if !strings.Contains(a.Message, want) {
			t.Errorf("message %q lacks %q", a.Message, want)
		}
	}
}

// The same failure seen by a later pass, or by a restarted loop, wakes
// nobody again; a second check going red on the same head wakes it once
// more, for that check alone.
func TestARedCheckWakesOncePerHeadAndCheck(t *testing.T) {
	lint := Check{Name: lintCheck, URL: lintURL}
	in := Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, lint), Rounds: 1}
	first := only(t, Plan(in), KindRed)
	in.Taken = taken(t, t1, first)
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("the same red check took %+v again", acts)
	}
	in.Pulls = redPull(head1, lint, Check{Name: "go-build", URL: buildURL})
	second := only(t, Plan(in), KindRed)
	if !strings.Contains(second.Message, "go-build "+buildURL) || strings.Contains(second.Message, lintCheck) {
		t.Errorf("second wake = %q, want go-build alone", second.Message)
	}
	in.Taken = append(in.Taken, taken(t, t1, second)...)
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("both told, still took %+v", acts)
	}
	// Two checks red in one pass are one wake, and a duplicate check row
	// is one check.
	in = Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, lint, lint, Check{Name: "go-build", URL: buildURL}), Rounds: 1}
	a := only(t, Plan(in), KindRed)
	if strings.Count(a.Message, lintCheck) != 1 || !strings.Contains(a.Message, "go-build") {
		t.Errorf("one wake for two checks = %q", a.Message)
	}
}

// A new head pushed and parked on is woken again for the same check name;
// a head nobody parked on, or one the worker moved past, wakes nobody.
func TestARePushedHeadWakesAgain(t *testing.T) {
	lint := Check{Name: lintCheck, URL: lintURL}
	first := only(t, Plan(Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, lint), Rounds: 1}), KindRed)
	done := taken(t, t1, first)
	// Pushed, not parked again yet: the park names the old head.
	if acts := Plan(Input{Workers: parkedOn(head1, t0), Pulls: redPull(head2, lint), Rounds: 1, Taken: done}); len(acts) != 0 {
		t.Errorf("a head the worker did not park on took %+v", acts)
	}
	later := t0.Add(time.Hour)
	a := only(t, Plan(Input{Workers: parkedOn(head2, later), Pulls: redPull(head2, lint), Rounds: 1, Taken: done}), KindRed)
	if a.Head != head2 || !strings.Contains(a.Message, short(head2)) {
		t.Errorf("red on the new head = %+v", a)
	}
	// Parked again on the same head after a rerun that failed again: the
	// new park is told once.
	again := only(t, Plan(Input{Workers: parkedOn(head1, later), Pulls: redPull(head1, lint), Rounds: 1, Taken: done}), KindRed)
	if again.Key == first.Key {
		t.Errorf("the new park reused the key %q", again.Key)
	}
}

// Red checks wake only a review request on its head: a plain wait, an
// unparked worker, a closed pull request and a head given the go wake
// nobody.
func TestARedCheckWakesOnlyAParkedHeadsWorker(t *testing.T) {
	lint := Check{Name: lintCheck, URL: lintURL}
	plain := []Worker{{Name: worker, ParkedOn: pr, ParkedAt: t0}}
	if acts := Plan(Input{Workers: plain, Pulls: redPull(head1, lint), Rounds: 1}); len(acts) != 0 {
		t.Errorf("a plain wait took %+v", acts)
	}
	started := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	if acts := Plan(Input{Pulls: redPull(head1, lint), Rounds: 1, Taken: taken(t, t0, started)}); len(acts) != 0 {
		t.Errorf("a worker that is not parked took %+v", acts)
	}
	closed := redPull(head1, lint)
	cp := closed[pr]
	cp.State = Closed
	closed[pr] = cp
	if acts := Plan(Input{Workers: parkedOn(head1, t0), Pulls: closed, Rounds: 1}); len(acts) != 0 {
		t.Errorf("a closed pull request took %+v", acts)
	}
	gone := Action{Key: goKey(pr, head1), Kind: KindGo, Ref: pr, Agent: worker}
	if acts := Plan(Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, lint), Rounds: 1, Taken: taken(t, t0, started, gone)}); len(acts) != 0 {
		t.Errorf("a head given the go took %+v", acts)
	}
}

// The green path is the one it was: a parked head gets its review once
// green, and a red wake before it changes nothing of the rounds.
func TestTheGreenPathStaysAfterARedWake(t *testing.T) {
	lint := Check{Name: lintCheck, URL: lintURL}
	red := only(t, Plan(Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, lint), Rounds: 1}), KindRed)
	later := t0.Add(time.Hour)
	in := Input{Workers: parkedOn(head2, later), Pulls: pull(head2, true), Rounds: 1, Taken: taken(t, t1, red)}
	rv := only(t, Plan(in), KindReview)
	if rv.Head != head2 || rv.Round != 1 {
		t.Errorf("review = %+v, want round 1 of head2", rv)
	}
	in.Taken = append(in.Taken, taken(t, later, rv)...)
	in.Pulls = pull(head2, true, Review{ID: "R1", Head: head2, Verdict: Approved, At: later.Add(time.Minute)})
	if g := only(t, Plan(in), KindGo); g.Head != head2 {
		t.Errorf("go = %+v", g)
	}
}

// A red wake is no act on the pull request: a running round's reviewer
// stays.
func TestARedWakeRetiresNoReviewer(t *testing.T) {
	rv := Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}
	red := Action{Key: redKey(pr, head1, t0, lintCheck), Kind: KindRed, Ref: pr, Agent: worker}
	in := Input{Workers: append(parkedOn(head1, t0), Worker{Name: ReviewerName(pr, 1), Started: true}), Pulls: redPull(head1, Check{Name: lintCheck}), Rounds: 1,
		Taken: append(taken(t, t0, rv), taken(t, t1, red)...)}
	if acts := Plan(in); len(acts) != 0 {
		t.Errorf("after a red wake the loop took %+v", acts)
	}
}

// A check's name that would end a key reads back as the same check.
func TestARedKeyReadsBack(t *testing.T) {
	odd := Check{Name: "ci [lint]; go\nvet", URL: lintURL}
	in := Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, odd), Rounds: 1}
	a := only(t, Plan(in), KindRed)
	in.Taken = taken(t, t1, a)
	if acts := Plan(in); len(acts) != 0 {
		t.Errorf("an odd check name woke again: %+v", acts)
	}
}
