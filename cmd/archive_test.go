package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

var (
	archiveNow = time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)
	// worker is a finished worker off the roster.
	worker = state.Party{Session: "w", Name: "test: finished"}
)

func unarchived(string) (*claude.Record, bool) { return &claude.Record{}, true }

// A worker removed while its CLI runs a turn is owed the archive without a
// try counted; the doctor waits while the turn runs and asks once it ended.
func TestOwedArchiveInATurn(t *testing.T) {
	st := &state.State{}
	lines := owe(st, []archiveOutcome{{agent: worker, line: "its desktop session stays: its CLI 7 is in a turn", host: "local_w"}}, archiveNow)
	if len(st.Archives) != 1 || st.Archives[0].Tries != 0 || !strings.HasSuffix(lines[0], "; the doctor asks again") {
		t.Fatalf("owed %+v, line %q", st.Archives, lines[0])
	}
	inTurn := func(state.Party) bool { return true }
	if p := planArchives(st, unarchived, inTurn, archiveNow.Add(time.Minute)); p[0].wait == "" {
		t.Errorf("in a turn: %s", p[0])
	}
	idle := func(state.Party) bool { return false }
	p := planArchives(st, unarchived, idle, archiveNow.Add(time.Minute))
	if p[0].wait != "" || p[0].drop != "" || !strings.Contains(p[0].String(), "try 1 of 5") {
		t.Errorf("the turn ended: %s", p[0])
	}
	lines = owe(st, []archiveOutcome{{agent: worker, line: "archived its desktop session local_w"}}, archiveNow.Add(time.Minute))
	if len(st.Archives) != 0 || !strings.Contains(lines[0], "owed since") {
		t.Errorf("archived: owed %+v, line %q", st.Archives, lines[0])
	}
}

// A steward that did not record the archive is asked again archiveAgain
// later, and the doctor gives the archive up after archiveTries turns.
func TestOwedArchiveStewardTimeout(t *testing.T) {
	st := &state.State{}
	timeout := archiveOutcome{agent: worker, line: "its desktop session local_w stays: local_s was asked and the desktop did not record it within 45s", host: "local_w", asked: true}
	idle := func(state.Party) bool { return false }
	now := archiveNow
	for try := 1; try < archiveTries; try++ {
		if line := owe(st, []archiveOutcome{timeout}, now)[0]; !strings.HasSuffix(line, "asks again") {
			t.Fatalf("try %d: %q", try, line)
		}
		if st.Archives[0].Tries != try {
			t.Fatalf("try %d counted %d", try, st.Archives[0].Tries)
		}
		if p := planArchives(st, unarchived, idle, now.Add(archiveAgain-time.Second)); p[0].wait == "" {
			t.Errorf("try %d: asked again before archiveAgain: %s", try, p[0])
		}
		now = now.Add(archiveAgain)
		if p := planArchives(st, unarchived, idle, now); p[0].wait != "" || p[0].drop != "" {
			t.Errorf("try %d: not asked again after archiveAgain: %s", try, p[0])
		}
	}
	line := owe(st, []archiveOutcome{timeout}, now)[0]
	if len(st.Archives) != 0 || !strings.Contains(line, "gives it up: 5 stewards' turns") {
		t.Errorf("after %d tries: owed %+v, line %q", archiveTries, st.Archives, line)
	}
}

