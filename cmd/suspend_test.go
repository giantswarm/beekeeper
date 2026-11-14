package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/watch"
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
	w.Standby = true
	entry := watchApp(w.Host).now
	exit := entry.Add(2 * time.Hour)
	worker := state.Party{Name: "fixture-worker", Session: "fixture-session"}
	supervisor := state.Party{Name: "fixture-supervisor", Session: "fixture-supervisor-session"}
	supervisedBy(t, w, supervisor, entry.Add(-time.Minute))
	err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
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
	w.SuspendDeliver = func(_ context.Context, wake watch.SuspendWake) error {
		calls[wake.Party.Name]++
		if wake.Party.Name == worker.Name && calls[worker.Name] == 1 {
			return errors.New("temporary fixture failure")
		}
		return nil
	}
	w.Suspends(t.Context(), entry.Add(-time.Minute), nil)
	if len(calls) != 0 {
		t.Fatal("recovered before resume")
	}
	fixture.records = []machine.SuspendRecord{{At: exit}}
	w.Suspends(t.Context(), entry, nil)
	w.Stand.Inflight.Wait()
	// Overlapping journal reads must not retry even an ambiguous delivery error.
	fixture.records = []machine.SuspendRecord{{At: entry, Entry: true}, {At: exit}}
	w.Suspends(t.Context(), exit, nil)
	w.Stand.Inflight.Wait()
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
	w.Standby = true
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	// A stopped desk must report gaps without scheduling recovery.
	st.Agents = []state.Agent{{Party: state.Party{Name: remoteMCPFixture}, Task: suspendFixtureTask}}
	st.Stop = &state.Stop{Reason: "suspend fixture stop"}
	w.Resumed(st, nil, watchApp(w.Host).now, watchApp(w.Host).now.Add(time.Hour))
	if len(w.SuspendRecovery) != 0 {
		t.Fatal("stopped desk queued recovery")
	}
}

func TestSuspendOnlyWakesStoppedBusyWorkers(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.Standby = true
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	st.Agents = []state.Agent{
		{Party: state.Party{Name: holderLive, Session: "live-id"}, Task: suspendFixtureTask},
		{Party: state.Party{Name: "suspend parked worker"}, Task: "wait", Park: &state.Park{}},
		{Party: state.Party{Name: reviewedStoppedCase, Session: "stopped-id"}, Task: suspendFixtureTask},
	}
	w.Resumed(st, []*claude.Session{{Name: holderLive, ID: "live-id", PID: 123}}, watchApp(w.Host).now, watchApp(w.Host).now.Add(time.Hour))
	if len(w.SuspendRecovery) != 1 || w.SuspendRecovery[reviewedStoppedCase].Party.Name != reviewedStoppedCase {
		t.Fatalf("queued: %v", w.SuspendRecovery)
	}
}

func TestSuspendRestartsSupervisorService(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.Standby = true
	fixture := &suspendFixture{Machine: plat.Machine, records: []machine.SuspendRecord{{At: watchApp(w.Host).now, Entry: true}, {At: watchApp(w.Host).now.Add(time.Hour)}}}
	original := plat.Machine
	plat.Machine = fixture
	defer func() { plat.Machine = original }()
	starts := 0
	w.Stand.LoopUnit = &watch.LoopService{
		State: func(context.Context) string { return "suspend fixture inactive" },
		Start: func(context.Context) error { starts++; return nil },
	}
	w.Suspends(t.Context(), watchApp(w.Host).now.Add(-time.Minute), nil)
	w.Stand.Inflight.Wait()
	if starts != 1 {
		t.Fatalf("supervisor starts: %d", starts)
	}
	w.Suspends(t.Context(), watchApp(w.Host).now, nil)
	if starts != 1 {
		t.Fatalf("duplicate supervisor recovery: %d", starts)
	}
}

