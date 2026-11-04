package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
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
	finishedID    = "fin"
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
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	busy := state.Party{Session: "busy", HostSession: busyHost, Name: "Board pull 902"}
	run := state.Party{Session: "run9", HostSession: runHost, Name: guideNine}
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
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	a, _ := archiveApp(t, func(st *state.State) { st.Starts = append(st.Starts, state.Start{Party: worker}) })
	desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, finishedID, false)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	o := a.archiveDesktops(context.Background(), st, []state.Party{worker}, "test")[0]
	if !strings.Contains(o.line, "agents.archiveAgreement is not set") || o.host != "" || o.asked || !o.unagreed {
		t.Errorf("outcome %+v", o)
	}
}

// noCLIMachine runs no process: no CLI to steward an archive.
type noCLIMachine struct{ platform.Machine }

func (noCLIMachine) Processes() (*proc.Table, error) {
	return &proc.Table{ByPID: map[int]*proc.Process{}}, nil
}

// Without an archive command, the doctor leaves archives owed and never
// warms a worker or asks a steward, regardless of prior model declines.
func TestArchiveWithoutCommandStaysOwed(t *testing.T) {
	run := state.Party{Session: "run9", HostSession: runHost, Name: guideNine}
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	for _, c := range []struct {
		desc     string
		agents   []state.Party
		declined bool
	}{
		{desc: "a relieved run, then a finished worker", agents: []state.Party{run, worker}},
		{desc: "a relieved run alone", agents: []state.Party{run}},
		{desc: "a finished worker", agents: []state.Party{worker}},
		{desc: "a finished worker that declined", agents: []state.Party{worker}, declined: true},
	} {
		t.Run(c.desc, func(t *testing.T) {
			a, _ := archiveApp(t, func(st *state.State) {
				st.Starts = append(st.Starts, state.Start{Party: run}, state.Start{Party: worker})
				st.Guide = &state.Role{Relieved: []state.Relief{{Party: run, Taken: time.Now()}}}
				if c.declined {
					st.Declines = []state.Decline{{Host: finishedHost, At: time.Now(), Why: "No."}}
				}
			})
			a.cfg.Agents.ArchiveAgreement = "agreed"
			plat.Machine = noCLIMachine{plat.Machine}
			desktopRecord(t, a.cfg.Claude.DesktopDir, runHost, "run9", false)
			desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, finishedID, false)
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			outcomes := a.archiveDesktops(context.Background(), st, c.agents, "test")
			outcomes = slices.DeleteFunc(outcomes, func(o archiveOutcome) bool {
				if !o.agent.Is(run) {
					return false
				}
				if o.asked || o.host != "" || !strings.Contains(o.line, "the person closes it") {
					t.Errorf("relieved run: outcome %+v", o)
				}
				return true
			})
			for _, o := range outcomes {
				if o.asked || o.host == "" || !strings.Contains(o.line, "agents.archiveCommand is not configured") {
					t.Fatalf("%s: outcome %+v", o.agent.Name, o)
				}
			}
			now := a.now
			owe(st, outcomes, now)
			idle := func(state.Party) bool { return false }
			for _, p := range planArchives(st, unarchived, idle, now.Add(archiveAgain-time.Second)) {
				if p.ar.Tries != 0 || !strings.Contains(p.wait, "no archive command ran") {
					t.Errorf("before archiveAgain: %s, tries %d", p, p.ar.Tries)
				}
			}
			for _, p := range planArchives(st, unarchived, idle, now.Add(archiveAgain)) {
				if p.wait != "" || p.drop != "" {
					t.Errorf("after archiveAgain: %s", p)
				}
			}
		})
	}
}
