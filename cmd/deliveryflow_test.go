package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	deliveryFlowTestTask  = "work"
	deliveryFlowTestVisit = "person's physical visit"
)

func TestDeliveryFlowClassification(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, on, waits, class                     string
		note, running, shortTimer, longTimer, pull bool
	}{
		{name: "active worker", class: deliveryFlowing, running: true},
		{name: "decision", on: "#1", note: true, class: deliveryPerson},
		{name: "missing decision", waits: "person's decision", class: deliveryPerson},
		{name: "visit", waits: deliveryFlowTestVisit, class: deliveryBlocked},
		{name: "external", waits: "external party delivers the equipment", class: deliveryBlocked},
		{name: "ci", on: deliveryTestPull, waits: "CI", pull: true, class: deliveryFlowing},
		{name: "queued review", on: deliveryTestPull, waits: deskReviewWait, pull: true, class: deliveryFlowing},
		{name: "release wait", on: "example/project@v1.0.0", class: deliveryFlowing},
		{name: "short timer", shortTimer: true, class: deliveryFlowing},
		{name: "long timer", longTimer: true, class: deliveryBlocked},
		{name: "unknown remains counted", waits: "something unspecified", class: deliveryFlowing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := state.Delivery{Ref: deliveryTestIssue, Agent: workerRole, Step: state.DeliveryImplementing, Since: now}
			park := &state.Park{On: tc.on, Waits: tc.waits, At: now}
			if tc.pull {
				park.RefKind = github.RefPullRequest
			}
			if tc.running {
				park = nil
			}
			st := &state.State{DeliveriesAt: now, Deliveries: []state.Delivery{d}, Agents: []state.Agent{{Party: state.Party{Name: workerRole}, Task: deliveryFlowTestTask, Park: park}}}
			if tc.note {
				st.Notes = []state.Note{{ID: 1, For: holderPerson, Refs: []string{d.Ref}}}
			}
			if tc.shortTimer || tc.longTimer {
				duration := time.Hour
				if tc.longTimer {
					duration = 48 * time.Hour
				}
				// The normal workflow arms the timer before the worker parks.
				st.Timers = []state.Timer{{Wake: workerRole, At: now.Add(-time.Minute), Due: now.Add(duration)}}
			}
			got := classifyDelivery(st, d, now, deliveryPolicy{})
			if got.Class != tc.class {
				t.Fatalf("classification %+v, want %s", got, tc.class)
			}
			if tc.name == "missing decision" && !got.MissingNote {
				t.Fatal("missing note not detected")
			}
			wip := readDeliveryWIP(st, 1, time.Second, now)
			if (wip.Count == 1) != (tc.class == deliveryFlowing) {
				t.Fatalf("admission %+v", wip)
			}
		})
	}
}

func TestMixedDeliveriesAdmitBelowFlowingLimit(t *testing.T) {
	now := time.Now().UTC()
	st := &state.State{DeliveriesAt: now}
	for i, name := range []string{"flow", "decision", "visit"} {
		ref := []string{reserveIssue, reservePull, incidentRepairIssue}[i]
		st.StartDelivery(ref, name, "", now)
		ag := state.Agent{Party: state.Party{Name: name}, Task: deliveryFlowTestTask}
		if i == 1 {
			ag.Park = &state.Park{On: "#1", At: now}
		}
		if i == 2 {
			ag.Park = &state.Park{Waits: deliveryFlowTestVisit, At: now}
		}
		st.Agents = append(st.Agents, ag)
	}
	st.Notes = []state.Note{{ID: 1, For: holderPerson, Refs: []string{reservePull}}}
	v := readDelivery(st, 2, now)
	if v.InDelivery != 1 || v.WaitingOnPerson != 1 || v.Blocked != 1 {
		t.Fatalf("split %+v", v)
	}
	if v.line() != "1 flowing of limit 2; 1 waiting on the person; 1 blocked" {
		t.Fatal(v.line())
	}
	if wip := readDeliveryWIP(st, 2, time.Second, now); wip.Reason != "" {
		t.Fatal(wip)
	}
	// A dependent delivery waits outside the flow too.
	st.Agents[0].Park = &state.Park{On: incidentRepairIssue}
	if got := classifyDelivery(st, st.Deliveries[0], now, deliveryPolicy{}); got.Class != deliveryBlocked {
		t.Fatal(got)
	}
	// A running fixer makes progress despite a parked original worker.
	st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: reserveFixer, Session: reserveFixer}, Task: testFixWord})
	st.Records = []state.Record{{Session: st.Agents[3].Party, Issue: incidentRepairIssue}}
	if got := classifyDelivery(st, st.Deliveries[2], now, deliveryPolicy{}); got.Class != deliveryFlowing {
		t.Fatal(got)
	}
}

