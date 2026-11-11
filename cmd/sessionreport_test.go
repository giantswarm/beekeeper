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
