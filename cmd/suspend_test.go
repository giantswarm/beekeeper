package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
)

const suspendFixtureTask = "work"

type suspendFixture struct {
	platform.Machine
	records []machine.SuspendRecord
}

func (f *suspendFixture) SuspendRecords(context.Context, time.Time) ([]machine.SuspendRecord, error) {
	return f.records, nil
}

func TestSuspendRecoveryAcrossPolls(t *testing.T) {
	w, out := onceWatch(t, nil)
	w.standby = true
	entry := w.now
	exit := entry.Add(2 * time.Hour)
	worker := state.Party{Name: "fixture-worker", Session: "fixture-session"}
	supervisor := state.Party{Name: "fixture-supervisor", Session: "fixture-supervisor-session"}
	supervisedBy(t, w, supervisor, entry.Add(-time.Minute))
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: worker, Task: "continue fixture"}, {Party: state.Party{Name: stateIdle}}, {Party: state.Party{Name: paramDone}, Task: "finished suspend fixture", Done: true}}
		st.Timers = []state.Timer{{ID: 1, Due: entry.Add(time.Hour), What: "fixture timer"}, {ID: 2, Due: exit.Add(time.Hour), What: "later suspend timer"}}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &suspendFixture{Machine: plat.Machine, records: []machine.SuspendRecord{{At: entry, Entry: true}}}
	original := plat.Machine
	plat.Machine = fixture
	defer func() { plat.Machine = original }()
	calls := map[string]int{}
	w.suspendDeliver = func(_ context.Context, wake suspendWake) error {
		calls[wake.party.Name]++
		if wake.party.Name == worker.Name && calls[worker.Name] == 1 {
			return errors.New("temporary fixture failure")
		}
		return nil
	}
	w.suspends(t.Context(), entry.Add(-time.Minute), nil)
	if len(calls) != 0 {
		t.Fatal("recovered before resume")
	}
	fixture.records = []machine.SuspendRecord{{At: exit}}
	w.suspends(t.Context(), entry, nil)
	w.stand.inflight.Wait()
	// Overlapping journal reads must not retry even an ambiguous delivery error.
	fixture.records = []machine.SuspendRecord{{At: entry, Entry: true}, {At: exit}}
	w.suspends(t.Context(), exit, nil)
	w.stand.inflight.Wait()
	if calls[worker.Name] != 1 || calls[supervisor.Name] != 1 || len(calls) != 2 {
		t.Fatalf("deliveries: %v", calls)
	}
	line := out.String()
	if strings.Count(line, "RESUME FAILED") != 1 || strings.Count(line, "SUSPENDED / RESUMED") != 1 || !strings.Contains(line, worker.Name) || !strings.Contains(line, "#1 fixture timer") || strings.Contains(line, "#2 later") {
		t.Fatalf("output: %s", line)
	}
}

func TestSuspendRespectsDeskStop(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.standby = true
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	// A stopped desk must report gaps without scheduling recovery.
	st.Agents = []state.Agent{{Party: state.Party{Name: remoteMCPFixture}, Task: suspendFixtureTask}}
	st.Stop = &state.Stop{Reason: "suspend fixture stop"}
	w.resumed(st, nil, w.now, w.now.Add(time.Hour))
	if len(w.suspendRecovery) != 0 {
		t.Fatal("stopped desk queued recovery")
	}
}

func TestSuspendOnlyWakesStoppedBusyWorkers(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.standby = true
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	st.Agents = []state.Agent{
		{Party: state.Party{Name: holderLive, Session: "live-id"}, Task: suspendFixtureTask},
		{Party: state.Party{Name: "suspend parked worker"}, Task: "wait", Park: &state.Park{}},
		{Party: state.Party{Name: reviewedStoppedCase, Session: "stopped-id"}, Task: suspendFixtureTask},
	}
	w.resumed(st, []*claude.Session{{Name: holderLive, ID: "live-id", PID: 123}}, w.now, w.now.Add(time.Hour))
	if len(w.suspendRecovery) != 1 || w.suspendRecovery[reviewedStoppedCase].party.Name != reviewedStoppedCase {
		t.Fatalf("queued: %v", w.suspendRecovery)
	}
}

func TestSuspendRestartsSupervisorService(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.standby = true
	fixture := &suspendFixture{Machine: plat.Machine, records: []machine.SuspendRecord{{At: w.now, Entry: true}, {At: w.now.Add(time.Hour)}}}
	original := plat.Machine
	plat.Machine = fixture
	defer func() { plat.Machine = original }()
	starts := 0
	w.stand.loopUnit = &loopService{
		state: func(context.Context) string { return "suspend fixture inactive" },
		start: func(context.Context) error { starts++; return nil },
	}
	w.suspends(t.Context(), w.now.Add(-time.Minute), nil)
	w.stand.inflight.Wait()
	if starts != 1 {
		t.Fatalf("supervisor starts: %d", starts)
	}
	w.suspends(t.Context(), w.now, nil)
	if starts != 1 {
		t.Fatalf("duplicate supervisor recovery: %d", starts)
	}
}