func TestDeliveryWaitMissingNoteReportedOnce(t *testing.T) {
	a, _ := supervisorApp(t)
	seed(t, a, func(st *state.State) {
		st.StartDelivery(deliveryTestIssue, workerRole, "", a.timeNow())
		st.Agents = []state.Agent{{Party: state.Party{Name: workerRole}, Task: deliveryFlowTestTask, Park: &state.Park{Waits: "person's decision"}}}
	})
	line, err := a.checkDeliveries(a.timeNow())
	if err != nil || !strings.Contains(line, "WAITING WITHOUT A NOTE") {
		t.Fatalf("%s %v", line, err)
	}
	if line, err := a.checkDeliveries(a.timeNow().Add(time.Minute)); err != nil || line != "" {
		t.Fatalf("duplicate %s %v", line, err)
	}
}

// A park waits on the person only when no target settles it and its words
// name the person or what only a person gives. A note covers the wait: open,
// it is in its addressee's queue; answered a moment ago, the watch resumes
// the park.
func TestDeliveryPersonWaitNamesThePerson(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, on, waits, class string
		missing                bool
	}{
		{name: "blocker issue", on: "example/blocker#235", waits: "after the trace fix, finish the release-note gap", class: deliveryFlowing},
		{name: "agents note", waits: "GitHub budget reset; continuation recorded in agents note", class: deliveryFlowing},
		{name: "answered note", on: "#2382", waits: "the person's decision on the scope", class: deliveryFlowing},
		{name: "central note", on: "team#7", waits: "the person's decision on the scope", class: deliveryPerson},
		{name: "hold", on: "hold merges", waits: "the release window the person decides", class: deliveryFlowing},
		{name: "alert", on: "alert:WorkloadDown", waits: "the person's reply on the alert", class: deliveryFlowing},
		{name: "loop review", waits: "the loop's review of the change", class: deliveryFlowing},
		{name: "named person", waits: "waiting on Timo", class: deliveryPerson, missing: true},
		{name: "person's answer", waits: "the person's answer on the cache", class: deliveryPerson, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := state.Delivery{Ref: deliveryTestIssue, Agent: workerRole, Step: state.DeliveryImplementing, Since: now}
			st := &state.State{DeliveriesAt: now, Deliveries: []state.Delivery{d}, Agents: []state.Agent{{Party: state.Party{Name: workerRole}, Task: deliveryFlowTestTask, Park: &state.Park{On: tc.on, Waits: tc.waits, At: now}}}}
			got := classifyDelivery(st, d, now, deliveryPolicy{person: "Timo"})
			if got.Class != tc.class || got.MissingNote != tc.missing {
				t.Fatalf("classification %+v, want %s, missing note %t", got, tc.class, tc.missing)
			}
		})
	}
}

// The live waits of https://github.com/giantswarm/beekeeper/issues/3713: a
// worker parked on a blocker issue, one on GitHub's budget reset with its
// continuation in an agents note, and one on a note answered seconds before
// raise no alarm. A worker waiting on Timo without a note raises it once.
func TestDeliveryWaitWithoutNoteAlarmsOnlyForThePerson(t *testing.T) {
	a, _ := supervisorApp(t)
	a.cfg.Guide.Person = "Timo"
	const blocker, budget, answered, person = "example/project#11", "example/project#12", "example/project#13", "example/project#14"
	parks := map[string]state.Park{
		blocker:  {On: "example/blocker#235", RefKind: github.RefIssue, Waits: "After trace fix, verify green consumer CI; finish v1.13.0 v2 release-note gap"},
		budget:   {Waits: "GitHub budget reset for post-merge acceptance; continuation recorded in agents note."},
		answered: {On: "#2382", Waits: "App drift review needs existing scope decision"},
		person:   {Waits: "waiting on Timo"},
	}
	seed(t, a, func(st *state.State) {
		for _, ref := range []string{blocker, budget, answered, person} {
			name := "worker " + ref
			park := parks[ref]
			park.At = a.timeNow()
			st.StartDelivery(ref, name, "", a.timeNow())
			st.Agents = append(st.Agents, state.Agent{Party: state.Party{Name: name}, Task: deliveryFlowTestTask, Park: &park})
		}
	})
	line, err := a.checkDeliveries(a.timeNow())
	if err != nil || strings.Count(line, "WAITING WITHOUT A NOTE") != 1 || !strings.Contains(line, deliveryURL(person, false)+":") {
		t.Fatalf("%q %v", line, err)
	}
	if line, err := a.checkDeliveries(a.timeNow().Add(time.Minute)); err != nil || line != "" {
		t.Fatalf("again %q %v", line, err)
	}
}

