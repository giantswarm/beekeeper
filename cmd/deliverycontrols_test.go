package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/issuemonitor"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	deliveryControlPendingRef = "o/r#90"
	deliveryControlActualHead = "actual"
	deliveryControlChecks     = "checks"
	deliveryControlDrop       = "drop"
	deliveryControlCapacity   = `{"ceiling":1,"room":1}`
	// deliveryControlCapacityTwo has room for one reservation: the
	// reserved slots are at most half the room.
	deliveryControlCapacityTwo = `{"ceiling":2,"room":2}`
)

func TestDeliveryAdmissionPreviewAndExceptions(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name           string
		limit, count   int
		stale          time.Duration
		exempt, repair bool
		pick           bool
	}{
		{name: "off stale", count: 3, stale: time.Hour, pick: true},
		{name: "under", limit: 2, count: 1, pick: true},
		{name: "full", limit: 2, count: 2},
		{name: "over", limit: 2, count: 3},
		{name: "stale", limit: 2, count: 1, stale: 11 * time.Minute},
		{name: "future fails closed", limit: 2, count: 1, stale: -time.Second},
		{name: "boundary fresh", limit: 2, count: 1, stale: 10 * time.Minute, pick: true},
		{name: "exempt", limit: 1, count: 1, exempt: true, pick: true},
		{name: "repair", limit: 1, count: 1, repair: true, pick: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &state.State{DeliveriesAt: now.Add(-tc.stale)}
			for i := 0; i < tc.count; i++ {
				st.StartDelivery(fmt.Sprintf("o/r#%d", i+20), "worker", "", now)
			}
			cs := boardCandidates(3)
			sc := pickScope{listed: now, alive: func(state.Party) bool { return false }, wipLimit: tc.limit, loopInterval: time.Second}
			if tc.exempt {
				cs[0].WIPExempt = true
			}
			if tc.repair {
				st.Holds = []state.Hold{{Target: deliveryFixtureRepo, Incident: true, Except: cs[0].Ref}}
			}
			res := nextFree(st, cs, sc)
			if (res.Pick != nil) != tc.pick {
				t.Fatalf("%+v", res)
			}
			if res.WouldPick == nil || res.WouldPick.Ref != cs[0].Ref || len(res.AfterPick) != 2 || len(res.Skipped) != 0 {
				t.Fatalf("preview lost: %+v", res)
			}
			if res.WIP.Count != tc.count || res.WIP.Limit != tc.limit {
				t.Fatal(res.WIP)
			}
			doc, err := nextJSON(res, nextView{fields: []string{"ref"}, limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(doc)
			var parsed map[string]json.RawMessage
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatal(err)
			}
			if len(parsed["wip"]) == 0 || len(parsed["would_pick"]) == 0 || !strings.Contains(string(parsed["after_pick_more"]), "1") {
				t.Fatal(string(raw))
			}
		})
	}
}

func TestDeliveryConcurrentLastClaim(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) { st.DeliveriesAt = now })
	var wg sync.WaitGroup
	results := make(chan nextResult, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			me := state.Party{Session: fmt.Sprintf("claim-%d", i)}
			res, err := claimNext(a.store, boardCandidates(3), pickScope{me: me, listed: now, alive: func(state.Party) bool { return true }, wipLimit: 1, loopInterval: time.Second}, "", func(string) bool { return false })
			if err != nil {
				t.Error(err)
			}
			results <- res
		})
	}
	wg.Wait()
	close(results)
	claimed := 0
	for res := range results {
		if res.Claimed {
			claimed++
		}
	}
	st, err := a.store.Read()
	if err != nil || claimed != 1 || len(st.Deliveries) != 1 {
		t.Fatalf("%d %+v %v", claimed, st, err)
	}
}