func TestSuspendReportsEachCompletedGap(t *testing.T) {
	w, out := onceWatch(t, nil)
	fixture := &suspendFixture{Machine: plat.Machine, records: []machine.SuspendRecord{
		{At: watchApp(w.Host).now.Add(-time.Hour)}, // exit without a known entry is ignored
		{At: watchApp(w.Host).now, Entry: true},
		{At: watchApp(w.Host).now.Add(time.Hour)},
		{At: watchApp(w.Host).now.Add(2 * time.Hour), Entry: true},
		{At: watchApp(w.Host).now.Add(3 * time.Hour)},
		{At: watchApp(w.Host).now.Add(4 * time.Hour), Entry: true}, // no exit yet
	}}
	original := plat.Machine
	plat.Machine = fixture
	defer func() { plat.Machine = original }()
	w.Suspends(t.Context(), watchApp(w.Host).now.Add(-time.Hour), nil)
	w.Suspends(t.Context(), watchApp(w.Host).now, nil)
	if got := strings.Count(out.String(), "SUSPENDED / RESUMED"); got != 2 {
		t.Fatalf("gap lines: %d\n%s", got, out)
	}
	if len(w.SuspendRecovery) != 0 {
		t.Fatal("ordinary watch queued recovery")
	}
}

func TestSuspendRecoveryDoesNotRetryFailuresAfterRelay(t *testing.T) {
	w, out := onceWatch(t, nil)
	w.Standby = true
	entry := watchApp(w.Host).now
	exit := entry.Add(time.Hour)
	worker := state.Party{Name: "copy-worker", Session: bgID}
	oldGuide := state.Party{Name: "Guide run 56", Session: "old-guide-session"}
	newGuide := state.Party{Name: "Guide run 57", Session: "new-guide-session"}
	supervisedBy(t, w, oldGuide, entry.Add(-time.Minute))
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
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
	w.SuspendDeliver = func(_ context.Context, wake watch.SuspendWake) error {
		calls[wake.Party.Name]++
		if wake.Party.Name == worker.Name {
			return fmt.Errorf("waking %s: %w 9e3d7b10 of session %s; stopped the copy", worker.Name, errResumeStartedCopy, worker.Session)
		}
		if wake.Party.Name == oldGuide.Name {
			return errors.New("temporary guide failure before handover")
		}
		return nil
	}
	w.Suspends(t.Context(), entry.Add(-time.Minute), nil)
	w.Stand.Inflight.Wait()
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Relieved = append(st.Guide.Relieved, state.Relief{Party: oldGuide, At: exit})
		st.Guide.Holder = &state.Supervisor{Party: newGuide, Since: exit}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.Suspends(t.Context(), exit, nil)
	w.Stand.Inflight.Wait()
	if calls[worker.Name] != 1 || calls[oldGuide.Name] != 1 || calls[newGuide.Name] != 0 {
		t.Fatalf("deliveries: %v", calls)
	}
	if !strings.Contains(out.String(), "RESUME FAILED: \""+worker.Name+"\"") {
		t.Fatalf("missing copy failure log: %s", out)
	}
}

type staticProcMachine struct {
	platform.Machine
	table *proc.Table
}

func (m staticProcMachine) Processes() (*proc.Table, error) {
	return m.table, nil
}