func TestSuspendReportsEachCompletedGap(t *testing.T) {
	w, out := onceWatch(t, nil)
	fixture := &suspendFixture{Machine: plat.Machine, records: []machine.SuspendRecord{
		{At: w.now.Add(-time.Hour)}, // exit without a known entry is ignored
		{At: w.now, Entry: true},
		{At: w.now.Add(time.Hour)},
		{At: w.now.Add(2 * time.Hour), Entry: true},
		{At: w.now.Add(3 * time.Hour)},
		{At: w.now.Add(4 * time.Hour), Entry: true}, // no exit yet
	}}
	original := plat.Machine
	plat.Machine = fixture
	defer func() { plat.Machine = original }()
	w.suspends(t.Context(), w.now.Add(-time.Hour), nil)
	w.suspends(t.Context(), w.now, nil)
	if got := strings.Count(out.String(), "SUSPENDED / RESUMED"); got != 2 {
		t.Fatalf("gap lines: %d\n%s", got, out)
	}
	if len(w.suspendRecovery) != 0 {
		t.Fatal("ordinary watch queued recovery")
	}
}

func TestSuspendRecoveryDoesNotRetryFailuresAfterRelay(t *testing.T) {
	w, out := onceWatch(t, nil)
	w.standby = true
	entry := w.now
	exit := entry.Add(time.Hour)
	worker := state.Party{Name: "copy-worker", Session: bgID}
	oldGuide := state.Party{Name: "Guide run 56", Session: "old-guide-session"}
	newGuide := state.Party{Name: "Guide run 57", Session: "new-guide-session"}
	supervisedBy(t, w, oldGuide, entry.Add(-time.Minute))
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: worker, Task: suspendFixtureTask}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture := &suspendFixture{Machine: plat.Machine, records: []machine.SuspendRecord{{At: entry, Entry: true}, {At: exit}}}
	original := plat.Machine
	plat.Machine = fixture
	defer func() { plat.Machine = original }()
	calls := map[string]int{}
	w.suspendDeliver = func(_ context.Context, wake suspendWake) error {
		calls[wake.party.Name]++
		if wake.party.Name == worker.Name {
			return fmt.Errorf("waking %s: %w 9e3d7b10 of session %s; stopped the copy", worker.Name, errResumeStartedCopy, worker.Session)
		}
		if wake.party.Name == oldGuide.Name {
			return errors.New("temporary guide failure before handover")
		}
		return nil
	}
	w.suspends(t.Context(), entry.Add(-time.Minute), nil)
	w.stand.inflight.Wait()
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Relieved = append(st.Guide.Relieved, state.Relief{Party: oldGuide, At: exit})
		st.Guide.Holder = &state.Supervisor{Party: newGuide, Since: exit}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.suspends(t.Context(), exit, nil)
	w.stand.inflight.Wait()
	if calls[worker.Name] != 1 || calls[oldGuide.Name] != 1 || calls[newGuide.Name] != 0 {
		t.Fatalf("deliveries: %v", calls)
	}
	if !strings.Contains(out.String(), "RESUME FAILED: \""+worker.Name+"\"") {
		t.Fatalf("missing copy failure log: %s", out)
	}
}

// A relay can finish after recovery is queued but before the serialized slot
// becomes available. Neither the old guide's roster entry nor its queued guide
// wake may resume it, and the successor's roster entry must not double-deliver.
func TestSuspendRecoveryRelayWhileQueued(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.standby = true
	oldGuide := state.Party{Name: "Guide run 61", Session: "suspend-old-guide"}
	newGuide := state.Party{Name: "Guide run 62", Session: "suspend-new-guide"}
	supervisedBy(t, w, oldGuide, w.now.Add(-time.Minute))
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{
			{Party: oldGuide, Task: suspendFixtureTask},
			{Party: newGuide, Task: suspendFixtureTask},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w.resumed(st, nil, w.now.Add(-time.Hour), w.now)
	w.stand.busy.Store(true)
	w.recoverSuspend(t.Context())
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Relieved = append(st.Guide.Relieved, state.Relief{Party: oldGuide, By: newGuide, At: w.now, Taken: w.now})
		st.Guide.Holder = &state.Supervisor{Party: newGuide, Since: w.now}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	w.suspendDeliver = func(_ context.Context, wake suspendWake) error {
		calls[wake.party.Session]++
		return nil
	}
	w.stand.busy.Store(false)
	w.recoverSuspend(t.Context())
	w.stand.inflight.Wait()
	w.recoverSuspend(t.Context())
	w.stand.inflight.Wait()
	if len(calls) != 1 || calls[newGuide.Session] != 1 {
		t.Fatalf("deliveries after relay: %v", calls)
	}
	// A later suspend must also ignore the stale relieved roster entry.
	st, err = w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w.resumed(st, nil, w.now, w.now.Add(time.Hour))
	if len(w.suspendRecovery) != 1 || !w.suspendRecovery[newGuide.Name].guide {
		t.Fatalf("queued after relay: %v", w.suspendRecovery)
	}
}