func TestDeliveryFullLoopReadsNothingAndReleaseHasNoPause(t *testing.T) {
	r, busy := fillRig(t)
	r.a.cfg.Delivery.WIPLimit = 1
	now := r.a.timeNow()
	seed(t, r.a, func(st *state.State) { st.DeliveriesAt = now; st.StartDelivery("o/r#99", "worker", "", now) })
	l := &supervisorLoop{a: r.a, said: map[string]bool{}, fillPause: fillPause{capacity: "3", until: now.Add(time.Hour)}}
	st, _ := r.a.store.Read()
	if err := l.fill(t.Context(), st, nil, nil); err != nil {
		t.Fatal(err)
	}
	if r.called(loopBoard) != nil || *busy != 0 || !l.fillPause.until.IsZero() {
		t.Fatalf("full walked or paused: %v %+v", r.calls, l.fillPause)
	}
	if _, err := l.startNext(t.Context(), superloop.Action{}); !errors.Is(err, errLoopLater) {
		t.Fatal(err)
	}
	if _, err := l.startItem(t.Context(), superloop.Action{}, loopBoardItem{Ref: epicTen}); !errors.Is(err, errLoopLater) {
		t.Fatal(err)
	}
	if r.called(loopBoard) != nil || r.called(agentsName) != nil {
		t.Fatal(r.calls)
	}
	seed(t, r.a, func(st *state.State) { st.Deliveries[0].Step = state.DeliveryDone })
	// The command mock records deliveries as a real start does, to bound this pass.
	old := r.answer
	r.answer = func(args []string) (string, int) {
		out, code := old(args)
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			seed(t, r.a, func(st *state.State) { st.StartDelivery(epicTen, args[2], "", now) })
		}
		return out, code
	}
	if err := l.fill(t.Context(), st, nil, nil); err != nil {
		t.Fatal(err)
	}
	if *busy != 1 || !l.fillPause.until.IsZero() {
		t.Fatalf("release did not free next pass: %d %+v", *busy, l.fillPause)
	}
}

func TestDeliveryReservationsRealPreviewAndEpisode(t *testing.T) {
	for _, step := range []string{state.DeliveryFixing, state.DeliveryReviewQueued} {
		t.Run(step, func(t *testing.T) {
			r, busy := fillRig(t)
			now := r.a.timeNow()
			seed(t, r.a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: deliveryControlPendingRef, Step: step, Since: now.Add(-time.Hour), Agent: holderGone}}
			})
			old := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == fillCapacityCommand {
					return deliveryControlCapacityTwo, 0
				}
				return old(args)
			}
			l := &supervisorLoop{a: r.a, said: map[string]bool{}}
			st, _ := r.a.store.Read()
			if err := l.fill(t.Context(), st, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := l.previewFill(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Of two slots the delivery holds one: one new start, not two.
			if *busy != 1 || strings.Count(r.out.String(), "NEXT RESERVES") != 1 {
				t.Fatalf("%s %v", r.out, r.calls)
			}
			// Resolving and recurring is a new reservation episode.
			seed(t, r.a, func(st *state.State) { st.Deliveries[0].Step = state.DeliveryCI })
			st, _ = r.a.store.Read()
			_ = l.deliveryRoom(1, st)
			seed(t, r.a, func(st *state.State) { st.Deliveries[0].Step = step })
			st, _ = r.a.store.Read()
			_ = l.deliveryRoom(1, st)
			if strings.Count(r.out.String(), "NEXT RESERVES") != 2 {
				t.Fatal(r.out)
			}
		})
	}
}

func TestDeliveryReservationsFrozenBeforeConcurrentStarts(t *testing.T) {
	r, busy := fillRig(t)
	now := r.a.timeNow()
	seed(t, r.a, func(st *state.State) {
		st.Deliveries = []state.Delivery{{Ref: deliveryControlPendingRef, Step: state.DeliveryFixing, Since: now, Agent: reserveFixer}}
	})
	st, _ := r.a.store.Read()
	l := &supervisorLoop{a: r.a, said: map[string]bool{}}
	reservations, _ := l.deliveryReservations(st)
	r.started(t, reserveFixer, "fixer-session")
	// The capacity result was read just before the other stream registered.
	current, _ := r.a.store.Read()
	if room := l.deliveryRoom(2, current, reservations); room != 1 {
		t.Fatalf("race gave room %d", room)
	}
	if *busy != 0 {
		t.Fatal(*busy)
	}
}