// An archive is owed no longer once the desktop has it archived or lost
// it, the agent is back on the roster or keeps a role, or archiveOwedFor
// passed; one never to be done is dropped by its outcome.
func TestPlanArchivesDrops(t *testing.T) {
	owed := func(id string, since time.Time) state.Archive {
		return state.Archive{Party: state.Party{Session: id, Name: id}, Host: "local_" + id, Since: since}
	}
	st := &state.State{
		Archives: []state.Archive{owed("put-away", archiveNow), owed("lost", archiveNow), owed("back", archiveNow),
			owed("role", archiveNow), owed("old", archiveNow.Add(-archiveOwedFor)), owed("due", archiveNow)},
		Agents:     []state.Agent{{Party: state.Party{Session: "back"}, Task: "more"}},
		Supervisor: &state.Supervisor{Party: state.Party{Session: "role"}},
	}
	record := func(host string) (*claude.Record, bool) {
		switch host {
		case "local_put-away":
			return &claude.Record{IsArchived: true}, true
		case "local_lost":
			return nil, false
		}
		return &claude.Record{}, true
	}
	want := []string{"has it archived", "no session", "roster again", "role", "owed 24h00m", ""}
	for i, p := range planArchives(st, record, func(state.Party) bool { return false }, archiveNow) {
		if !strings.Contains(p.drop, want[i]) || (want[i] == "") != (p.drop == "") {
			t.Errorf("%s: drop %q, want %q", p.ar.Name, p.drop, want[i])
		}
	}
	st = &state.State{Archives: []state.Archive{owed("w", archiveNow)}}
	owe(st, []archiveOutcome{{agent: state.Party{Session: "w"}, line: "the desktop has its session archived already"}}, archiveNow)
	if len(st.Archives) != 0 {
		t.Errorf("an outcome without a host is owed: %+v", st.Archives)
	}
}

// The doctor seeds, once, the archives of the started workers off the
// roster left unarchived, their CLI running or not; never a roster agent's,
// a role's, an omp agent's or one archived.
func TestSeedArchives(t *testing.T) {
	start := func(id string) state.Start {
		return state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: id}}
	}
	omp := start("omp")
	omp.HostSession, omp.Harness = "omp_omp", "omp"
	run := start("run")
	run.Name = "Supervisor run 7"
	st := &state.State{
		Starts: []state.Start{start("warm"), start("roster"), start("stopped"), start("shelved"), run, omp, start("owed")},
		Agents: []state.Agent{{Party: state.Party{Session: "roster"}}},
	}
	st.Archives = []state.Archive{{Party: state.Party{Session: "owed"}, Host: "local_owed", Tries: 2}}
	record := func(host string) (*claude.Record, bool) {
		return &claude.Record{IsArchived: host == "local_shelved"}, true
	}
	seedArchives(st, record, archiveNow)
	if len(st.Archives) != 3 || st.Archives[1].Host != "local_warm" || st.Archives[2].Host != "local_stopped" || st.Archives[0].Tries != 2 || !st.FinishedSeeded {
		t.Fatalf("seeded %+v", st.Archives)
	}
	st.Archives = nil
	if seedArchives(st, record, archiveNow); len(st.Archives) != 0 {
		t.Errorf("seeded twice: %+v", st.Archives)
	}
}

