package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The desktop sessions of the archive tests: a finished worker, a busy
// one, a role's run and a session the person started.
const (
	finishedHost = "local_fin"
	busyHost     = "local_busy"
	runHost      = "local_run9"
	personHost   = "local_person"
	finishedName = "Board pull 901"
	finishedID   = "fin"
)

// archiveApp is a stub app with a desktop directory and the state st.
func archiveApp(t *testing.T, st func(*state.State)) (*app, *strings.Builder) {
	t.Helper()
	a, _ := stubApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	a.now = time.Now()
	if err := a.store.Update(func(s *state.State) ([]state.Event, error) { st(s); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	a.out = &out
	return a, &out
}

// agents archivable confirms a finished worker beekeeper started and
// refuses a session the person started, a roster agent and a role's run.
func TestArchivable(t *testing.T) {
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	busy := state.Party{Session: "busy", HostSession: busyHost, Name: "Board pull 902"}
	run := state.Party{Session: "run9", HostSession: runHost, Name: "Supervisor run 9"}
	a, out := archiveApp(t, func(st *state.State) {
		for _, p := range []state.Party{worker, busy, run} {
			st.Starts = append(st.Starts, state.Start{Party: p})
		}
		st.Agents = append(st.Agents, state.Agent{Party: busy, Task: "a task"})
	})
	for _, h := range []string{finishedHost, busyHost, runHost, personHost} {
		desktopRecord(t, a.cfg.Claude.DesktopDir, h, strings.TrimPrefix(h, "local_"), false)
	}
	archivable := func(hosts ...string) error {
		c := a.agentsCmd()
		c.SetArgs(append([]string{"archivable"}, hosts...))
		c.SetOut(out)
		c.SetErr(out)
		return c.Execute()
	}
	if err := archivable(finishedHost); err != nil || !strings.Contains(out.String(), finishedHost+`: archivable: "`+finishedName+`"`) {
		t.Fatalf("a finished worker: %v\n%s", err, out)
	}
	out.Reset()
	err := archivable(finishedHost, personHost, busyHost, runHost)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitRefused {
		t.Fatalf("exit %v", err)
	}
	for _, want := range []string{personHost + ": not archivable: beekeeper did not start it", busyHost + ": not archivable: it is on the roster", runHost + ": not archivable: its desktop session stays: it holds or held"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

// A removed worker's archive is owed and asks no session: the doctor puts
// it to the person.
func TestArchiveDesktopsAsksNoSession(t *testing.T) {
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	a, _ := archiveApp(t, func(st *state.State) { st.Starts = append(st.Starts, state.Start{Party: worker}) })
	desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, finishedID, false)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	o := a.archiveDesktops(context.Background(), st, []state.Party{worker})[0]
	if o.host != finishedHost || !strings.Contains(o.line, "the doctor asks the person through the guide") {
		t.Errorf("outcome %+v", o)
	}
}

// The archives owed reach the person as one decision for the guide, the
// sessions listed with the command that confirms them and the archive
// tool; a second run files none beside it, and an archive owed meanwhile
// waits for the next. On the person's yes the guide's session archives
// them: the doctor drops each once the desktop records it and then closes
// the decision. On a no the decision closes with the sessions unarchived:
// the doctor owes them no longer. Without guide.person no decision is
// filed.
func TestArchiveDecision(t *testing.T) {
	const person = "Timo"
	other := state.Party{Session: "fin2", HostSession: "local_fin2", Name: "Board pull 903"}
	late := state.Party{Session: "fin3", HostSession: "local_fin3", Name: "Board pull 904"}
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	owed := func(p state.Party) state.Archive {
		return state.Archive{Party: p, Host: p.HostSession, Since: time.Now(), Why: "a finished worker left unarchived"}
	}
	setup := func(t *testing.T) (*app, func(...state.Party) ([]string, []state.Event, *state.State)) {
		a, _ := archiveApp(t, func(st *state.State) {
			for _, p := range []state.Party{worker, other, late} {
				st.Starts = append(st.Starts, state.Start{Party: p})
			}
			st.Archives = []state.Archive{owed(worker), owed(other)}
		})
		for _, p := range []state.Party{worker, other, late} {
			desktopRecord(t, a.cfg.Claude.DesktopDir, p.HostSession, p.Session, false)
		}
		ask := func(ask ...state.Party) (lines []string, evs []state.Event, st *state.State) {
			err := a.store.Update(func(s *state.State) ([]state.Event, error) {
				lines, evs = a.askArchives(s, ask, person, watchParty, a.now)
				st = s
				return evs, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return lines, evs, st
		}
		return a, ask
	}
	record := func(a *app) func(string) (*claude.Record, bool) {
		return func(h string) (*claude.Record, bool) { return claude.ReadRecord(a.cfg, h) }
	}
	idle := func(state.Party) bool { return false }

	a, ask := setup(t)
	lines, _, st := ask(worker, other)
	if len(st.Notes) != 1 || len(lines) != 1 {
		t.Fatalf("notes %+v, lines %v", st.Notes, lines)
	}
	n := st.Notes[0]
	if n.For != person || n.Kind != noteDecision || !isArchiveDecision(n) || n.Recommend != 1 || len(n.Options) != 2 {
		t.Errorf("decision %+v", n)
	}
	for _, want := range []string{finishedHost, "local_fin2", "beekeeper agents archivable " + finishedHost + " local_fin2", archiveTool, fmt.Sprintf("beekeeper note done %d", n.ID)} {
		if !strings.Contains(n.Text, want) {
			t.Errorf("decision lacks %q: %s", want, n.Text)
		}
	}
	for _, ar := range st.Archives {
		if ar.Asked != n.ID {
			t.Errorf("%s asked %d, want %d", ar.Host, ar.Asked, n.ID)
		}
	}
	for _, p := range planArchives(st, record(a), idle, a.now.Add(2*archiveOwedFor)) {
		if !strings.Contains(p.wait, fmt.Sprintf("note #%d asks the person", n.ID)) {
			t.Errorf("while asked: %s", p)
		}
	}
	if err := a.store.Update(func(s *state.State) ([]state.Event, error) {
		s.Archives = append(s.Archives, owed(late))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if due := a.archiveDecisionDue(st, []state.Party{late}, person); due {
		t.Error("a decision is due beside the open one")
	}
	if _, _, st = ask(late); len(st.Notes) != 1 || st.Archives[2].Asked != 0 {
		t.Errorf("a second decision beside the open one: notes %+v, archives %+v", st.Notes, st.Archives)
	}

	// Yes: the guide's session archived both, the desktop records them.
	desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, finishedID, true)
	desktopRecord(t, a.cfg.Claude.DesktopDir, "local_fin2", "fin2", true)
	if _, err := a.owedArchives(nil, record(a), idle, watchParty); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !a.archiveDecisionDue(st, []state.Party{late}, person) {
		t.Error("no decision due once the desktop recorded the archives")
	}
	lines, evs, st := ask(late)
	if len(st.Archives) != 1 || st.Archives[0].Host != "local_fin3" {
		t.Fatalf("archives %+v", st.Archives)
	}
	if len(evs) != 2 || evs[0].Verb != noteOvertaken || len(st.Notes) != 1 || st.Notes[0].ID == n.ID || st.Archives[0].Asked != st.Notes[0].ID {
		t.Errorf("the next decision: lines %v, events %+v, notes %+v", lines, evs, st.Notes)
	}

	// No: the decision closes with its session unarchived.
	next := st.Notes[0].ID
	if err := a.store.Update(func(s *state.State) ([]state.Event, error) { s.Notes = nil; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if p := planArchives(st, record(a), idle, a.now); len(p) != 1 || !strings.Contains(p[0].drop, fmt.Sprintf("note #%d, closed with it unarchived", next)) {
		t.Errorf("kept: %v", p)
	}

	// No person to ask: no decision due, the archives counted.
	a, _ = setup(t)
	if st, err = a.store.Read(); err != nil {
		t.Fatal(err)
	}
	both := []state.Party{worker, other}
	if n := len(a.askable(st, both)); n != 2 || a.archiveDecisionDue(st, both, "") {
		t.Errorf("no person: %d askable, due %v", n, a.archiveDecisionDue(st, both, ""))
	}
	if lines, _ := a.askArchives(st, both, "", watchParty, a.now); len(lines) != 0 || len(st.Notes) != 0 {
		t.Errorf("no person: lines %v, notes %+v", lines, st.Notes)
	}
}