func TestDeliveryStallsRestartTwoWatchesRecoveryAndFull(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	a.cfg.Delivery.WIPLimit = 1
	a.cfg.Delivery.StallAfter.Duration = time.Hour
	seed(t, a, func(st *state.State) {
		st.Agents = []state.Agent{{Party: state.Party{Name: "live-worker"}, Task: deliveryTestTask}}
		st.Deliveries = []state.Delivery{{Ref: deliveryControlPendingRef, Agent: "live-worker", Step: state.DeliveryCI, Since: now.Add(-2 * time.Hour), Started: now.Add(-3 * time.Hour), Waits: deliveryControlChecks}}
	})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := a.checkDeliveries(now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == loopFileVerb })
	if err != nil || len(evs) != 2 {
		t.Fatalf("%+v %v", evs, err)
	}
	if !slices.ContainsFunc(evs, func(e state.Event) bool { return strings.Contains(e.Detail, "WIP FULL") }) {
		t.Fatal(evs)
	}
	if line, err := a.checkDeliveries(now.Add(-90 * time.Minute)); err != nil || line != "" {
		t.Fatalf("older tick undid newer episode: %s %v", line, err)
	}

	reopened, err := state.Open(a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	if line, err := a.checkDeliveries(now.Add(time.Minute)); err != nil || line != "" {
		t.Fatalf("%s %v", line, err)
	}
	st, _ := a.store.Read()
	if !st.Deliveries[0].Active() {
		t.Fatal("stall freed place")
	}
	now = now.Add(2 * time.Minute)
	seed(t, a, func(st *state.State) { st.Deliveries[0].Step = state.DeliveryFixing; st.Deliveries[0].Since = now })
	line, err := a.checkDeliveries(now)
	if err != nil || strings.Count(line, "RECOVERED") != 2 {
		t.Fatalf("%s %v", line, err)
	}
	line, err = a.checkDeliveries(now.Add(time.Minute))
	if err != nil || line != "" {
		t.Fatalf("change did not reset full timer: %s %v", line, err)
	}
	line, err = a.checkDeliveries(now.Add(time.Hour))
	if err != nil || !strings.Contains(line, "DELIVERY STALLED") || !strings.Contains(line, "WIP FULL") {
		t.Fatalf("%s %v", line, err)
	}
}

func TestDeliveryAllStepStallsAndReleaseFailures(t *testing.T) {
	for _, step := range []string{state.DeliveryUnknown, state.DeliveryImplementing, state.DeliveryCI, state.DeliveryReviewQueued, state.DeliveryReviewing, state.DeliveryFixing, state.DeliveryMerging, state.DeliveryReleasing} {
		t.Run(step, func(t *testing.T) {
			a, _ := supervisorApp(t)
			now := a.timeNow()
			seed(t, a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: refOne, Step: step, Since: now.Add(-25 * time.Hour)}}
			})
			if line, err := a.checkDeliveries(now); err != nil || !strings.Contains(line, "DELIVERY STALLED") {
				t.Fatalf("%s %v", line, err)
			}
			if line, err := a.checkDeliveries(now); err != nil || line != "" {
				t.Fatalf("%s %v", line, err)
			}
		})
	}
	for _, verdict := range []string{state.DeliveryReleaseFailed} {
		t.Run(verdict, func(t *testing.T) {
			a, _ := supervisorApp(t)
			now := a.timeNow()
			seed(t, a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: refOne, Step: state.DeliveryReleasing, Since: now, Verdict: verdict}}
			})
			if line, err := a.checkDeliveries(now); err != nil || !strings.Contains(line, "DELIVERY STALLED") {
				t.Fatalf("%s %v", line, err)
			}
		})
	}
}

func TestDeliveryDropReasonActorHeadAndReplay(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	a.as = guideName
	oldRead := readLoopPull
	t.Cleanup(func() { readLoopPull = oldRead })
	head := deliveryControlActualHead
	readLoopPull = func(context.Context, string) (superloop.Pull, error) { return superloop.Pull{Head: head}, nil }
	seed(t, a, func(st *state.State) {
		st.StartDelivery(epicTen, "worker", "", now)
		st.Deliveries[0].Pulls = []string{blockerRef}
		st.Deliveries[0].Heads = map[string]string{blockerRef: "stale-observation"}
	})
	run := func(reason string) error {
		c := a.deliveryCmd()
		c.SetArgs([]string{deliveryControlDrop, "https://github.com/o/r/issues/10", flagNoteReason, reason})
		return c.Execute()
	}
	if err := run("  "); Code(err) != ExitUsage {
		t.Fatal(err)
	}
	if err := run("duplicate delivery"); err != nil {
		t.Fatal(err)
	}
	if err := run("duplicate delivery"); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	d := st.Deliveries[0]
	if d.Active() || d.DropBy.Name != guideName || d.DropReason != "duplicate delivery" || d.Outcome != state.DeliveryDropped || d.Evidence != "duplicate delivery" || d.DroppedHeads[blockerRef] != deliveryControlActualHead {
		t.Fatalf("%+v", d)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "delivery.drop" })
	if err != nil || len(evs) != 1 || evs[0].By.Name != guideName {
		t.Fatalf("%+v %v", evs, err)
	}
	if len(st.Agents) != 0 || len(st.Merges) != 0 {
		t.Fatal("drop changed external lifecycle")
	}
	reopened, err := state.Open(a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	a.store = reopened
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	in := superloop.Input{Now: now.Add(time.Minute), Desk: superloop.Desk{Authors: []string{mailHost}}, Pulls: map[string]superloop.Pull{blockerRef: {Ref: blockerRef, Head: deliveryControlActualHead, Author: mailHost, State: superloop.Open, Closes: []string{epicTen}}}}
	got := superloop.Deliveries(in, st.Deliveries)
	if len(got) != 1 || got[0].Active() {
		t.Fatalf("restarted drop resurrected: %+v", got)
	}

	seed(t, a, func(st *state.State) {
		st.Deliveries[0].Step = state.DeliveryCI
		st.Deliveries[0].DropReason = ""
		st.Deliveries[0].Heads = nil
	})
	head = ""
	if err := run("unknown head"); Code(err) != ExitRefused {
		t.Fatal(err)
	}
}

