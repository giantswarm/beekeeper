package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
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

const busyWord = "busy"

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
	busy := state.Party{Session: busyWord, HostSession: busyHost, Name: "Board pull 902"}
	run := state.Party{Session: "run9", HostSession: runHost, Name: guideNine}
	a, out := archiveApp(t, func(st *state.State) {
		st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: worker, Finished: true, At: time.Now()})
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

// What a steward did is read from its turn: an archive call names it,
// words without one are a decline, nothing is no answer.
func TestStewardAnswers(t *testing.T) {
	call := func(id string, failed bool) claude.AnswerCall {
		in, _ := json.Marshal(map[string]string{executionOmpSession: id})
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

// noCLIMachine runs no process: no CLI to steward an archive.
type noCLIMachine struct{ platform.Machine }

func (noCLIMachine) Processes() (*proc.Table, error) {
	return &proc.Table{ByPID: map[int]*proc.Process{}}, nil
}

// The doctor asks no session about its own archive while it declined, for
// a day. Its own CLI is warmed for a finished worker; a relieved run of the
// guide is never archived: its session stays the person's to close
// (https://github.com/giantswarm/beekeeper/issues/2195). A run that asked no steward
// counts no try and leaves the archive archiveAgain, so the doctor does not
// ask again on every run.
func TestArchiveAsksNoSessionAboutItselfAgain(t *testing.T) {
	run := state.Party{Session: "run9", HostSession: runHost, Name: guideNine}
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	for _, c := range []struct {
		desc     string
		agents   []state.Party
		declined bool
		warmed   string // the host whose CLI the doctor would warm, "" for none
	}{
		{desc: "a relieved run, then a finished worker", agents: []state.Party{run, worker}, warmed: finishedHost},
		{desc: "a relieved run alone", agents: []state.Party{run}},
		{desc: "a finished worker", agents: []state.Party{worker}, warmed: finishedHost},
		{desc: "a finished worker that declined", agents: []state.Party{worker}, declined: true},
	} {
		t.Run(c.desc, func(t *testing.T) {
			a, _ := archiveApp(t, func(st *state.State) {
				st.Starts = append(st.Starts, state.Start{Party: run}, state.Start{Party: worker})
				st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: worker, Finished: true, At: time.Now()})
				st.Guide = &state.Role{Relieved: []state.Relief{{Party: run, Taken: time.Now()}}}
				if c.declined {
					st.Declines = []state.Decline{{Host: finishedHost, At: time.Now(), Why: "No."}}
				}
			})
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
				if o.asked || o.host == "" || !strings.Contains(o.line, errNoSteward.Error()) {
					t.Fatalf("%s: outcome %+v", o.agent.Name, o)
				}
				for _, h := range []string{runHost, finishedHost} {
					if warmed := strings.Contains(o.line, "warm the CLI of "+h); warmed != (h == c.warmed) {
						t.Errorf("%s: warmed %s %v: %q", o.agent.Name, h, warmed, o.line)
					}
				}
			}
			now := a.now
			owe(st, outcomes, now)
			idle := func(state.Party) bool { return false }
			for _, p := range planArchives(st, unarchived, idle, now.Add(archiveAgain-time.Second)) {
				if p.ar.Tries != 0 || !strings.Contains(p.wait, "no steward was asked") {
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

// Finished workers are archived through a steward with the default config.
func TestArchiveFinishedWorkerWithoutConfiguration(t *testing.T) {
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	a, _ := archiveApp(t, func(st *state.State) {
		st.Starts = append(st.Starts, state.Start{Party: worker})
		st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: worker, Finished: true, At: time.Now()})
	})
	desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, finishedID, false)
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if err := os.MkdirAll(filepath.Join(runtime, "cc-socks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peerSocket(runtime, 7), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	warm := &atomic.Bool{}
	warm.Store(true)
	plat.Machine = warmMachine{Machine: plat.Machine, id: finishedID, warm: warm}
	sent := fakeRelay(t, false)
	// The fake steward records the archive before acknowledging the request.
	script := filepath.Join(filepath.Dir(sent), "claude")
	raw, err := os.ReadFile(filepath.Clean(script))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_ARCHIVE_RECORD", filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", finishedHost+".json"))
	raw = []byte(strings.Replace(string(raw), "#!/bin/sh\n", "#!/bin/sh\nsed -i 's/\"isArchived\":false/\"isArchived\":true/' \"$TEST_ARCHIVE_RECORD\"\n", 1))
	if err := os.WriteFile(script, raw, 0o700); err != nil { //nolint:gosec // executable fixture in a test-owned directory
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome := a.archiveDesktops(ctx, st, []state.Party{worker}, "beekeeper doctor")[0]
	record, ok := claude.ReadRecord(a.cfg, finishedHost)
	if !ok || !record.IsArchived || outcome.host != "" || !strings.Contains(outcome.line, "is archived") {
		t.Fatalf("outcome %+v, record %+v", outcome, record)
	}
	if got := relayed(t, sent); len(got) != 1 || got[0] != "uds:"+peerSocket(runtime, 7) {
		t.Fatalf("steward deliveries %v", got)
	}
}

func TestArchivableRequiresCompletion(t *testing.T) {
	p := state.Party{Session: workerRole, HostSession: litLocalWorker, Name: litWorker}
	for _, kind := range []string{"missing report", "idle report", "done report", "open task record", "new assignment"} {
		t.Run(kind, func(t *testing.T) {
			a, _ := archiveApp(t, func(st *state.State) {
				st.Starts = append(st.Starts, state.Start{Party: p})
				if kind != "missing report" {
					st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: p, Finished: kind != "idle report", At: time.Now().Add(-time.Hour)})
				}
				if kind == "open task record" {
					st.Records = append(st.Records, state.Record{Session: p, Issue: "issue", At: time.Now()})
				}
				if kind == "new assignment" {
					st.Starts[len(st.Starts)-1].At = time.Now()
				}
			})
			desktopRecord(t, a.cfg.Claude.DesktopDir, p.HostSession, p.Session, false)
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			_, why := a.archivableHost(st, p.HostSession, state.Party{})
			if (why == "") != (kind == "done report") {
				t.Fatalf("archivable reason = %q", why)
			}
		})
	}
}

// An old roster report cannot make a live worker with uncommitted rework
// finished, even when that worker asks about archiving its own session.
func TestArchivableLiveTurnDirtyWorktree(t *testing.T) {
	a, _ := archiveApp(t, func(*state.State) {})
	dir := t.TempDir()
	gitT(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "rework.go"), []byte("package rework\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dir, "status", "--porcelain"); got == "" {
		t.Fatal("expected dirty worktree")
	}
	plat.Machine = headlessMachine{Machine: plat.Machine, id: "s42"}
	p := state.Party{Session: "s42", HostSession: "local_worker", Name: litWorker}
	desktopRecord(t, a.cfg.Claude.DesktopDir, p.HostSession, p.Session, false)
	// A headless turn remains live independently of the old last report.
	ss, table, err := a.sessions()
	if err != nil || len(ss) != 1 {
		t.Fatalf("sessions: %v, %v", ss, err)
	}
	ss[0].Cwd = dir
	busy := func(p state.Party) bool { _, ok := turnRunning(ss, table, p, a.now); return ok }
	if !busy(p) {
		t.Fatal("expected live turn")
	}
	st := &state.State{Starts: []state.Start{{Party: p}}, Agents: []state.Agent{{Party: p, Task: "finish rework", ActivityAt: a.now.Add(-48 * time.Hour), Registered: a.now.Add(-48 * time.Hour)}}, WorkerReports: []state.WorkerReport{{By: p, At: a.now.Add(-48 * time.Hour)}}}
	if chores := planChores(st, ss, unarchived, busy, 24*time.Hour, a.now, a.cfg); len(chores) != 0 {
		t.Fatalf("busy worker chores: %v", chores)
	}
	if _, why := a.archivableHost(st, p.HostSession, p); why == "" {
		t.Fatal("busy roster worker is archivable")
	}
	st.Agents = nil
	if _, why := a.archivableHost(st, p.HostSession, p); why == "" {
		t.Fatal("worker without done report is archivable off roster")
	}
	st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: p, Finished: true, At: a.now})
	if _, why := a.archivableHost(st, p.HostSession, p); !strings.Contains(why, "in a turn") {
		t.Fatalf("live caller with previous completion: %q", why)
	}
}

func TestArchivableCompletionAuditHistory(t *testing.T) {
	p := state.Party{Session: "worker", HostSession: "local_worker", Name: litWorker}
	a, _ := archiveApp(t, func(st *state.State) { st.Starts = append(st.Starts, state.Start{Party: p}) })
	desktopRecord(t, a.cfg.Claude.DesktopDir, p.HostSession, p.Session, false)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, why := a.archivable(st, p); !strings.Contains(why, "has not reported") {
		t.Fatalf("archive without completion: %q", why)
	}
	for i, verb := range []string{"agents.done", "agents.idle"} {
		if err := a.store.Record(state.Event{By: p, At: a.now.Add(time.Duration(i) * time.Second), Verb: verb}); err != nil {
			t.Fatal(err)
		}
		_, why := a.archivableHost(st, p.HostSession, state.Party{})
		if (why == "") != (verb == "agents.done") {
			t.Fatalf("after %s: %q", verb, why)
		}
	}
}
