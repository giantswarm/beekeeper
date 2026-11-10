package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	abandonNoOwner     = "released owner"
	abandonClaimed     = "retained claim"
	abandonRoster      = "assigned owner"
	abandonLive        = "live serve record"
	abandonReviewPark  = "saved review park"
	abandonFailedRel   = "failed release"
	abandonFreshRel    = "recent release"
	abandonUnknown     = "missing end evidence"
	abandonArchive     = "archived owner"
	abandonEndedWorker = "ended worker"
	abandonDoneAgent   = "done roster owner"
	abandonName        = "abandonment-fixture-worker"
	abandonSession     = "abandonment-fixture-session"
	abandonReplacement = "abandonment-fixture-successor"
)

func TestEndedReleasedDeliveryDoesNotStall(t *testing.T) {
	for _, protect := range []string{abandonNoOwner, abandonClaimed, abandonRoster, abandonLive, abandonReviewPark, abandonFailedRel, abandonFreshRel, abandonUnknown, abandonArchive, abandonEndedWorker, abandonDoneAgent} {
		t.Run(protect, func(t *testing.T) {
			a, _ := supervisorApp(t)
			now := a.timeNow()
			by := state.Party{Name: abandonName, Session: abandonSession}
			seed(t, a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: refOne, Agent: by.Name, Step: state.DeliveryReleasing, Started: now.Add(-48 * time.Hour), Since: now.Add(-25 * time.Hour)}}
				st.Records = []state.Record{{Session: by, Issue: refOne, Ended: now.Add(-time.Hour)}}
				switch protect {
				case abandonClaimed:
					st.Claims = []state.Claim{{Issue: refOne, Session: by}}
				case abandonRoster:
					st.Agents = []state.Agent{{Party: by, Task: "finish delivery"}}
				case abandonLive:
					st.Records = append(st.Records, state.Record{Session: state.Party{Name: abandonReplacement}, Issue: refOne})
				case abandonReviewPark:
					st.ReviewParks = []state.ReviewPark{{Agent: by.Name, On: refOne, Serves: refOne}}
				case abandonFailedRel:
					st.Deliveries[0].Verdict = state.DeliveryReleaseFailed
				case abandonFreshRel:
					st.Deliveries[0].Since = now.Add(-5 * time.Minute)
				case abandonArchive:
					st.Records = nil
					st.Archives = []state.Archive{{Party: by, Since: now.Add(-time.Hour)}}
				case abandonEndedWorker:
					st.Records = nil
					st.EndedWorkers = []state.Start{{Party: by, At: now.Add(-50 * time.Hour)}}
				case abandonDoneAgent:
					st.Records = nil
					st.Agents = []state.Agent{{Party: by, Done: true, LastTask: "finish delivery"}}
				case abandonUnknown:
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
			want := protect == abandonNoOwner || protect == abandonArchive || protect == abandonEndedWorker || protect == abandonDoneAgent
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
				st.StartDelivery(refOne, abandonReplacement, "", now.Add(time.Hour))
				if !st.Deliveries[0].Active() || st.Deliveries[0].Abandoned {
					t.Fatal(st.Deliveries)
				}
			}
		})
	}
}

func TestEndedWorkerTimersAreCancelled(t *testing.T) {
	now := time.Now().UTC()
	by := state.Party{Name: abandonName, Session: abandonSession}
	endedBy := state.Party{Name: "ended-worker-name", Session: "ended-worker-session"}
	doneBy := state.Party{Name: "done-worker-name", Session: "done-worker-session"}
	handedBy := state.Party{Name: "handed-worker-name", Session: "handed-worker-session"}
	successor := state.Party{Name: abandonReplacement, Session: "successor-session"}
	st := &state.State{
		Agents:       []state.Agent{{Party: doneBy, Done: true}, {Party: successor}},
		Records:      []state.Record{{Session: by, Ended: now.Add(-time.Hour)}},
		EndedWorkers: []state.Start{{Party: endedBy, At: now.Add(-2 * time.Hour)}, {Party: handedBy, At: now.Add(-2 * time.Hour), Successor: &successor}},
		Timers: []state.Timer{
			{ID: 1, Wake: by.Name, Due: now.Add(-time.Minute), Repeat: state.RepeatDaily},
			{ID: 2, Wake: by.Session, Due: now.Add(time.Hour)},
			{ID: 3, Wake: "unknown-worker", Due: now.Add(time.Hour)},
			{ID: 4, Wake: endedBy.Name, Due: now.Add(time.Hour)},
			{ID: 5, Wake: doneBy.Name, Due: now.Add(time.Hour)},
			{ID: 6, Wake: handedBy.Session, Due: now.Add(time.Hour)},
		},
	}
	_, events, fires, changed := settleTimers(st, nil, now)
	if !changed || len(events) != 4 || len(fires) != 0 || len(st.Timers) != 2 || st.Timers[0].ID != 3 || st.Timers[1].ID != 6 {
		t.Fatalf("timers=%+v events=%+v fires=%+v", st.Timers, events, fires)
	}
	st.Agents = []state.Agent{{Party: by}}
	if endedTimerTarget(st, by.Name) {
		t.Fatal("registered continuation was cancelled")
	}
}
