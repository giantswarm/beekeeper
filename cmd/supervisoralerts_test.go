package cmd

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/state"
)

func loopFiring() alerts.Firing {
	return alerts.Firing{Installation: "test", Key: "fp@2026-01-01T00:00:00Z", Alert: alerts.Alert{Alertname: "WorkloadDown", Severity: "critical", Where: "ns/app", Since: "2026-01-01T00:00:00Z"}}
}

func TestLoopAlertPagesOnceAcrossRestartAndRefire(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = "critical"
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	r.pass(t)
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].For != loopPerson || st.Notes[0].ID != 1 || !strings.Contains(st.Notes[0].Text, f.Instance()) {
		t.Fatalf("notes: %+v", st.Notes)
	}
	f.Key += "-refired"
	r.pass(t)
	st, _ = r.a.store.Read()
	if len(st.Notes) != 2 {
		t.Fatalf("refire did not page: %+v", st.Notes)
	}
}

func TestLoopAlertOwnedAndOtherTeamDoNotPage(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = "critical"
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AlertOwners = []state.AlertOwner{{Alert: f.Key, By: state.Party{Name: "owner"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatal("owned alert paged")
	}
	r.a.cfg.Alerts.Team = "another-team"
	f.Key += "-new"
	r.pass(t)
	st, _ = r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatal("another team's alert paged")
	}
}

func TestLoopAlertWakesParkedWorkerOnce(t *testing.T) {
	r := newLoopRig(t)
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	r.park(t, alertParkPrefix+f.Alertname, time.Now())
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c == nil || !strings.Contains(c[3], f.Instance()) {
		t.Fatalf("wake: %v", c)
	}
	r.pass(t)
	if r.called(agentsName, agentWakeName) != nil {
		t.Fatal("duplicate wake after restart")
	}
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatal("parked owner also paged")
	}
}

func TestLoopAlertReplacesUnresumableWorkerAndRetriesStaffing(t *testing.T) {
	r := newLoopRig(t)
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	loopResumable = func(*app, *state.State, state.Agent) bool { return false }
	r.park(t, alertParkPrefix+f.Name(), time.Now())
	fail := true
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName && fail {
			return "no capacity", ExitRefused
		}
		return "", 0
	}
	r.pass(t)
	if r.called(agentsName, loopRemove, loopWorker) == nil {
		t.Fatal("unresumable session not retired")
	}
	if r.called(agentsName, agentWakeName) != nil {
		t.Fatal("attempted impossible wake")
	}
	c := r.called(agentsName, agentStartName, loopWorker)
	if c == nil {
		t.Fatal("original task not restaffed")
	}
	brief, err := os.ReadFile(c[3])
	if err != nil || !strings.Contains(string(brief), loopURL) || !strings.Contains(string(brief), f.Instance()) {
		t.Fatalf("brief %s: %v", brief, err)
	}
	// Simulate the real remove command: a failed start must not lose its task.
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	fail = false
	r.pass(t)
	if r.called(agentsName, agentStartName, loopWorker) == nil {
		t.Fatal("pending replacement lost across restart")
	}
	r.pass(t)
	if r.called(agentsName, agentStartName) != nil {
		t.Fatal("replacement started twice")
	}
}

func TestLoopAlertCadenceAndReadFailureRetry(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.Every.Duration = time.Minute
	l, err := r.a.newSupervisorLoop()
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) {
		reads++
		if reads == 1 {
			return nil, errors.New("unreachable")
		}
		return nil, nil
	}
	if err := l.watchAlerts(context.Background()); err == nil {
		t.Fatal("read failure lost")
	}
	if err := l.watchAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.watchAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Fatalf("reads %d", reads)
	}
	r.a.now = r.a.now.Add(time.Minute)
	if err := l.watchAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 3 {
		t.Fatalf("next reading missing: %d", reads)
	}
}