// Run with -race -count repeatedly: capacity always reports the same last
// slot, even while review/transition and worker streams race each other. The
// reserved slots are at most half the room, so new work may take the last
// slot; the delivery's review and fix still start.
func TestDeliveryPriorityAcrossConcurrentStreams(t *testing.T) {
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	r.a.cfg.Supervisor.ReviewRounds = 2
	r.a.cfg.Capacity.ReviewHeadroom = 0
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "initial-worker")
	seed(t, r.a, func(st *state.State) { work := ""; st.Agents[0].Park.PostMerge = &work })
	r.answer = func(args []string) (string, int) {
		if args[0] == loopCapacity {
			return deliveryControlCapacity, 0
		}
		if args[0] == loopBoard {
			return `{"pick":{"ref":"o/r#99","url":"https://github.com/o/r/issues/99"}}`, 0
		}
		return "", 0
	}
	if err := r.a.dryRunSupervisorLoop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.called(agentsName, agentStartName) != nil {
		t.Fatalf("preview started: %v", r.calls)
	}

	r.pass(t)
	if r.called(agentsName, agentStartName, loopReviewer) == nil {
		t.Fatalf("review did not start: %v %s", r.calls, r.out)
	}
	r.verdict("priority-R1", loopHead, superloop.ChangesRequested, "fix the bug")
	r.pass(t)
	if r.called(agentsName, agentStartName, loopWorker) == nil {
		t.Fatalf("fixer did not start: %v %s", r.calls, r.out)
	}
}

func TestDeliveryFullMonitoredQueueCostsNoRead(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.StateDir = t.TempDir()
	r.a.cfg.Delivery.WIPLimit = 1
	now := r.a.timeNow()
	seed(t, r.a, func(st *state.State) { st.DeliveriesAt = now; st.StartDelivery(refOne, "worker", "", now) })
	if err := issuemonitor.WithState(t.Context(), r.a.issueMonitorDir(), func(st *issuemonitor.State, save func(*issuemonitor.State) error) error {
		st.Queue = []issuemonitor.Pointer{{Ref: idleRef}}
		return save(st)
	}); err != nil {
		t.Fatal(err)
	}
	old := issueMonitorGH
	t.Cleanup(func() { issueMonitorGH = old })
	issueMonitorGH = func(context.Context, ...string) ([]byte, error) {
		t.Error("WIP full read monitored queue")
		return nil, errors.New("unexpected queue read")
	}
	l := &supervisorLoop{a: r.a, said: map[string]bool{}}
	if _, err := l.nextItem(t.Context()); !errors.Is(err, errLoopLater) {
		t.Fatal(err)
	}
	if r.called(loopBoard) != nil {
		t.Fatal(r.calls)
	}
}

func TestDeliveryFreshGateAfterPlacement(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.Delivery.WIPLimit = 1
	r.a.cfg.Agents.Models = []config.AgentModelRule{{Match: ".*", Model: "test-model"}}
	now := r.a.timeNow()
	seed(t, r.a, func(st *state.State) { st.DeliveriesAt = now })
	old := r.answer
	r.answer = func(args []string) (string, int) {
		if args[0] == loopCapacity {
			seed(t, r.a, func(st *state.State) { st.StartDelivery(deliveryControlPendingRef, "other", "", now) })
		}
		return old(args)
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.startItem(t.Context(), superloop.Action{Key: "fresh gate"}, loopBoardItem{Ref: "o/r#99"}); !errors.Is(err, errLoopLater) {
		t.Fatal(err)
	}
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("stale pick dispatched after limit filled")
	}
}

