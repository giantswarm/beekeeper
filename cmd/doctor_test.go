package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The doctor takes a finished worker off the roster once its CLI is idle,
// a desktop CLI kept warm included, a relieved role holder at once and an idle worker without a CLI once it
// went stale, archiving the desktop sessions of the finished and the stale
// ones; it retitles a started session the desktop dropped the title of,
// and leaves busy agents, role holders and sessions a person started alone.
func TestPlanChores(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	agent := func(id string, idle time.Duration) state.Agent {
		return state.Agent{Party: state.Party{Session: id, Name: "test: " + id}, IdleSince: now.Add(-idle)}
	}
	done, doneBusy := agent("done", time.Minute), agent("done-busy", time.Minute)
	done.Done, doneBusy.Done = true, true
	task, doneTask := agent("task", 48*time.Hour), agent("done-task", time.Minute)
	task.Task, doneTask.Task, doneTask.Done = "a task", "a new task", true
	st := &state.State{
		Agents: []state.Agent{done, doneBusy, agent("relieved", 0), agent("stale", 48*time.Hour), agent("stale-live", 48*time.Hour),
			agent("fresh", time.Hour), task, agent("holder", 48*time.Hour), agent("untitled", 0), agent("titled", 0), agent("own", 0), agent("archived", 0), doneTask},
		Supervisor: &state.Supervisor{Party: state.Party{Session: "holder"}},
		Relieved:   []state.Relief{{Party: state.Party{Session: "relieved"}}},
	}
	for _, id := range []string{"untitled", "titled", "archived", "holder"} {
		st.Starts = append(st.Starts, state.Start{Party: state.Party{Session: id, HostSession: "local_" + id}})
	}
	// done's CLI still runs, idle: the desktop keeps the session warm.
	sessions := []*claude.Session{{ID: "done"}, {ID: "done-busy"}, {ID: "done-task"}, {ID: "stale-live"}, {ID: "untitled"}, {ID: "titled"}, {ID: "own"}}
	record := func(host string) (*claude.Record, bool) {
		switch host {
		case "local_untitled":
			return &claude.Record{Title: "klaus-lab-3d"}, true
		case "local_titled":
			return &claude.Record{Title: "test: titled"}, true
		case "local_archived":
			return &claude.Record{IsArchived: true}, true
		}
		return &claude.Record{}, true
	}
	busy := func(p state.Party) bool { return p.Session == "done-busy" }
	var got []string
	for _, c := range planChores(st, sessions, record, busy, 24*time.Hour, now) {
		got = append(got, c.String())
	}
	want := []string{
		`take "test: done" off the roster and archive its desktop session (it reported its work done)`,
		`take "test: relieved" off the roster (it was relieved of its role, which relays instead)`,
		`take "test: stale" off the roster and archive its desktop session (idle 2d, its CLI no longer runs)`,
		`retitle local_untitled "test: untitled" (the desktop recorded "klaus-lab-3d" instead of "test: untitled")`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("chores:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := planChores(st, sessions, record, busy, 0, now); slices.ContainsFunc(c, func(c chore) bool { return strings.Contains(c.why, "idle") }) {
		t.Errorf("staleAfter 0 takes no stale agent off: %v", c)
	}
}

// A configured fault is probed; a failing one whose remedy may run
// unattended is remedied and probed again, one that may not is only
// remedied when named; a remedy that does not fix it says so.
func TestCheckFaults(t *testing.T) {
	const stuck, unit = "stuck", "unit"
	broken := map[string]bool{unit: true, "manual": true, stuck: true}
	var ran []string
	run := func(_ context.Context, command string, _ time.Duration) error {
		name, verb, _ := strings.Cut(command, " ")
		if verb == "fix" {
			ran = append(ran, name)
			if name != stuck {
				broken[name] = false
			}
			return nil
		}
		if broken[name] {
			return errors.New("exit status 1")
		}
		return nil
	}
	faults := []config.Fault{
		{Name: "ok", Probe: "ok probe", Remedy: "ok fix", Unattended: true},
		{Name: unit, Probe: unit + " probe", Remedy: unit + " fix", Unattended: true},
		{Name: "manual", Probe: "manual probe", Remedy: "manual fix"},
		{Name: stuck, Probe: stuck + " probe", Remedy: stuck + " fix", Unattended: true},
	}
	fix := func(f config.Fault) bool { return f.Unattended }
	got := checkFaults(context.Background(), faults, fix, run)
	if !slices.Equal(ran, []string{unit, stuck}) {
		t.Errorf("remedies ran: %v, want unit and stuck", ran)
	}
	for i, want := range []string{"absent", `fixed fault "unit"`, "runs attended only: beekeeper doctor --fault manual", "also after its remedy"} {
		if !strings.Contains(got[i].String(), want) {
			t.Errorf("fault %s: %q, want %q", faults[i].Name, got[i], want)
		}
	}
	got = checkFaults(context.Background(), faults[2:3], func(config.Fault) bool { return true }, run)
	if !got[0].fixed {
		t.Errorf("--fault manual: %q", got[0])
	}
}

// A fault the doctor could not fix is one note for the person however
// often it is seen, and the note closes once the fault is absent.
func TestNoteFaults(t *testing.T) {
	st := &state.State{}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	failing := []faultFinding{{fault: config.Fault{Name: "unit", Probe: "p", Remedy: "r"}}}
	for range 3 {
		noteFaults(st, failing, notePerson, now)
	}
	if len(st.Notes) != 1 || st.Notes[0].For != notePerson || !strings.Contains(st.Notes[0].Text, "beekeeper doctor --fault unit") {
		t.Fatalf("notes after three sightings: %+v", st.Notes)
	}
	failing[0].healthy = true
	if lines, _ := noteFaults(st, failing, notePerson, now); len(st.Notes) != 0 || len(lines) != 1 {
		t.Errorf("absent again: notes %+v, lines %v", st.Notes, lines)
	}
}

// One steward's turn archives a batch, its own session last as "self".
func TestArchiveRequest(t *testing.T) {
	msg := archiveRequest(steward{host: "local_y"}, []string{"local_x", "local_y", "local_z"}, "beekeeper doctor")
	if !strings.Contains(msg, `"local_x", "local_z", "self"`) || !strings.Contains(msg, "archive_session") {
		t.Errorf("request: %q", msg)
	}
}

// agents idle --done marks the work finished; a new assignment clears it.
func TestIdleDone(t *testing.T) {
	a, out := noteApp(t)
	run := func(args ...string) {
		t.Helper()
		c := a.agentsCmd()
		c.SetArgs(args)
		c.SetOut(out)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	run("assign", agentOne, "a task")
	run("idle", "--done")
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || !st.Agents[0].Done || st.Agents[0].Task != "" {
		t.Fatalf("after idle --done: %+v", st.Agents)
	}
	run("assign", agentOne, "another task")
	if st, _ = a.store.Read(); st.Agents[0].Done {
		t.Errorf("an assignment keeps done: %+v", st.Agents[0])
	}
}