func TestLoopAlertBudgetHoldsWakeAndReplacement(t *testing.T) {
	for _, resumable := range []bool{true, false} {
		t.Run(strconv.FormatBool(resumable), func(t *testing.T) {
			r := newLoopRig(t)
			f := loopFiring()
			readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
			loopResumable = func(*app, *state.State, state.Agent) bool { return resumable }
			r.park(t, alertParkPrefix+f.Alertname, time.Now())
			r.answer = func(args []string) (string, int) {
				if slices.Equal(args, []string{"budget", "--gate"}) {
					return "under floor", ExitRefused
				}
				return "", 0
			}
			r.pass(t)
			if r.called(agentsName, agentWakeName) != nil || r.called(agentsName, agentStartName) != nil || r.called(agentsName, loopRemove) != nil {
				t.Fatalf("budget hold bypassed: %v", r.calls)
			}
		})
	}
}

func TestLoopAlertInterruptedWakeAndReplacementAreNotRepeated(t *testing.T) {
	for _, resumable := range []bool{true, false} {
		t.Run(strconv.FormatBool(resumable), func(t *testing.T) {
			r := newLoopRig(t)
			f := loopFiring()
			readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
			loopResumable = func(*app, *state.State, state.Agent) bool { return resumable }
			r.park(t, alertParkPrefix+f.Alertname, time.Now())
			r.failRecord = func(e state.Event) bool { return e.Verb == loopAlertVerb }
			r.pass(t)
			r.pass(t)
			if r.called(agentsName, agentWakeName) != nil || r.called(agentsName, agentStartName) != nil || r.called(agentsName, loopRemove) != nil {
				t.Fatalf("repeated interrupted side effect: %v", r.calls)
			}
		})
	}
}

func TestLoopAlertParkCommandAcceptsNamedEvent(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := park(t, w, "--on", "alert:WorkloadDown", "external event"); err != nil {
		t.Fatal(err)
	}
	if ag := agentOf(t, w); ag.Park == nil || ag.Park.On != "alert:WorkloadDown" {
		t.Fatalf("park: %+v", ag.Park)
	}
	if err := park(t, w, "--on", "alert:", "external event"); err == nil {
		t.Fatal("empty alert name accepted")
	}
}

func TestLoopAlertReadsTheWatchOwnedBaselineWithoutStealingIt(t *testing.T) {
	read := readLoopAlerts
	r := newLoopRig(t)
	r.a.cfg.StateDir = t.TempDir()
	store := alerts.NewStore(r.a.cfg.StateDir)
	owned, _, err := store.Own()
	if err != nil || !owned {
		t.Fatalf("own baseline: %v %v", owned, err)
	}
	defer func() { _ = store.Release() }()
	base, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	f := loopFiring()
	base.Installations[f.Installation] = &alerts.Installation{Reachable: true, Alerts: alerts.Set{f.Fingerprint(): f.Alert}}
	if err := store.Save(base); err != nil {
		t.Fatal(err)
	}
	got, err := read(context.Background(), r.a)
	if err != nil || len(got) != 1 || got[0].Instance() != f.Instance() {
		t.Fatalf("read shared baseline: %+v %v", got, err)
	}
	// The long-running watch retains ownership after the loop's read.
	other := alerts.NewStore(r.a.cfg.StateDir)
	ok, _, err := other.Own()
	if err != nil || ok {
		t.Fatalf("loop stole the watch's lock: %v %v", ok, err)
	}
}

func TestLoopAlertDeliveredWakeDoesNotPageAfterParkClears(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = "critical"
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	r.park(t, alertParkPrefix+f.Alertname, time.Now())
	r.pass(t)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Park = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatal("already staffed alert paged after wake cleared its park")
	}
}

func TestLoopAlertMissingTranscriptRestaffsOriginalTask(t *testing.T) {
	resumable := loopResumable
	r := newLoopRig(t)
	loopResumable = resumable
	r.a.cfg.Claude.ProjectsDir = t.TempDir()
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	r.park(t, alertParkPrefix+f.Alertname, time.Now())
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Session = "missing-transcript-session"
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, agentWakeName) != nil {
		t.Fatal("woke a session without a transcript")
	}
	if r.called(agentsName, loopRemove, loopWorker) == nil || r.called(agentsName, agentStartName, loopWorker) == nil {
		t.Fatal("missing transcript stranded the parked task")
	}
}
