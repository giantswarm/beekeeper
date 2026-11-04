package superloop

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

const reopenClosed = "closed"

// A closed PR can reopen under the same ref and request review again.
func TestReopenedPullWithFreshRequestIsTracked(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t1, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr, Message: reopenClosed})...)
	w := parkedOn(head2, t0.Add(time.Hour))
	if got := Tracked(w, tk, nil, Desk{}); !slices.Equal(got, []string{pr}) {
		t.Errorf("fresh request after close was lost: %v", got)
	}
}

func TestReopenedPullCanBeSwept(t *testing.T) {
	in := sweepInput(pull(head2, true), OrphanWait)
	in.Taken = taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	in.Taken = append(in.Taken, taken(t, t1, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr, Message: reopenClosed})...)
	if got := Ownerless(in); !slices.Equal(got, []string{pr}) {
		t.Errorf("reopened PR is excluded from recovery: %v", got)
	}
	if acts := Plan(in); !slices.ContainsFunc(acts, func(a Action) bool { return a.Ref == pr && a.Kind == KindReview && a.Head == head2 }) {
		t.Errorf("reopened green PR never resumes review: %+v", acts)
	}
}

func TestReopenedPullRepeatsSameHeadAcrossRestarts(t *testing.T) {
	in := Input{Taken: taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})}
	closed := Action{Key: endKey(pr), Kind: KindEnd, Ref: pr}
	in.Taken = append(in.Taken, taken(t, t1, closed)...)
	lastReview := reviewKey(pr, head1)
	for cycle := 1; cycle <= 2; cycle++ {
		now := t1.Add(time.Duration(cycle) * time.Hour)
		in.Workers, in.Pulls, in.Now = parkedOn(head1, now), pull(head1, true), now
		review := only(t, Plan(in), KindReview)
		if review.Key == lastReview || review.Round != 1 || KeyHead(review.Key) != head1 {
			t.Fatalf("cycle %d reused the closed episode: %+v", cycle, review)
		}
		lastReview = review.Key
		in.Taken = append(in.Taken, taken(t, now, review)...)
		if got := Plan(in); len(got) != 0 {
			t.Fatalf("cycle %d repeated after replay: %+v", cycle, got)
		}
		in.Pulls[pr] = Pull{Ref: pr, URL: prURL, State: Closed, Head: head1}
		end := only(t, Plan(in), KindEnd)
		if end.Key == closed.Key {
			t.Fatalf("cycle %d reused its old end: %+v", cycle, end)
		}
		closed = end
		in.Taken = append(in.Taken, taken(t, now.Add(time.Minute), end)...)
		if got := Tracked(in.Workers, in.Taken, nil, Desk{}); len(got) != 0 {
			t.Fatalf("closed episode still tracked: %v", got)
		}
		if got := Ownerless(in); len(got) != 0 {
			t.Fatalf("closed episode swept: %v", got)
		}
	}
	in.Workers = nil
	in.Pulls[pr] = Pull{Ref: pr, URL: prURL, State: Merged, Head: head1}
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("merged PR resumed: %+v", got)
	}
}

func TestReopeningPreservesUnrelatedEventKeys(t *testing.T) {
	e := Event{At: t0, Verb: VerbDone, Agent: "worker reopened parser"}
	a := Action{Key: eventKey(KindDone, e), Kind: KindDone, Agent: e.Agent}
	tk := taken(t, t0, a)
	tk = append(tk, taken(t, t1, Action{Key: reviewKey(pr, head1) + episodeTag + t0.Format(time.RFC3339Nano), Kind: KindReview, Ref: pr, Agent: worker})...)
	if !newLedger(tk, nil).has(a.Key) {
		t.Fatal("reopening a PR lost an unrelated worker's completed action")
	}
}

func TestReopenedPullForwardedFindingIsNotFiled(t *testing.T) {
	tk := taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker})
	tk = append(tk, taken(t, t1, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr})...)
	now := t1.Add(time.Hour)
	in := Input{Workers: parkedOn(head1, now), Pulls: pull(head1, true), Taken: tk}
	review := only(t, Plan(in), KindReview)
	in.Taken = append(in.Taken, taken(t, now, review)...)
	at := now.Add(time.Minute)
	body := "The parser's nil check is missing in Parse"
	in.Pulls = pull(head1, true, Review{ID: "reopened-r1", Head: head1, Verdict: ChangesRequested, Body: body, At: at})
	reviewer := ReviewerName(pr, 1)
	in.Events = []Event{{At: at, Verb: VerbProblem, Agent: reviewer, Detail: reviewer + ": " + body}}
	forward := only(t, Plan(in), KindForward)
	in.Taken = append(in.Taken, taken(t, at, forward)...)
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("forwarded finding repeated after replay: %+v", acts)
	}
}

func TestReopenedTextInCheckNamePreservesReplay(t *testing.T) {
	for _, reopened := range []bool{false, true} {
		t.Run(fmt.Sprint(reopened), func(t *testing.T) {
			in := Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, Check{Name: "integration reopened service", URL: lintURL})}
			if reopened {
				in.Taken = taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr})
			}
			wake := only(t, Plan(in), KindRed)
			in.Taken = append(in.Taken, taken(t, t1, wake)...)
			if acts := Plan(in); len(acts) != 0 {
				t.Fatalf("check-name text corrupted replay: %+v", acts)
			}
		})
	}
}

func TestReopenedPullResetsRedWake(t *testing.T) {
	in := Input{Workers: parkedOn(head1, t0), Pulls: redPull(head1, Check{Name: lintCheck, URL: lintURL})}
	wake := only(t, Plan(in), KindRed)
	in.Taken = taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}, wake)
	in.Taken = append(in.Taken, taken(t, t1, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr})...)
	resumed := only(t, Plan(in), KindRed)
	if resumed.Key == wake.Key {
		t.Fatal("reopened red wake reused the closed episode")
	}
	in.Taken = append(in.Taken, taken(t, t1.Add(time.Hour), resumed)...)
	if acts := Plan(in); len(acts) != 0 {
		t.Fatalf("reopened red wake repeated after replay: %+v", acts)
	}
}
