package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

var archiveNow = time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)

func unarchived(string) (*claude.Record, bool) { return &claude.Record{}, true }

// A worker removed while its CLI runs a turn is owed the archive without a
// try counted; the doctor waits while the turn runs and asks once it ended.
func TestOwedArchiveInATurn(t *testing.T) {
	st := &state.State{}
	worker := state.Party{Session: "w", Name: "worker"}
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
	worker := state.Party{Session: "w", Name: "worker"}
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
		Archives: []state.Archive{owed("archived", archiveNow), owed("lost", archiveNow), owed("back", archiveNow),
			owed("role", archiveNow), owed("old", archiveNow.Add(-archiveOwedFor)), owed("due", archiveNow)},
		Agents:     []state.Agent{{Party: state.Party{Session: "back"}, Task: "more"}},
		Supervisor: &state.Supervisor{Party: state.Party{Session: "role"}},
	}
	record := func(host string) (*claude.Record, bool) {
		switch host {
		case "local_archived":
			return &claude.Record{IsArchived: true}, true
		case "local_lost":
			return nil, false
		}
		return &claude.Record{}, true
	}
	want := []string{"archived", "no session", "roster again", "role", "owed 24h00m", ""}
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
// roster whose CLI runs on unarchived; never a roster agent's, a role's,
// an omp agent's, one whose CLI stopped or one archived.
func TestSeedArchives(t *testing.T) {
	start := func(id string) state.Start {
		return state.Start{Party: state.Party{Session: id, HostSession: "local_" + id, Name: id}}
	}
	omp := start("omp")
	omp.HostSession, omp.Harness = "omp_omp", "omp"
	run := start("run")
	run.Name = "Supervisor run 7"
	st := &state.State{
		Starts: []state.Start{start("warm"), start("roster"), start("stopped"), start("archived"), run, omp, start("owed")},
		Agents: []state.Agent{{Party: state.Party{Session: "roster"}}},
	}
	st.Archives = []state.Archive{{Party: state.Party{Session: "owed"}, Host: "local_owed", Tries: 2}}
	var sessions []*claude.Session
	for _, id := range []string{"warm", "roster", "archived", "run", "omp", "owed"} {
		sessions = append(sessions, &claude.Session{ID: id, HostID: "local_" + id})
	}
	record := func(host string) (*claude.Record, bool) {
		return &claude.Record{IsArchived: host == "local_archived"}, true
	}
	seedArchives(st, sessions, record, archiveNow)
	if len(st.Archives) != 2 || st.Archives[1].Host != "local_warm" || st.Archives[0].Tries != 2 || !st.ArchivesSeeded {
		t.Fatalf("seeded %+v", st.Archives)
	}
	st.Archives = nil
	if seedArchives(st, sessions, record, archiveNow); len(st.Archives) != 0 {
		t.Errorf("seeded twice: %+v", st.Archives)
	}
}
