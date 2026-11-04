package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestParkedLeaseHolder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		roster bool
		park   bool
		timer  bool
		done   bool
		live   bool
		want   string
	}{
		{name: "parked with timer", roster: true, park: true, timer: true, want: holderParked},
		{name: "person can resume park", roster: true, park: true, want: holderParked},
		{name: "timer can resume", roster: true, timer: true, want: holderParked},
		{name: "no roster despite timer", timer: true, want: holderGone},
		{name: "no wake path", roster: true, want: holderGone},
		{name: "completed worker", roster: true, timer: true, done: true, want: holderGone},
		{name: "running worker", roster: true, live: true, want: holderLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claimApp(t)
			ag := state.Agent{Party: claimOther, Done: tc.done}
			if tc.park {
				ag.Park = &state.Park{On: holderPerson, Waits: "an answer", At: a.now}
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents, st.Timers, st.Records = nil, nil, []state.Record{{Session: claimOther, Issue: "giantswarm/beekeeper#660"}}
				if tc.roster {
					st.Agents = []state.Agent{ag}
				}
				if tc.timer {
					st.Timers = []state.Timer{{ID: 1, Wake: strings.ToLower(ag.Name), Due: a.now.Add(time.Hour)}}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			holdLease(t, a, graveler, claimOther)
			var sessions []*claude.Session
			if tc.live {
				sessions = []*claude.Session{{ID: claimOther.Session, Name: claimOther.Name}}
			}
			holders, err := a.leases()
			if err != nil {
				t.Fatal(err)
			}
			// The list's discovery is empty in this fixture.
			if !tc.live && (len(holders.Held) != 1 || holders.Held[0].State != tc.want) {
				t.Fatalf("lease list = %+v, want %s", holders.Held, tc.want)
			}
			var out strings.Builder
			a.out = &out
			w := &watcher{app: a, reported: map[string]bool{}, last: map[string]time.Time{}}
			w.staleLeases(context.Background(), sessions)
			stale := strings.Contains(out.String(), "STALE LEASE")
			if stale != (tc.want == holderGone) {
				t.Fatalf("stale alarm = %v, output %q", stale, out.String())
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			lines, _ := firePending(st, sessions, a.now)
			ended := strings.Contains(strings.Join(lines, "\n"), "SESSION ENDED")
			if ended != (tc.want == holderGone) || (tc.want == holderParked && !st.Records[0].Ended.IsZero()) {
				t.Fatalf("session end = %v, lines %v, record %+v", ended, lines, st.Records[0])
			}
			// Losing every wake path must make the same lease stale on the next poll.
			if tc.want == holderParked {
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Agents[0].Park, st.Timers = nil, nil
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				w.staleLeases(context.Background(), nil)
				if !strings.Contains(out.String(), "STALE LEASE") {
					t.Fatal("no stale alarm after losing wake path")
				}
				st, err = a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				lines, _ = firePending(st, nil, a.now)
				if !strings.Contains(strings.Join(lines, "\n"), "SESSION ENDED") {
					t.Fatal("no session end after losing wake path")
				}
			}
		})
	}
}
