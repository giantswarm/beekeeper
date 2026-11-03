package superloop

import "testing"

// A completed worker must not take capacity ahead of a parked green PR.
func TestReviewsPrecedeNextStartsInOnePass(t *testing.T) {
	done := Event{At: t0, Verb: VerbDone, Agent: "finished", Detail: "finished done: shipped"}
	removed := Action{Key: eventKey(KindDone, done), Kind: KindDone, Agent: done.Agent, Event: &done}
	in := Input{Workers: parked(t0), Pulls: pull(head1, true), Events: []Event{done}, Taken: taken(t, t0, removed)}
	acts := Plan(in)
	review, next := -1, -1
	for i, a := range acts {
		if a.Kind == KindReview {
			review = i
		}
		if a.Kind == KindNext {
			next = i
		}
	}
	if review < 0 || next < 0 || review >= next {
		t.Fatalf("review must precede next in one pass: %+v", acts)
	}
}
