package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// busyStore is a store whose state lock is always busy for Log: an event
// that gives up on a busy lock is lost, one that waits is recorded.
type busyStore struct{ state.Store }

func (busyStore) Log(...state.Event) error { return errors.New("the state lock is busy") }

// The log keeps every event, however old, and --since 30d returns every
// agents.start of the last 30 days, past the 50 the default -n shows.
func TestLogKeepsAgentsStartsForTheWindow(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	start := func(at time.Time, detail string) state.Event {
		return state.Event{At: at, By: state.Party{Name: supervisorName}, Verb: "agents.start", Detail: detail}
	}
	evs := []state.Event{start(now.AddDate(0, 0, -400), "a year back")}
	for i := range 60 {
		evs = append(evs,
			start(now.AddDate(0, 0, -29).Add(time.Duration(i)*time.Hour), fmt.Sprintf("worker %d", i)),
			state.Event{At: now.Add(-time.Minute), Verb: "run.start"})
	}
	if err := store.Record(evs...); err != nil {
		t.Fatal(err)
	}
	logged := func(args ...string) []state.Event {
		t.Helper()
		var out bytes.Buffer
		a := &app{store: store, out: &out, json: true, now: now}
		c := a.logCmd()
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatalf("log %v: %v", args, err)
		}
		var got []state.Event
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("log %v: %v in %q", args, err, out.String())
		}
		return got
	}
	if got := logged("--verb", "agents.start", "--since", "30d"); len(got) != 60 || got[0].Detail != "worker 0" || got[59].Detail != "worker 59" {
		t.Errorf("--since 30d = %d starts (%+v), want all 60 of the window", len(got), got)
	}
	if got := logged("--verb", "agents.start", "--since", "30d", "-n", "5"); len(got) != 5 || got[4].Detail != "worker 59" {
		t.Errorf("--since 30d -n 5 = %+v, want the latest 5", got)
	}
	if got := logged("--verb", "agents.start", "-n", "0"); len(got) != 61 || got[0].Detail != "a year back" {
		t.Errorf("-n 0 = %d starts, want 61 from a year back: nothing is trimmed", len(got))
	}
	if got := logged("--verb", "agents.start"); len(got) != 50 {
		t.Errorf("default = %d starts, want the latest 50", len(got))
	}
}

// --since takes days as well as Go durations.
func TestReportTimeTakesDays(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if got, err := reportTime(now, "30d", time.UTC); err != nil || !got.Equal(now.AddDate(0, 0, -30)) {
		t.Errorf("30d = %s, %v", got, err)
	}
	if _, err := reportTime(now, "-3d", time.UTC); Code(err) != ExitUsage {
		t.Errorf("-3d = %v, want a usage error", err)
	}
}