func TestDeliveryManualStartOverLimitWarnsAndCounts(t *testing.T) {
	a, out := supervisorApp(t)
	a.cfg.Delivery.WIPLimit = 1
	now := a.timeNow()
	st := &state.State{}
	st.StartDelivery(refOne, "first", "", now)
	start := state.Start{Party: state.Party{Session: "manual-start", Name: "manual worker"}, At: now}
	if _, err := a.recordStart(st, start, "do the task", idleRef, func(state.Party) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if len(st.Deliveries) != 2 || !strings.Contains(out.String(), "2 of 1 in delivery") {
		t.Fatalf("%+v %s", st.Deliveries, out)
	}
}

func TestDeliveryFullJSONAndTextExitThree(t *testing.T) {
	a, out := supervisorApp(t)
	now := a.timeNow()
	st := &state.State{DeliveriesAt: now}
	st.StartDelivery(blockerRef, "worker", "", now)
	res := nextFree(st, boardCandidates(3), pickScope{listed: now, alive: func(state.Party) bool { return false }, wipLimit: 1, loopInterval: time.Second})
	if err := a.printNext(res, 3, nextView{}); Code(err) != ExitRefused || !strings.Contains(out.String(), "wip full: 1 of 1 in delivery") {
		t.Fatalf("%s %v", out, err)
	}
	out.Reset()
	a.json = true
	if err := a.printNext(res, 3, nextView{}); Code(err) != ExitRefused {
		t.Fatal(err)
	}
	var doc struct {
		Pick      *json.RawMessage  `json:"pick"`
		WouldPick *json.RawMessage  `json:"would_pick"`
		WIP       deliveryWIP       `json:"wip"`
		AfterPick []json.RawMessage `json:"after_pick"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Pick != nil || doc.WouldPick == nil || doc.WIP.Count != 1 || len(doc.AfterPick) != 2 {
		t.Fatal(out)
	}
}

func TestDeliveryRoomMinAfterReservation(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.Delivery.WIPLimit = 2
	now := r.a.timeNow()
	st := &state.State{DeliveriesAt: now, Deliveries: []state.Delivery{{Ref: refOne, Step: state.DeliveryReviewQueued, Since: now}}}
	l := &supervisorLoop{a: r.a, said: map[string]bool{}}
	if room := l.deliveryFillRoom(3, st); room != 1 {
		t.Fatalf("reservation incorrectly consumed WIP room: %d", room)
	}
}

func TestDeliveryMissingObservationAndZeroOff(t *testing.T) {
	now := time.Now()
	st := &state.State{}
	if v := readDeliveryWIP(st, 1, time.Second, now); !strings.Contains(v.Reason, "stale") {
		t.Fatal(v)
	}
	if v := readDeliveryWIP(st, 0, time.Second, now); v.Reason != "" {
		t.Fatal(v)
	}
}

func TestUnconfirmedReleaseStallGraceAndRecovery(t *testing.T) {
	for _, completes := range []bool{false, true} {
		t.Run(fmt.Sprintf("completes=%t", completes), func(t *testing.T) {
			a, _ := supervisorApp(t)
			a.cfg.Delivery.StallAfter.Duration = time.Minute
			now := a.timeNow()
			seed(t, a, func(st *state.State) {
				st.Deliveries = []state.Delivery{{Ref: refOne, Step: state.DeliveryReleasing, Since: now, Verdict: verdictUnconfirmed}}
			})
			for _, age := range []time.Duration{4 * time.Second, 30*time.Minute - time.Second} {
				if line, err := a.checkDeliveries(now.Add(age)); err != nil || line != "" {
					t.Fatalf("pending release at %s: %s %v", age, line, err)
				}
			}
			if completes {
				seed(t, a, func(st *state.State) {
					applyDeliveryVerdict(&st.Deliveries[0], verdictAvailable, "v1.2.3", now.Add(29*time.Minute))
				})
			}
			line, err := a.checkDeliveries(now.Add(30 * time.Minute))
			if err != nil || (strings.Contains(line, "DELIVERY STALLED") != !completes) {
				t.Fatalf("threshold: %s %v", line, err)
			}
			if line, err := a.checkDeliveries(now.Add(31 * time.Minute)); err != nil || line != "" {
				t.Fatalf("duplicate: %s %v", line, err)
			}
			if !completes {
				seed(t, a, func(st *state.State) {
					applyDeliveryVerdict(&st.Deliveries[0], verdictAvailable, "v1.2.3", now.Add(32*time.Minute))
				})
				if line, err := a.checkDeliveries(now.Add(32 * time.Minute)); err != nil || !strings.Contains(line, "RECOVERED:") {
					t.Fatalf("recovery: %s %v", line, err)
				}
			}
		})
	}
}
