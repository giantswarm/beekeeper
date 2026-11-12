package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestLoopStaleNextSettlesBeforeClaimReconciliation(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	l.StartedAt = r.a.now.Add(-30 * time.Minute)
	acts := []superloop.Action{}
	claims := map[string]loop.LoopClaim{}
	for i, age := range []time.Duration{6 * 24 * time.Hour, 2 * time.Hour, 61 * time.Minute} {
		key := fmt.Sprintf("stale next %d", i)
		acts = append(acts, superloop.Action{Key: key, Kind: superloop.KindNext, Event: &superloop.Event{At: r.a.now.Add(-age), Verb: superloop.VerbDone}})
		claims[key] = loop.LoopClaim{Target: "interrupted worker", At: r.a.now.Add(-age)}
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RunPassActions(t.Context(), st, claims, nil, nil, acts); err != nil {
		t.Fatal(err)
	}
	if *busy != 3 {
		t.Fatalf("refill busy=%d: %s", *busy, r.out)
	}
	for _, act := range acts {
		if !l.IsCompleted(act.Key) {
			t.Fatalf("unsettled %s", act.Key)
		}
	}
	if strings.Count(r.out.String(), "SKIPPED 3 stale done events") != 1 {
		t.Fatal(r.out)
	}
	// Persisted taken records suppress the backlog after a process restart too.
	count := 0
	for _, e := range r.logged(t, "supervisor.next") {
		if taken, ok := superloop.ParseTaken(e.At, e.Verb, e.Subject, e.Detail); ok && strings.HasPrefix(taken.Key, "stale next") {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("persisted stale actions=%d", count)
	}
}

func TestLoopStaleNextBoundaries(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.StartedAt = r.a.now.Add(-2 * time.Hour)
	for _, tc := range []struct {
		age   time.Duration
		stale bool
	}{
		{time.Hour, false}, {time.Hour + time.Nanosecond, true}, {time.Minute, false},
	} {
		act := superloop.Action{Kind: superloop.KindNext, Event: &superloop.Event{At: r.a.now.Add(-tc.age)}}
		if got := l.StaleNext(act); got != tc.stale {
			t.Fatalf("age=%s stale=%v", tc.age, got)
		}
	}
}

func TestLoopRefillSurvivesNextFailureAndInflightIntake(t *testing.T) {
	for _, failedNext := range []bool{false, true} {
		t.Run(fmt.Sprint(failedNext), func(t *testing.T) {
			r, busy := fillRig(t)
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			l.Once = true
			l.BeginInFlight("stream:intake")
			defer l.EndInFlight("stream:intake")
			acts := []superloop.Action{
				{Kind: superloop.KindDone, Key: "done missing worker", Agent: "missing next-action worker"},
				{Kind: superloop.KindIntake, Key: "intake pending"},
			}
			if failedNext {
				// Next bookkeeping cannot record its completion; refill still runs.
				acts = append(acts, superloop.Action{Kind: superloop.KindNext, Key: "next failure"})
				r.failRecord = func(e state.Event) bool {
					if e.Verb == "supervisor.next" && strings.Contains(e.Detail, "next failure") {
						return true
					}
					return false
				}
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			_ = l.RunPassActions(t.Context(), st, nil, map[string]loop.LoopFailure{}, nil, acts)
			if *busy != 3 {
				t.Fatalf("busy=%d: %s", *busy, r.out)
			}
		})
	}
}

func TestLoopNextWithOpenPullSettles(t *testing.T) {
	r, _ := fillRig(t)
	original := r.answer
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			return "item already has an open pull request", ExitRefused
		}
		return original(args)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	act := superloop.Action{Kind: superloop.KindNext, Key: "next refused"}
	if err := l.ExecAction(t.Context(), st, nil, map[string]loop.LoopFailure{}, act); err != nil {
		t.Fatal(err)
	}
	if !l.IsCompleted(act.Key) {
		t.Fatalf("open delivery did not settle next: %s", r.out)
	}
	if len(r.logged(t, "supervisor.next")) != 1 {
		t.Fatal(r.out)
	}
}
