package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestEndedReleasedDeliveryDoesNotStall(t *testing.T) {
	for _, protect := range []string{"none", "claim", "worker", "live record", "unknown", "archived"} {
		t.Run(protect, func(t *testing.T) {
			a, _ := supervisorApp(t)
			now := a.timeNow()
			by := state.Party{Name: "ended-worker", Session: "ended-session"}
			seed(t, a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: refOne, Agent: by.Name, Step: state.DeliveryReleasing, Started: now.Add(-48 * time.Hour), Since: now.Add(-25 * time.Hour)}}
				st.Records = []state.Record{{Session: by, Issue: refOne, Ended: now.Add(-time.Hour)}}
				switch protect {
				case "claim":
					st.Claims = []state.Claim{{Issue: refOne, Session: by}}
				case "worker":
					st.Agents = []state.Agent{{Party: by, Task: "release"}}
				case "live record":
					st.Records = append(st.Records, state.Record{Session: state.Party{Name: "replacement"}, Issue: refOne})
				case "archived":
					st.Records = nil
					st.Archives = []state.Archive{{Party: by, Since: now.Add(-time.Hour)}}
				case "unknown":
					st.Records = nil
				}
			})
			line, err := a.checkDeliveries(now)
			if err != nil {
				t.Fatal(err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			want := protect == "none" || protect == "archived"
			if st.Deliveries[0].Abandoned != want {
				t.Fatalf("delivery: %+v", st.Deliveries[0])
			}
			if want {
				if strings.Contains(line, "DELIVERY STALLED") {
					t.Fatal(line)
				}
				next := superloop.Deliveries(superloop.Input{Now: now.Add(time.Minute)}, st.Deliveries)
				if next[0].Active() {
					t.Fatalf("reopened: %+v", next)
				}
				st.StartDelivery(refOne, "replacement", "", now.Add(time.Hour))
				if !st.Deliveries[0].Active() || st.Deliveries[0].Abandoned {
					t.Fatal(st.Deliveries)
				}
			}
		})
	}
}

func TestEndedWorkerTimersAreCancelled(t *testing.T) {
	now := time.Now().UTC()
	by := state.Party{Name: "ended-worker", Session: "ended-session"}
	st := &state.State{Records: []state.Record{{Session: by, Ended: now.Add(-time.Hour)}}, Timers: []state.Timer{
		{ID: 1, Wake: by.Name, Due: now.Add(-time.Minute), Repeat: state.RepeatDaily},
		{ID: 2, Wake: by.Session, Due: now.Add(time.Hour)},
		{ID: 3, Wake: "unknown-worker", Due: now.Add(time.Hour)},
	}}
	_, events, fires, changed := settleTimers(st, nil, now)
	if !changed || len(events) != 2 || len(fires) != 0 || len(st.Timers) != 1 || st.Timers[0].ID != 3 {
		t.Fatalf("timers=%+v events=%+v fires=%+v", st.Timers, events, fires)
	}
	st.Agents = []state.Agent{{Party: by}}
	if endedTimerTarget(st, by.Name) {
		t.Fatal("registered continuation was cancelled")
	}
}
