package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func sessionReportFixture(t *testing.T) *app {
	t.Helper()
	a, _ := noteApp(t)
	a.queueReports = true
	a.now = time.Now()
	a.cfg.StateDir = a.store.Dir()
	a.cfg.LeaseDir = t.TempDir()
	t.Setenv("PATH", t.TempDir())
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: "fixture task", AssignedAt: a.now}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func reportCommand(t *testing.T, a *app, c *cobra.Command, args ...string) {
	t.Helper()
	c.SetArgs(append([]string{}, args...))
	c.SetOut(a.out)
	c.SetErr(a.out)
	start := time.Now()
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("%v waited %s", args, elapsed)
	}
}

// This deliberately holds the actual exclusive file lock for the full two
// minutes from the acceptance criterion. No writer can rescue these commands.
func TestSessionReportsAndSnapshotsWithTwoMinuteWriter(t *testing.T) {
	a := sessionReportFixture(t)
	lock := flock.New(filepath.Join(a.store.Dir(), "state.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
	heldAt := time.Now()
	reportCommand(t, a, a.sessionsCmd(), "serve", agentOne, "fixture task")
	reportCommand(t, a, a.agentsCmd(), "problem", midtaskFinding)
	reportCommand(t, a, a.noteCmd(), "add", "Record the fixture action", "--kind", "todo", "--for", "supervisor")
	messages, err := state.PendingReports(a.store)
	if err != nil || len(messages) != 3 {
		t.Fatalf("messages: %v, %v", messages, err)
	}
	reportCommand(t, a, a.agentsCmd(), "park", "the fixture decision", "--on", reportReferencePrefix+messages[2].ID)
	reportCommand(t, a, a.agentsCmd(), verbIdle, flagDone, flagReport, "fixture completed", flagProblem, noProblems)
	reportCommand(t, a, a.statusCmd())
	reportCommand(t, a, a.capacityCmd())
	reportCommand(t, a, a.deliveryCmd())
	reportCommand(t, a, a.simCmd(), "forecast", "--no-board", "--save=false")
	st, err := a.store.Peek()
	if err != nil || st.Agents[0].Done || len(st.Records) != 0 {
		t.Fatalf("producer changed state: %+v, %v", st, err)
	}
	select {
	case <-time.After(time.Until(heldAt.Add(2 * time.Minute))):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	// Reopen both the store and consumer: nothing relies on producer memory.
	a.store, err = state.Open(a.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.consumeReports(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil || !st.Agents[0].Done || len(st.ReportReceipts) != 5 || len(st.Notes) != 1 {
		t.Fatalf("effects: %+v, %v", st, err)
	}
	for id, r := range st.ReportReceipts {
		if r.Error != "" {
			t.Errorf("%s: %s", id, r.Error)
		}
	}
	events, err := a.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, e := range events {
		verbs = append(verbs, e.Verb)
	}
	want := []string{verbServe, agentProblemEvent, noteAddedEvent, parkEvent, executionDone}
	last := -1
	for _, verb := range want {
		i := slices.Index(verbs, verb)
		if i <= last {
			t.Fatalf("out of order or missing %s: %v", verb, verbs)
		}
		last = i
	}
	if err := a.consumeReports(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := a.store.Events(0, nil)
	if err != nil || len(after) != len(events) {
		t.Fatalf("replayed events: %d -> %d, %v", len(events), len(after), err)
	}
}

func TestSessionReportReceiptSurvivesUnremovedMessage(t *testing.T) {
	a := sessionReportFixture(t)
	reportCommand(t, a, a.agentsCmd(), "problem", midtaskFinding)
	messages, err := state.PendingReports(a.store)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages: %v %v", messages, err)
	}
	m := messages[0]
	if err := a.consumeReports(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Restore exactly the committed input, as if the consumer crashed before
	// unlink. The durable receipt, not the absence of the file, deduplicates.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.store.Dir(), "session-reports", m.ID+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.consumeReports(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == agentProblemEvent })
	if err != nil || len(events) != 1 {
		t.Fatalf("duplicate application: %v %v", events, err)
	}
}

func TestDelayedReportCannotCompleteAnotherAssignment(t *testing.T) {
	a := sessionReportFixture(t)
	reportCommand(t, a, a.agentsCmd(), verbIdle, flagDone, flagReport, "fixture done", flagProblem, noProblems)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Task = "replacement task"
		st.Agents[0].AssignedAt = a.now.Add(time.Minute)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.consumeReports(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Peek()
	if err != nil || st.Agents[0].Done || st.Agents[0].Task != "replacement task" {
		t.Fatalf("replacement changed: %+v %v", st, err)
	}
	for _, r := range st.ReportReceipts {
		if !strings.Contains(r.Error, "replaced assignment") {
			t.Fatalf("receipt: %+v", r)
		}
	}
}

func TestSessionReportDeliveryOutcomes(t *testing.T) {
	a := sessionReportFixture(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Task = heldRef
		st.StartDelivery(heldRef, agentOne, "", a.now)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	c := a.agentsCmd()
	c.SetArgs([]string{stateIdle, flagDone, flagReport, "Completed https://github.com/giantswarm/beekeeper/issues/7", flagProblem, ghNone})
	c.SetOut(a.out)
	c.SetErr(a.out)
	if err := c.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), taskEndings) {
		t.Fatalf("accepted completion without merged pull request: %v", err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Deliveries[0].Pulls = []string{alsoHeldRef}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	prev := refStates
	t.Cleanup(func() { refStates = prev })
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		return map[github.PR]github.RefState{refs[0]: {Kind: github.RefPullRequest, State: github.Merged}}, nil
	}
	reportCommand(t, a, a.agentsCmd(), stateIdle, flagDone, flagReport, "Merged https://github.com/giantswarm/beekeeper/pull/8", flagProblem, ghNone)
	if err := a.consumeReports(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Peek()
	if err != nil || !st.Agents[0].Done || st.Deliveries[0].Outcome != state.OutcomeMerged {
		t.Fatalf("delivery outcome not recorded: %+v, %v", st, err)
	}
}

// The doctor leaves a session on the roster while a report of its current
// assignment waits in the inbox, and takes it off once the loop applied the
// report. On a desk the watch's doctor took a reviewer off as gone three
// minutes after it queued its approval; the loop then rejected the approval
// as one of an ended assignment, and the guide never got it
// (https://github.com/giantswarm/beekeeper/issues/3625).
func TestDoctorKeepsSessionUntilItsQueuedReportApplies(t *testing.T) {
	a, _ := stubApp(t)
	runLive(t, a, nil)
	a.queueReports = true
	a.cfg.Agents.StaleAfter.Duration = 24 * time.Hour
	ag := tasked("report-test")
	ag.Task = "unfinished fixture"
	ag.Registered = a.now.Add(-48 * time.Hour)
	ag.AssignedAt = ag.Registered
	seed(t, a, func(st *state.State) { st.Agents = []state.Agent{ag} })
	as := a.as
	a.as = ag.Name
	reportCommand(t, a, a.agentsCmd(), verbIdle, flagDone, flagReport, "fixture completed", flagProblem, noProblems)
	a.as = as
	messages, err := state.PendingReports(a.store)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages: %v, %v", messages, err)
	}
	id := messages[0].ID
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, gone := goneAgent(a.cfg, st, nil, st.Agents[0], a.cfg.Agents.StaleAfter.Duration, a.now); !gone {
		t.Fatal("precondition: the session is not gone")
	}

	rep, err := a.doctor(t.Context(), doctorRun{by: watchParty, dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rep.chores, func(l string) bool {
		return strings.HasPrefix(l, `would leave "report-test" on the roster`) && strings.Contains(l, reportReferencePrefix+id)
	}) {
		t.Fatalf("dry run does not name the waiting report: %q", rep.chores)
	}
	if _, err := a.doctor(t.Context(), doctorRun{by: watchParty}); err != nil {
		t.Fatal(err)
	}
	// The loop's own roster tidy removes through the same path.
	chores := planChores(st, nil, func(string) (*claude.Record, bool) { return nil, false }, func(state.Party) bool { return false }, a.cfg.Agents.StaleAfter.Duration, a.now, a.cfg)
	if removed, err := a.removeAgents(chores, loopParty); err != nil || len(removed) != 0 {
		t.Fatalf("removed with a waiting report: %+v, %v", removed, err)
	}
	st, err = a.store.Read()
	if err != nil || len(st.Agents) != 1 || st.Agents[0].Task != ag.Task {
		t.Fatalf("roster: %+v, %v", st.Agents, err)
	}

	if err := a.consumeReports(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := st.ReportReceipts[id]; !ok || r.Error != "" || len(st.Agents) != 1 || !st.Agents[0].Done {
		t.Fatalf("report not applied: receipt %+v, roster %+v", st.ReportReceipts, st.Agents)
	}
	rep, err = a.doctor(t.Context(), doctorRun{by: watchParty})
	if err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil || len(st.Agents) != 0 {
		t.Fatalf("done session stays: %+v, %v", st.Agents, err)
	}
	if !slices.Contains(rep.chores, `took "report-test" off the roster: `+whyReportedDone) {
		t.Fatalf("chores: %q", rep.chores)
	}
}

// Only a report of the entry's current assignment keeps it on the roster,
// and an unreadable inbox keeps every entry.
func TestKeepReportingMatchesTheAssignment(t *testing.T) {
	ag := tasked("report-match")
	ag.AssignedAt = keepNow
	queued := func(change func(*sessionReport)) []state.ReportMessage {
		p := sessionReport{Kind: reportKindIdle, Registered: true, Assignee: ag.Party, Task: ag.Task, AssignedAt: ag.AssignedAt}
		if change != nil {
			change(&p)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return []state.ReportMessage{{ID: "m1", By: ag.Party, Kind: p.Kind, Payload: raw}}
	}
	for name, tt := range map[string]struct {
		messages []state.ReportMessage
		inboxErr error
		kept     string
	}{
		"current assignment": {messages: queued(nil), kept: reportReferencePrefix + "m1"},
		"earlier assignment": {messages: queued(func(p *sessionReport) { p.AssignedAt = keepNow.Add(-time.Hour) })},
		"another task":       {messages: queued(func(p *sessionReport) { p.Task = "an earlier task" })},
		"another assignee":   {messages: queued(func(p *sessionReport) { p.Assignee = tasked("report-other").Party })},
		"unregistered":       {messages: queued(func(p *sessionReport) { p.Registered = false })},
		"empty inbox":        {},
		"unreadable inbox":   {inboxErr: os.ErrPermission, kept: "inbox is unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			chores := keepReporting([]chore{{kind: choreRemove, agent: ag, why: "its CLI ended"}}, tt.messages, tt.inboxErr)
			if kept := chores[0].kind == choreKeep; kept != (tt.kept != "") || kept && !strings.Contains(chores[0].why, tt.kept) {
				t.Fatalf("chore = %+v, want kept for %q", chores[0], tt.kept)
			}
		})
	}
}