func TestDeliveryExternalHoldRestoresBoardAfterRestart(t *testing.T) {
	a, _ := supervisorApp(t)
	f := useFakeBoard(t, a, map[int]string{7: holdWorking})
	seed(t, a, func(st *state.State) {
		st.StartDelivery(heldRef, workerRole, "", a.timeNow())
		st.Agents = []state.Agent{{Party: state.Party{Name: workerRole}, Task: deliveryFlowTestTask, Park: &state.Park{Waits: deliveryFlowTestVisit}}}
	})
	l := &loop.Loop{Host: a}
	if err := l.SyncDeliveryHolds(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.statusOf(7) != holdBlocked {
		t.Fatal(f.statusOf(7))
	}
	st, _ := a.store.Read()
	if st.DeliveryHolds[heldRef].Was != holdWorking {
		t.Fatalf("hold %+v", st.DeliveryHolds)
	}
	seed(t, a, func(st *state.State) { st.Agents[0].Park = nil })
	l = &loop.Loop{Host: a}
	if err := l.SyncDeliveryHolds(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.statusOf(7) != holdWorking {
		t.Fatal(f.statusOf(7))
	}
	st, _ = a.store.Read()
	if len(st.DeliveryHolds) != 0 {
		t.Fatal(st.DeliveryHolds)
	}
}

func TestExplicitExternalParkIsRetained(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := park(t, w, "--external", "equipment delivery"); err != nil {
		t.Fatal(err)
	}
	ag := agentOf(t, w)
	if ag.Park == nil || !ag.Park.External {
		t.Fatalf("park %+v", ag.Park)
	}
	if err := park(t, w, "--external", "--on", "#3", "equipment delivery"); err == nil {
		t.Fatal("external event accepted a decision target")
	}
}

func TestExecutionExternalParkIsRetained(t *testing.T) {
	a, _ := executionTestApp(t)
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs, err := a.applyExecutionControl(t.Context(), st, st.Agents[0].Party, execution.ControlRequest{
			Operation: execution.ControlPark, Data: mustExecutionJSON(t, executionPark{External: true, Waits: "equipment delivery"}),
		})
		return evs, err
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if st.Agents[0].Park == nil || !st.Agents[0].Park.External {
		t.Fatal("execution lost external wait classification")
	}
}

func TestDeliveryHoldCoexistsWithDecisionHold(t *testing.T) {
	a, _ := supervisorApp(t)
	f := useFakeBoard(t, a, map[int]string{7: holdWorking})
	seed(t, a, func(st *state.State) {
		st.StartDelivery(heldRef, workerRole, "", a.timeNow())
		st.Agents = []state.Agent{{Party: state.Party{Name: workerRole}, Task: deliveryFlowTestTask, Park: &state.Park{External: true, Waits: "equipment delivery"}}}
	})
	l := &loop.Loop{Host: a}
	if err := l.SyncDeliveryHolds(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	h, err := a.holdOn(t.Context(), st.Notes, heldRef)
	if err != nil {
		t.Fatal(err)
	}
	if h.Was != holdWorking {
		t.Fatalf("decision lost original Status: %+v", h)
	}
	seed(t, a, func(st *state.State) {
		st.Notes = []state.Note{{ID: 1, For: holderPerson, Refs: []string{heldRef}, Held: h}}
		st.Agents[0].Park = nil
	})
	if err := l.SyncDeliveryHolds(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.statusOf(7) != holdBlocked {
		t.Fatal("released another note's hold")
	}
	seed(t, a, func(st *state.State) { st.Notes = nil })
	if err := l.SyncDeliveryHolds(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.statusOf(7) != holdWorking {
		t.Fatal(f.statusOf(7))
	}
}