// A state seeded before the doctor warmed a session's own CLI is seeded
// once more, the archives it gave up included, and a relieved role run's
// with them.
func TestSeedArchivesAgainForTheWarmedSteward(t *testing.T) {
	st := handOverState()
	st.Starts = []state.Start{{Party: supA}, {Party: supB}, {Party: state.Party{Session: "given-up", HostSession: "local_given-up", Name: "given up"}}}
	if _, _, err := relayRole(st, supA, supB, relayNow, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := startRole(st, supB, true, false, relayNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.Archives, st.ArchivesSeeded, st.FinishedSeeded = nil, true, true
	seedArchives(st, unarchived, archiveNow)
	var hosts []string
	for _, ar := range st.Archives {
		hosts = append(hosts, ar.Host)
	}
	if !slices.Equal(hosts, []string{supA.HostSession, "local_given-up"}) || !st.WarmSeeded {
		t.Fatalf("seeded %v", hosts)
	}
	st.Archives = nil
	if seedArchives(st, unarchived, archiveNow); len(st.Archives) != 0 {
		t.Errorf("seeded again: %+v", st.Archives)
	}
}

// A chain of three hand-overs owes the archive of each old session, never
// the follow-up's, so once the desktop records them exactly one row of the
// name is left; a session beekeeper did not start is owed nothing.
func TestHandoverChainOwesEachOldSession(t *testing.T) {
	name := "test: chain"
	start := func(id string, at time.Duration) state.Start {
		return state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: name}, At: archiveNow.Add(at)}
	}
	st := &state.State{
		Starts: []state.Start{start("chain-1", 0), start("chain-2", time.Hour), start("chain-3", 2*time.Hour)},
		Agents: []state.Agent{{Party: state.Party{Session: "chain-3", HostSession: "local_chain-3", Name: name}, Task: "go on"}},
	}
	for _, id := range []string{"chain-1", "chain-2"} {
		if host := oweArchive(st, state.Party{Session: id, Name: name}, "handed over", archiveNow); host != "local_"+id {
			t.Fatalf("%s: owed %q", id, host)
		}
	}
	oweArchive(st, state.Party{Session: "chain-2", Name: name}, "handed over", archiveNow)
	if host := oweArchive(st, state.Party{Session: "person", Name: name}, "handed over", archiveNow); host != "" || len(st.Archives) != 2 {
		t.Fatalf("owed %q, archives %+v", host, st.Archives)
	}
	archived := map[string]bool{}
	record := func(host string) (*claude.Record, bool) { return &claude.Record{IsArchived: archived[host]}, true }
	idle := func(state.Party) bool { return false }
	for _, o := range planArchives(st, record, idle, archiveNow.Add(time.Minute)) {
		if o.wait != "" || o.drop != "" {
			t.Errorf("%s not asked for: %s", o.ar.Host, o)
		}
		archived[o.ar.Host] = true
	}
	for _, o := range planArchives(st, record, idle, archiveNow.Add(2*time.Minute)) {
		if !strings.Contains(o.drop, "has it archived") {
			t.Errorf("%s: %s", o.ar.Host, o)
		}
	}
	var visible []string
	for _, s := range st.Starts {
		if !archived[s.HostSession] {
			visible = append(visible, s.HostSession)
		}
	}
	if len(visible) != 1 || visible[0] != "local_chain-3" {
		t.Errorf("visible rows %v, want only the follow-up's", visible)
	}
}

// A relay's successor taking the role owes the relieved run's archive,
// which the doctor asks for once the run's CLI runs no turn; the holder's
// own session, and a run no relay relieved, stay.
func TestRelievedRunIsArchived(t *testing.T) {
	st := handOverState()
	st.Starts = []state.Start{{Party: supA}, {Party: supB}}
	if _, _, err := relayRole(st, supA, supB, relayNow, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 0 {
		t.Fatalf("owed before the successor took the role: %+v", st.Archives)
	}
	msg, _, err := startRole(st, supB, true, false, relayNow.Add(time.Minute))
	if err != nil || !strings.Contains(msg, "archives its desktop session local_A once its CLI runs no turn") {
		t.Fatalf("start: %q, %v", msg, err)
	}
	if len(st.Archives) != 1 || st.Archives[0].Host != "local_A" {
		t.Fatalf("archives %+v", st.Archives)
	}
	inTurn := func(state.Party) bool { return true }
	if p := planArchives(st, unarchived, inTurn, relayNow.Add(2*time.Minute)); p[0].wait == "" {
		t.Errorf("in a turn: %s", p[0])
	}
	idle := func(state.Party) bool { return false }
	if p := planArchives(st, unarchived, idle, relayNow.Add(2*time.Minute)); p[0].wait != "" || p[0].drop != "" {
		t.Errorf("idle: %s", p[0])
	}
	if !roleKeeps(st, supB) || roleKeeps(st, supA) {
		t.Errorf("roleKeeps: holder %v, relieved %v", roleKeeps(st, supB), roleKeeps(st, supA))
	}
	// The relieved run supervises again: its archive is owed no longer.
	st.Supervisor = &state.Supervisor{Party: supA}
	if p := planArchives(st, unarchived, idle, relayNow.Add(3*time.Minute)); !strings.Contains(p[0].drop, "role") {
		t.Errorf("holding again: %s", p[0])
	}
}
