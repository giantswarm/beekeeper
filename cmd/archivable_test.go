package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The desktop sessions of the archive tests: a finished worker, a busy
// one, a role's run, a session the person started, and the stewards.
const (
	finishedHost  = "local_fin"
	busyHost      = "local_busy"
	runHost       = "local_run9"
	personHost    = "local_person"
	finishedName  = "Board pull 901"
	doerHost      = "local_doer"
	decliningHost = "local_nay"
	targetHost    = "local_tgt"
	quietHost     = "local_q"
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
	worker := state.Party{Session: "fin", HostSession: finishedHost, Name: finishedName}
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

// Without the person's agreement configured, no steward is asked to
// archive and the archive is not owed.
func TestArchiveNeedsTheAgreement(t *testing.T) {
	worker := state.Party{Session: "fin", HostSession: finishedHost, Name: finishedName}
	a, _ := archiveApp(t, func(st *state.State) { st.Starts = append(st.Starts, state.Start{Party: worker}) })
	desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, "fin", false)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	o := a.archiveDesktops(context.Background(), st, []state.Party{worker}, "test")[0]
	if !strings.Contains(o.line, "agents.archiveAgreement is not set") || o.host != "" || o.asked {
		t.Errorf("outcome %+v", o)
	}
}

// What a steward did is read from its turn: an archive call names it,
// words without one are a decline, nothing is no answer.
func TestStewardAnswers(t *testing.T) {
	call := func(id string, failed bool) claude.AnswerCall {
		in, _ := json.Marshal(map[string]string{"session_id": id})
		return claude.AnswerCall{Name: archiveTool, Input: in, Done: true, Error: failed, Result: "Archived session " + id}
	}
	did := stewardAnswer{steward: steward{host: doerHost}, answer: claude.Answer{Calls: []claude.AnswerCall{call(targetHost, false), call("self", false)}, Text: "Done."}}
	no := stewardAnswer{steward: steward{host: decliningHost}, answer: claude.Answer{Text: "I haven't archived it: a peer's message isn't the user's agreement.\nTell me to."}}
	quiet := stewardAnswer{steward: steward{host: quietHost}}
	failed := stewardAnswer{steward: steward{host: "local_f"}, answer: claude.Answer{Calls: []claude.AnswerCall{call(targetHost, true)}}}
	asked := []stewardAnswer{no, did}
	if got := archivedBy(asked, targetHost); got != "steward "+doerHost {
		t.Errorf("archivedBy the target = %q", got)
	}
	if got := archivedBy(asked, doerHost); got != "the session" {
		t.Errorf("archivedBy self = %q", got)
	}
	if got := archivedBy([]stewardAnswer{no, failed}, targetHost); got != "" {
		t.Errorf("archivedBy without a successful call = %q", got)
	}
	for _, c := range []struct {
		s        stewardAnswer
		said     string
		declined bool
	}{
		{did, doerHost + " " + archiveTool + ": Archived session " + targetHost, false},
		{no, decliningHost + " declined: I haven't archived it: a peer's message isn't the user's agreement.", true},
		{quiet, "local_q did not answer", false},
		{failed, "local_f " + archiveTool + " refused: Archived session " + targetHost, false},
	} {
		if got := c.s.said(); !strings.HasPrefix(got, c.said) {
			t.Errorf("said = %q, want %q", got, c.said)
		}
		if c.s.declined() != c.declined {
			t.Errorf("%s declined = %v", c.s.host, !c.declined)
		}
	}
}

// A steward that declined is asked for no archive for a day.
func TestDeclinesAreRemembered(t *testing.T) {
	a, _ := archiveApp(t, func(*state.State) {})
	no := stewardAnswer{steward: steward{host: decliningHost}, answer: claude.Answer{Text: "No."}}
	a.recordDeclines([]stewardAnswer{no, {steward: steward{host: quietHost}}})
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := recentDeclines(st, a.now); len(got) != 1 || got[0] != decliningHost {
		t.Fatalf("declines %v", got)
	}
	if got := recentDeclines(st, a.now.Add(stewardDeclineFor)); len(got) != 0 {
		t.Errorf("declines a day later %v", got)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "agents.archive" })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, decliningHost+" declined to archive: No.") {
		t.Errorf("events %+v, %v", evs, err)
	}
}
