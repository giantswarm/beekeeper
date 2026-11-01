package cmd

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/state"
)

func loopFiring() alerts.Firing {
	return alerts.Firing{Installation: "loop-fixture", Key: "fp@2026-01-01T00:00:00Z", Alert: alerts.Alert{Alertname: "WorkloadDown", Severity: alerts.Page, Where: "ns/app", Since: "2026-01-01T00:00:00Z"}}
}

func TestLoopAlertBlockedWorkPagesOnceAcrossRestartAndRefire(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	f := loopFiring()
	blockLoopAlert(t, r, f)
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

func TestLoopAlertOwnedDoesNotPage(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	f := loopFiring()
	blockLoopAlert(t, r, f)
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AlertOwners = []state.AlertOwner{{Alert: f.Key, By: state.Party{Name: "alert-fixture-owner"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	st, _ := r.a.store.Read()
	if len(st.Notes) != 0 {
		t.Fatal("owned alert paged")
	}
}

// With alerts.team set only the team's own alerts are relevant: another
// team's page, blocking work or not, gets neither a note nor a summary line.
func TestLoopAlertIgnoresOtherTeamsPages(t *testing.T) {
	read := readLoopAlerts
	r := newLoopRig(t)
	readLoopAlerts = read
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	r.a.cfg.Alerts.Team = "desk-team"
	f := loopFiring()
	blockLoopAlert(t, r, f)
	f.Team = "another-team"
	unowned := loopFiring()
	unowned.Installation, unowned.Team = "unowned-fixture", "another-team"
	w := r.a.newWatcher(false, false)
	w.publishAlerts([]alerts.Firing{f, unowned})
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil || len(st.Notes) != 0 {
		t.Fatalf("another team's page reached the guide: notes=%+v err=%v", st.Notes, err)
	}
	if strings.Contains(r.out.String(), "SUPERVISOR ALERTS") {
		t.Fatalf("another team's page was summarized: %s", r.out.String())
	}
}

// The team's alerts are relevant 24/7: one the business-hours inhibition
// mutes is read from the Alertmanager's answer, published by the watch and
// routed by the loop like any other page.
func TestLoopAlertTeamPageUnderWorkingHoursInhibitionCounts(t *testing.T) {
	read := readLoopAlerts
	r := newLoopRig(t)
	readLoopAlerts = read
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	r.a.cfg.Alerts.Team = "desk-team"
	hours := alerts.Raw{Fingerprint: "wh", Labels: map[string]string{"alertname": alerts.WorkingHours, "severity": "none"}, StartsAt: "2026-01-01T00:00:00Z"}
	labels := map[string]string{"alertname": "WorkloadDown", "severity": alerts.Page, "team": "desk-team",
		"namespace": "ns", "deployment": "app", "cancel_if_outside_working_hours": "true"}
	page := alerts.Raw{Fingerprint: "fp", Labels: labels, StartsAt: "2026-01-01T00:00:00Z", Status: alerts.Status{InhibitedBy: []string{"wh"}}}
	other := alerts.Raw{Fingerprint: "ot", Labels: map[string]string{"alertname": "WorkloadDown", "severity": alerts.Page, "team": "another-team"},
		StartsAt: "2026-01-01T00:00:00Z", Status: alerts.Status{InhibitedBy: []string{"wh"}}}
	rules := alerts.Rules{Team: "desk-team", Ignore: []string{alerts.WorkingHours}}
	var firing []alerts.Firing
	for fp, al := range rules.Normalize([]alerts.Raw{hours, page, other}, "loop-fixture") {
		firing = append(firing, alerts.Firing{Installation: "loop-fixture", Key: alerts.Key(fp, al), Alert: al})
	}
	if len(firing) != 1 || firing[0].Team != "desk-team" {
		t.Fatalf("read alerts: %+v", firing)
	}
	blockLoopAlert(t, r, firing[0])
	w := r.a.newWatcher(false, false)
	w.publishAlerts(firing)
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil || len(st.Notes) != 1 || !strings.Contains(st.Notes[0].Text, firing[0].Instance()) {
		t.Fatalf("inhibited team page did not reach the guide: notes=%+v err=%v", st.Notes, err)
	}
}

func TestLoopAlertWakesParkedWorkerOnce(t *testing.T) {
	r := newLoopRig(t)
	f := loopFiring()
	blockLoopAlert(t, r, f)
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	r.park(t, alertParkPrefix+f.Alertname, time.Now())
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c == nil || !strings.Contains(c[3], f.Instance()) {
		t.Fatalf("wake: %v", c)
	}
	afterWake, _ := r.a.store.Read()
	if afterWake.Agents[0].Park != nil {
		t.Fatal("successful wake left worker counted parked")
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

func TestLoopAlertBudgetHoldsWakeAndReplacement(t *testing.T) {
	for _, resumable := range []bool{true, false} {
		t.Run(strconv.FormatBool(resumable), func(t *testing.T) {
			r := newLoopRig(t)
			f := loopFiring()
			readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
			loopResumable = func(*app, *state.State, state.Agent) bool { return resumable }
			r.park(t, alertParkPrefix+f.Alertname, time.Now())
			if err := r.a.store.Record(event(watchParty, "watch.budget-state", "true")); err != nil {
				t.Fatal(err)
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
			if r.failRecord != nil {
				t.Fatal("interruption was not injected")
			}
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

func TestLoopAlertReadsWatchEvent(t *testing.T) {
	read := readLoopAlerts
	r := newLoopRig(t)
	f := loopFiring()
	w := r.a.newWatcher(false, false)
	w.publishAlerts([]alerts.Firing{f})
	w.emitNow("alerts", "NEW alert line after the structured snapshot")
	got, err := read(context.Background(), r.a)
	if err != nil || len(got) != 1 || got[0].Instance() != f.Instance() {
		t.Fatalf("watch event: %+v %v", got, err)
	}
	w.publishAlerts(nil)
	got, err = read(context.Background(), r.a)
	if err != nil || len(got) != 0 {
		t.Fatalf("resolved: %+v %v", got, err)
	}
}

func TestLoopAlertDeliveredWakeDoesNotPageAfterParkClears(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = alerts.Page
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

func TestLoopAlertFailedWakeDoesNotHideAnotherPage(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	f := loopFiring()
	other := f
	other.Key, other.Alertname = "another-fingerprint", "AnotherWorkloadDown"
	blockLoopAlert(t, r, other)
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f, other}, nil }
	r.park(t, alertParkPrefix+f.Alertname, time.Now())
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentWakeName {
			return "no capacity", ExitRefused
		}
		return "", 0
	}
	r.pass(t)
	st, _ := r.a.store.Read()
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0].Text, other.Instance()) {
		t.Fatalf("failed wake hid unrelated alert: %+v", st.Notes)
	}
}

func TestLoopAlertReparkWaitsForNewFiring(t *testing.T) {
	r := newLoopRig(t)
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	r.park(t, alertParkPrefix+f.Alertname, time.Now())
	r.pass(t)
	if r.called(agentsName, agentWakeName) == nil {
		t.Fatal("initial firing not delivered")
	}
	r.park(t, alertParkPrefix+f.Alertname, time.Now().Add(time.Second))
	r.pass(t)
	if r.called(agentsName, agentWakeName) != nil {
		t.Fatal("same firing woke reparked worker")
	}
	f.Key += "-refired"
	r.pass(t)
	if r.called(agentsName, agentWakeName) == nil {
		t.Fatal("new firing not delivered")
	}
}

func TestLoopAlertUnownedBurstDoesNotPage(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	var firing []alerts.Firing
	for i := range 123 {
		f := loopFiring()
		f.Installation = "unowned-" + strconv.Itoa(i%3)
		f.Key = strconv.Itoa(i)
		firing = append(firing, f)
	}
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return firing, nil }
	r.pass(t)
	r.pass(t) // A restart must not turn the same burst into guide decisions.
	summary := "SUPERVISOR ALERTS unowned: unowned-0=41, unowned-1=41, unowned-2=41; count=123; oldest firing=2026-01-01T00:00:00Z"
	if strings.Count(r.out.String(), summary) != 2 {
		t.Fatalf("missing aggregate summary: %s", r.out.String())
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 0 {
		t.Fatalf("unowned burst produced %d notes", len(st.Notes))
	}
}

func blockLoopAlert(t *testing.T, r *loopRig, f alerts.Firing) {
	t.Helper()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Records = append(st.Records, state.Record{Session: state.Party{Name: "blocked-worker"}, Issue: loopURL, Waits: alertParkPrefix + f.Name()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoopAlertRequiresExplicitBlockedWork(t *testing.T) {
	wait := alertParkPrefix + loopFiring().Alertname
	for _, tc := range []struct {
		name, issue, waits string
		ended              bool
	}{
		{name: "unrelated work", issue: loopURL, waits: "CI"},
		{name: "another installation", issue: loopURL, waits: "alert:another/WorkloadDown"},
		{name: "no owning item", waits: wait},
		{name: "ended work", issue: loopURL, waits: wait, ended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.Alerts.PageSeverity = alerts.Page
			r.a.cfg.Guide.Person = "" // Unowned alerts require no guide configuration.
			f := loopFiring()
			readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				rec := state.Record{Issue: tc.issue, Waits: tc.waits}
				if tc.ended {
					rec.Ended = time.Now()
				}
				st.Records = append(st.Records, rec)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Notes) != 0 {
				t.Fatalf("unrelated record paged: %+v", st.Notes)
			}
		})
	}
}

func TestLoopAlertBlockedNotesScopeInstallation(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Alerts.PageSeverity = alerts.Page
	f := loopFiring()
	other := f
	other.Installation = "another"
	blockLoopAlert(t, r, f)
	blockLoopAlert(t, r, other)
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f, other, f}, nil }
	r.pass(t)
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 2 {
		t.Fatalf("notes not deduplicated per installation: %+v", st.Notes)
	}
	for _, note := range st.Notes {
		if len(note.Refs) != 1 || !strings.Contains(note.StatusQuo, "blocks "+note.Refs[0]) {
			t.Fatalf("note lost blocked item: %+v", note)
		}
	}
}
