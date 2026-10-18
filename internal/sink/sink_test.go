package sink

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func ev(at time.Time, subject string) state.Event {
	return state.Event{At: at, Verb: config.SinkServe, Subject: subject}
}

// The first claim starts the ledger and claims nothing before it; later
// claims take each event once, across ledgers on the same directory, never
// one older than the window; an unclaimed event is claimed again.
func TestTheLedgerClaimsEachEventOnce(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	start := time.Now()
	one, two := NewLedger(dir), NewLedger(dir)
	if got, err := one.Claim(ctx, start, []state.Event{ev(start.Add(-time.Second), "o/r#1")}); err != nil || len(got) != 0 {
		t.Fatalf("the first claim replays the log: %v, %v", got, err)
	}
	now := start.Add(time.Minute)
	evs := []state.Event{ev(start.Add(time.Second), "o/r#2"), ev(start.Add(2*time.Second), "o/r#3"), ev(start.Add(-time.Second), "o/r#1")}
	got, err := one.Claim(ctx, now, evs)
	if err != nil || len(got) != 2 || got[0].Subject != "o/r#2" || got[1].Subject != "o/r#3" {
		t.Fatalf("claimed %+v, %v", got, err)
	}
	if got, err := two.Claim(ctx, now, evs); err != nil || len(got) != 0 {
		t.Fatalf("another watch claims them again: %+v, %v", got, err)
	}
	if err := one.Unclaim(ctx, now, evs[1]); err != nil {
		t.Fatal(err)
	}
	if got, err := two.Claim(ctx, now, evs); err != nil || len(got) != 1 || got[0].Subject != "o/r#3" {
		t.Fatalf("the unclaimed event is claimed again: %+v, %v", got, err)
	}
	late := start.Add(Window + 3*time.Second)
	if got, err := one.Claim(ctx, late, []state.Event{ev(start.Add(2*time.Second), "o/r#4")}); err != nil || len(got) != 0 {
		t.Fatalf("an event older than the window: %+v, %v", got, err)
	}
}

// Dispatch runs the workflow on its ref with the inputs verb, subject and
// time only, and on the default branch without a ref.
func TestDispatchSendsTheEventsVerbSubjectAndTime(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 123, time.UTC)
	var calls []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	e := state.Event{At: at, Verb: config.SinkMerged, Detail: "o/r#7 exit 0, release v1.2.0", Subject: "o/r#7"}
	for _, s := range []config.Sink{{Repo: "o/site", Workflow: "refresh.yaml", Ref: "main"}, {Repo: "o/site", Workflow: "refresh.yaml"}} {
		if err := Dispatch(context.Background(), gh, s, e); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"workflow run refresh.yaml --repo o/site --ref main -f verb=merged -f subject=o/r#7 -f time=2026-10-18T12:00:00Z",
		"workflow run refresh.yaml --repo o/site -f verb=merged -f subject=o/r#7 -f time=2026-10-18T12:00:00Z",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %q\nwant %q", calls, want)
	}
}