func TestSuspendResumedGuideKeepsRoleForWorkDoneAnswerAndStart(t *testing.T) {
	const (
		origID            = "a2e980cb-6c26-411f-a3fd-45bc0a0ebde0"
		origShortID       = "a2e980cb"
		resumedID         = "2e1b7ca7-98b8-4682-a3e3-e9a7b0f31f0a"
		resumedName       = "beekeeper resume after suspend"
		guideName56       = "Guide run 56"
		resumedWorkText   = "follow up after suspend"
		resumedNoteText   = "Proceed with the fix?"
		resumedDoneReason = "closed after suspend"
		resumedAnswerText = "yes, proceed"
	)

	t.Run("background guide resumed with new session id in daemon listing", func(t *testing.T) {
		a, _ := stubApp(t)
		a.as = ""
		plat.Machine = tableMachine{plat.Machine}
		dir := t.TempDir()
		guide := state.Party{Name: guideName56, Session: origID}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Starts = []state.Start{{Party: guide, Mode: state.ModeBypass, Role: guideRole.Name, Dir: dir, Background: true, At: a.now.Add(-time.Hour)}}
			st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: a.now.Add(-time.Hour)}, Run: 56}
			st.GuideWork = []state.GuideWork{{ID: 1, Kind: paramTask, Text: resumedWorkText, Due: a.now, By: guide}}
			st.Notes = []state.Note{{ID: 1152, Kind: state.NoteDecision, For: guideRole.Name, Text: resumedNoteText, At: a.now.Add(-time.Minute)}}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		stubBackground(t, "note: woke session "+origShortID+" with its saved options.\nbackgrounded · "+origShortID+" · "+resumedName+"\n")
		woke := false
		origRun := bgRun
		bgRun = func(ctx context.Context, d string, env, argv []string) (string, error) {
			woke = true
			return origRun(ctx, d, env, argv)
		}
		bgList = func(context.Context, string, []string, bool) ([]bgSession, error) {
			if !woke {
				return nil, nil
			}
			return []bgSession{{ID: origShortID, SessionID: resumedID, Name: resumedName, Kind: bgKind}}, nil
		}
		if err := a.reviveBackground(t.Context(), guide, "beekeeper: the machine resumed from suspend"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CODE_SESSION_ID", resumedID)
		t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
		t.Setenv("CLAUDE_CODE_SESSION_NAME", resumedName)

		workCmd := a.guideWorkCmd()
		workCmd.SetArgs([]string{paramDone, "1", flagNoteReason, resumedDoneReason})
		if err := workCmd.Execute(); err != nil {
			t.Fatalf("guide work done: %v", err)
		}
		if err := a.recordAnswerWithOptions(t.Context(), "", 1152, 0, viaCLI, resumedAnswerText, false, answerNoteOpts{}); err != nil {
			t.Fatalf("answer: %v", err)
		}
		if err := a.runStart(t.Context(), guideRole, false); err != nil {
			t.Fatalf("guide start: %v", err)
		}
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if st.Guide == nil || st.Guide.Holder == nil || st.Guide.Holder.Session != resumedID || st.Guide.Holder.Name != guideName56 || st.Guide.Run != 56 {
			t.Fatalf("guide state after resume: %+v", st.Guide)
		}
	})

	t.Run("background guide resumed with new session id in cli record", func(t *testing.T) {
		a, _ := stubApp(t)
		a.as = ""
		dir := t.TempDir()
		guide := state.Party{Name: guideName56, Session: origID}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Starts = []state.Start{{Party: guide, Mode: state.ModeBypass, Role: guideRole.Name, Dir: dir, Background: true, At: a.now.Add(-time.Hour)}}
			st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: a.now.Add(-time.Hour)}, Run: 56}
			st.GuideWork = []state.GuideWork{{ID: 1, Kind: paramTask, Text: resumedWorkText, Due: a.now, By: guide}}
			st.Notes = []state.Note{{ID: 1152, Kind: state.NoteDecision, For: guideRole.Name, Text: resumedNoteText, At: a.now.Add(-time.Minute)}}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(a.cfg.Claude.SessionsDir, "4102140.json"),
			`{"pid":4102140,"sessionId":"`+origID+`","name":"`+guideName56+`","jobId":"`+origShortID+`","procStart":"100"}`)
		writeFile(t, filepath.Join(a.cfg.Claude.SessionsDir, "4242.json"),
			`{"pid":4242,"sessionId":"`+resumedID+`","name":"`+resumedName+`","jobId":"`+origShortID+`","procStart":"200"}`)
		plat.Machine = staticProcMachine{
			Machine: plat.Machine,
			table: &proc.Table{
				ByPID: map[int]*proc.Process{
					4102140: {PID: 4102140, Comm: claudeComm, StartTicks: 100, Args: []string{claudeComm, sessionIDFlag, origID, "-n", guideName56}},
					4242:    {PID: 4242, Comm: claudeComm, StartTicks: 200, Args: []string{"claude bg-spare", "--bg-spare"}},
				},
			},
		}
		t.Setenv("CLAUDE_CODE_SESSION_ID", resumedID)
		t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
		t.Setenv("CLAUDE_CODE_SESSION_NAME", resumedName)

		workCmd := a.guideWorkCmd()
		workCmd.SetArgs([]string{paramDone, "1", flagNoteReason, resumedDoneReason})
		if err := workCmd.Execute(); err != nil {
			t.Fatalf("guide work done: %v", err)
		}
		if err := a.recordAnswerWithOptions(t.Context(), "", 1152, 0, viaCLI, resumedAnswerText, false, answerNoteOpts{}); err != nil {
			t.Fatalf("answer: %v", err)
		}
		if err := a.runStart(t.Context(), guideRole, false); err != nil {
			t.Fatalf("guide start: %v", err)
		}
		if err := a.runStart(t.Context(), guideRole, true); err != nil {
			t.Fatalf("guide start --take-over: %v", err)
		}
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if st.Guide == nil || st.Guide.Holder == nil || st.Guide.Holder.Session != resumedID || st.Guide.Holder.Name != guideName56 || st.Guide.Run != 56 {
			t.Fatalf("guide state after resume: %+v", st.Guide)
		}
	})

	t.Run("guide resumed with same session id and new process 'beekeeper guide run N' beside stale predecessor", func(t *testing.T) {
		const (
			guideName51 = "Guide run 51"
			resumed51   = "beekeeper guide run 51"
		)
		a, _ := stubApp(t)
		a.as = ""
		dir := t.TempDir()
		guide := state.Party{Name: guideName51, Session: origID}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Starts = []state.Start{{Party: guide, Mode: state.ModeBypass, Role: guideRole.Name, Dir: dir, Background: true, At: a.now.Add(-time.Hour)}}
			st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: a.now.Add(-time.Hour)}, Run: 51}
			st.GuideWork = []state.GuideWork{{ID: 1, Kind: paramTask, Text: resumedWorkText, Due: a.now, By: guide}}
			st.Notes = []state.Note{{ID: 1152, Kind: state.NoteDecision, For: guideRole.Name, Text: resumedNoteText, At: a.now.Add(-time.Minute)}}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(a.cfg.Claude.SessionsDir, "4102140.json"),
			`{"pid":4102140,"sessionId":"`+origID+`","name":"`+guideName51+`","jobId":"`+origShortID+`","procStart":"100"}`)
		writeFile(t, filepath.Join(a.cfg.Claude.SessionsDir, "603457.json"),
			`{"pid":603457,"sessionId":"`+origID+`","name":"`+resumed51+`","jobId":"`+origShortID+`","procStart":"200"}`)
		plat.Machine = staticProcMachine{
			Machine: plat.Machine,
			table: &proc.Table{
				ByPID: map[int]*proc.Process{
					4102140: {PID: 4102140, Comm: claudeComm, StartTicks: 100, Args: []string{claudeComm, sessionIDFlag, origID, "-n", guideName51}},
					603457:  {PID: 603457, Comm: claudeComm, StartTicks: 200, Args: []string{claudeComm, resumeFlag, origID}},
				},
			},
		}
		t.Setenv("CLAUDE_CODE_SESSION_ID", origID)
		t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
		t.Setenv("CLAUDE_CODE_SESSION_NAME", resumed51)

		if err := a.roleStatus(guideRole); err != nil {
			t.Fatalf("guide status: %v", err)
		}
		workCmd := a.guideWorkCmd()
		workCmd.SetArgs([]string{paramDone, "1", flagNoteReason, resumedDoneReason})
		if err := workCmd.Execute(); err != nil {
			t.Fatalf("guide work done: %v", err)
		}
		if err := a.recordAnswerWithOptions(t.Context(), "", 1152, 0, viaCLI, resumedAnswerText, false, answerNoteOpts{}); err != nil {
			t.Fatalf("answer: %v", err)
		}
		if err := a.runStart(t.Context(), guideRole, false); err != nil {
			t.Fatalf("guide start: %v", err)
		}
		if err := a.runStart(t.Context(), guideRole, true); err != nil {
			t.Fatalf("guide start --take-over: %v", err)
		}
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if st.Guide == nil || st.Guide.Holder == nil || st.Guide.Holder.Session != origID || st.Guide.Holder.Name != guideName51 || st.Guide.Run != 51 {
			t.Fatalf("guide state after resume: %+v", st.Guide)
		}
	})

	t.Run("desktop guide started without prior holder and resumed with new session id", func(t *testing.T) {
		a, _ := stubApp(t)
		a.as = ""
		plat.Machine = tableMachine{plat.Machine}
		hostID := "local_" + origID
		seed := state.Party{Name: guideName56, Session: origID, HostSession: hostID}
		if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Starts = []state.Start{{Party: seed, Mode: state.ModeBypass, Role: guideRole.Name, At: a.now.Add(-time.Hour)}}
			st.Guide = &state.Role{
				Relay: &state.Relay{To: seed, At: a.now.Add(-time.Minute), Expires: a.now.Add(10 * time.Minute)},
				Run:   56,
			}
			_, evs, err := guideRole.Start(st, state.Party{Name: guideName56, Session: origID}, false, false, a.now)
			if err != nil {
				return nil, err
			}
			st.GuideWork = []state.GuideWork{{ID: 1, Kind: paramTask, Text: resumedWorkText, Due: a.now, By: seed}}
			st.Notes = []state.Note{{ID: 1152, Kind: state.NoteDecision, For: guideRole.Name, Text: resumedNoteText, At: a.now.Add(-time.Minute)}}
			return evs, nil
		}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CODE_SESSION_ID", resumedID)
		t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", hostID)
		t.Setenv("CLAUDE_CODE_SESSION_NAME", resumedName)

		workCmd := a.guideWorkCmd()
		workCmd.SetArgs([]string{paramDone, "1", flagNoteReason, resumedDoneReason})
		if err := workCmd.Execute(); err != nil {
			t.Fatalf("guide work done: %v", err)
		}
		if err := a.recordAnswerWithOptions(t.Context(), "", 1152, 0, viaCLI, resumedAnswerText, false, answerNoteOpts{}); err != nil {
			t.Fatalf("answer: %v", err)
		}
		if err := a.runStart(t.Context(), guideRole, false); err != nil {
			t.Fatalf("guide start: %v", err)
		}
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if st.Guide == nil || st.Guide.Holder == nil || st.Guide.Holder.HostSession != hostID || st.Guide.Holder.Name != guideName56 || st.Guide.Run != 56 {
			t.Fatalf("guide state after resume: %+v", st.Guide)
		}
	})
}

