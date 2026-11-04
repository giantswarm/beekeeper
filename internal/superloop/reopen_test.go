package superloop

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestReopenedPullEpisodesReplaySameHead(t *testing.T) {
	in := Input{Workers: parked(t0), Pulls: pull(head1, true), Rounds: 1}
	in.Workers[0].ParkedHead = head1
	seen := map[string]bool{}
	record := func(kind string, at time.Time) Action {
		t.Helper()
		a := only(t, Plan(in), kind)
		if seen[a.Key] {
			t.Fatalf("reused durable key %q", a.Key)
		}
		seen[a.Key] = true
		in.Taken = append(in.Taken, taken(t, at, a)...)
		return a
	}
	record(KindReview, t0)
	for cycle := 1; cycle <= 2; cycle++ {
		at := t0.Add(time.Duration(cycle) * time.Hour)
		p := in.Pulls[pr]
		p.State = Closed
		in.Pulls[pr] = p
		record(KindEnd, at)
		if got := Tracked(in.Workers, in.Taken, nil, Desk{}); len(got) != 0 {
			t.Fatalf("old park tracked closed pull: %v", got)
		}
		in.Workers[0].ParkedAt = at.Add(time.Minute)
		if got := Tracked(in.Workers, in.Taken, nil, Desk{}); !slices.Equal(got, []string{pr}) {
			t.Fatalf("fresh request not tracked: %v", got)
		}
		// A fresh request asks for an observation; CLOSED must stay ended.
		if got := Plan(in); len(got) != 0 {
			t.Fatalf("closed pull restarted: %+v", got)
		}
		p.State = Open
		in.Pulls[pr] = p
		record(KindReopen, at.Add(2*time.Minute))
		a := record(KindReview, at.Add(3*time.Minute))
		if a.Round != 1 || KeyHead(a.Key) != head1 {
			t.Fatalf("new episode review: %+v", a)
		}
		if got := Plan(in); len(got) != 0 {
			t.Fatalf("replayed review: %+v", got)
		}
		p = in.Pulls[pr]
		p.Reviews = []Review{{ID: fmt.Sprintf("approval-%d", cycle), Head: head1, Verdict: Approved, At: at.Add(4 * time.Minute)}}
		in.Pulls[pr] = p
		record(KindGo, at.Add(5*time.Minute))
		if got := Plan(in); len(got) != 0 {
			t.Fatalf("replayed merge go: %+v", got)
		}
	}
	// Merging the latest episode remains terminal even with another fresh park.
	p := in.Pulls[pr]
	p.State = Merged
	in.Pulls[pr] = p
	record(KindEnd, t0.Add(3*time.Hour))
	in.Workers[0].ParkedAt = t0.Add(4 * time.Hour)
	if got := Tracked(in.Workers, in.Taken, nil, Desk{}); len(got) != 0 {
		t.Fatalf("merged pull tracked: %v", got)
	}
	p.State = Open
	in.Pulls[pr] = p
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("merged pull reopened: %+v", got)
	}
}

func TestReopenedPullOwnerlessRecovery(t *testing.T) {
	in := sweepInput(pull(head1, true), OrphanWait)
	in.Taken = taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr}, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr})
	if got := Ownerless(in); !slices.Equal(got, []string{pr}) {
		t.Fatalf("ownerless = %v", got)
	}
	a := only(t, Plan(in), KindReopen)
	in.Taken = append(in.Taken, taken(t, t0.Add(time.Minute), a)...)
	a = only(t, Plan(in), KindReview)
	if a.Round != 1 {
		t.Fatalf("recovered review: %+v", a)
	}
	in.Taken = append(in.Taken, taken(t, in.Now, a)...)
	if got := Plan(in); len(got) != 0 {
		t.Fatalf("replayed sweep: %+v", got)
	}
}

func TestReopenPreservesWorkerEventActions(t *testing.T) {
	e := Event{Verb: VerbDone, Agent: worker, At: t0}
	a := Action{Key: eventKey(KindDone, e), Kind: KindDone, Ref: pr, Agent: worker}
	log := taken(t, t0, a, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr}, Action{Key: "reopen " + pr, Kind: KindReopen, Ref: pr})
	if l := newLedger(log, nil); !l.has(a.Key) {
		t.Fatal("reopen lost unrelated worker action")
	}
}

func TestEndedPullRequiresFreshVerifiedRequest(t *testing.T) {
	log := taken(t, t1, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr}, Action{Key: endKey(pr), Kind: KindEnd, Ref: pr})
	for _, tc := range []struct {
		name string
		w    Worker
		want bool
	}{
		{"fresh", Worker{ParkedOn: pr, ReviewRequested: true, ParkedHead: head1, ParkedAt: t1.Add(time.Minute)}, true},
		{"old", Worker{ParkedOn: pr, ReviewRequested: true, ParkedHead: head1, ParkedAt: t0}, false},
		{"plain", Worker{ParkedOn: pr, ParkedAt: t1.Add(time.Minute)}, false},
		{"unverified", Worker{ParkedOn: pr, ReviewRequested: true, ParkedAt: t1.Add(time.Minute)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Tracked([]Worker{tc.w}, log, nil, Desk{})
			if (len(got) == 1) != tc.want {
				t.Fatalf("tracked = %v, want inclusion %v", got, tc.want)
			}
		})
	}
}