// A relay can finish after recovery is queued but before the serialized slot
// becomes available. Neither the old guide's roster entry nor its queued guide
// wake may resume it, and the successor's roster entry must not double-deliver.
func TestSuspendRecoveryRelayWhileQueued(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.Standby = true
	oldGuide := state.Party{Name: "Guide run 61", Session: "suspend-old-guide"}
	newGuide := state.Party{Name: "Guide run 62", Session: "suspend-new-guide"}
	supervisedBy(t, w, oldGuide, watchApp(w.Host).now.Add(-time.Minute))
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{
			{Party: oldGuide, Task: suspendFixtureTask},
			{Party: newGuide, Task: suspendFixtureTask},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w.Resumed(st, nil, watchApp(w.Host).now.Add(-time.Hour), watchApp(w.Host).now)
	w.Stand.Busy.Store(true)
	w.RecoverSuspend(t.Context())
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Relieved = append(st.Guide.Relieved, state.Relief{Party: oldGuide, By: newGuide, At: watchApp(w.Host).now, Taken: watchApp(w.Host).now})
		st.Guide.Holder = &state.Supervisor{Party: newGuide, Since: watchApp(w.Host).now}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	w.SuspendDeliver = func(_ context.Context, wake watch.SuspendWake) error {
		calls[wake.Party.Session]++
		return nil
	}
	w.Stand.Busy.Store(false)
	w.RecoverSuspend(t.Context())
	w.Stand.Inflight.Wait()
	w.RecoverSuspend(t.Context())
	w.Stand.Inflight.Wait()
	if len(calls) != 1 || calls[newGuide.Session] != 1 {
		t.Fatalf("deliveries after relay: %v", calls)
	}
	// A later suspend must also ignore the stale relieved roster entry.
	st, err = watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	w.Resumed(st, nil, watchApp(w.Host).now, watchApp(w.Host).now.Add(time.Hour))
	if len(w.SuspendRecovery) != 1 || !w.SuspendRecovery[newGuide.Name].Guide {
		t.Fatalf("queued after relay: %v", w.SuspendRecovery)
	}
}
