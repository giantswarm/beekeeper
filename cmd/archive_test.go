package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

const retryHostSession = "local_retry"

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
		Agents: []state.Agent{{Party: state.Party{Session: "back"}, Task: "more"}},
		Guide:  &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "role"}}},
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
	run := start(testRunWord)
	run.Name = "Guide run 7"
	st := &state.State{
		Starts: []state.Start{start("warm"), start("roster"), start("stopped"), start("shelved"), run, omp, start("owed")},
		Agents: []state.Agent{{Party: state.Party{Session: "roster"}}},
	}
	st.Archives = []state.Archive{{Party: state.Party{Session: "owed"}, Host: "local_owed", Tries: 2}}
	record := func(host string) (*claude.Record, bool) {
		return &claude.Record{IsArchived: host == "local_shelved"}, true
	}
	seedArchives(st, record, nil, archiveNow)
	if len(st.Archives) != 3 || st.Archives[1].Host != "local_warm" || st.Archives[2].Host != "local_stopped" || st.Archives[0].Tries != 2 || !st.FinishedSeeded {
		t.Fatalf("seeded %+v", st.Archives)
	}
	st.Archives = nil
	if seedArchives(st, record, nil, archiveNow); len(st.Archives) != 0 {
		t.Errorf("seeded twice: %+v", st.Archives)
	}
}

// A state seeded before the doctor warmed a session's own CLI is seeded
// once more, with only the starts reseed takes (a relieved role run's, a
// worker's that reported done) and never a session someone brought back
// from the Archived list; each is asked for once its session stayed quiet
// a day.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestSeedArchivesAgainOnlyWhatReseedTakes(t *testing.T) {
	st := handOverState()
	done := state.Party{Session: "w-done", HostSession: "local_w-done", Name: "reported done"}
	back := state.Party{Session: "w-back", HostSession: "local_w-back", Name: "brought back"}
	stale := state.Party{Session: kindStale, HostSession: "local_stale", Name: "left stale"}
	st.Starts = []state.Start{{Party: supA}, {Party: supB}, {Party: done}, {Party: back}, {Party: stale}}
	if _, _, err := guideRole.Relay(st, supA, supB, relayNow, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := guideRole.Start(st, supB, true, false, relayNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.Archives, st.ArchivesSeeded, st.FinishedSeeded = nil, true, true
	active := archiveNow.Add(-time.Hour)
	record := func(host string) (*claude.Record, bool) {
		return &claude.Record{AutoArchiveExempt: host == back.HostSession}, true
	}
	reseed := func(st *state.State, s state.Start) (time.Time, bool) {
		return active, s.Session != stale.Session && (s.Session != supA.Session || relieved(st, s.Party))
	}
	seedArchives(st, record, reseed, archiveNow)
	var hosts []string
	for _, ar := range st.Archives {
		hosts = append(hosts, ar.Host)
	}
	// The relieved run supA stays the person's: never seeded.
	if !slices.Equal(hosts, []string{done.HostSession}) || !st.DoneSeeded {
		t.Fatalf("seeded %v", hosts)
	}
	idle := func(state.Party) bool { return false }
	if p := planArchives(st, record, idle, archiveNow.Add(time.Hour)); !strings.Contains(p[0].wait, "quiet a day") {
		t.Errorf("an hour on: %s", p[0])
	}
	if p := planArchives(st, record, idle, active.Add(reseedQuiet)); p[0].wait != "" || p[0].drop != "" {
		t.Errorf("a quiet day on: %s", p[0])
	}
	shown := func(string) (*claude.Record, bool) {
		return &claude.Record{LastFocusedAt: active.Add(reseedQuiet).UnixMilli()}, true
	}
	if p := planArchives(st, shown, idle, active.Add(reseedQuiet+time.Hour)); !strings.Contains(p[0].wait, "quiet a day") {
		t.Errorf("shown meanwhile: %s", p[0])
	}
	st.Archives = nil
	if seedArchives(st, record, reseed, archiveNow); len(st.Archives) != 0 {
		t.Errorf("seeded again: %+v", st.Archives)
	}
}

// The reseed takes a session that reported done or a relieved role run,
// whose CLI does not run and that no person typed in.
func TestReseedable(t *testing.T) {
	const running = "r-live"
	dir := t.TempDir()
	write := func(id string, lines ...string) {
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	brief := `{"type":"user","entrypoint":"sdk-cli","message":{"role":"user","content":"the brief"}}`
	peer := `{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":"Another Claude session sent a message: go"}}`
	typed := `{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":[{"type":"text","text":"merge it"}]}}`
	write("r-done", brief, peer)
	write("r-typed", brief, typed)
	write(running, brief)
	write("r-undone", brief)
	transcript := func(id string) (string, bool) {
		p := filepath.Join(dir, id+".jsonl")
		_, err := os.Stat(p)
		return p, err == nil
	}
	done := map[string]bool{"r-done": true, "r-typed": true, running: true, "r-missing": true}
	live := func(p state.Party) bool { return p.Session == running }
	for id, want := range map[string]bool{"r-done": true, "r-typed": false, running: false, "r-undone": false, "r-missing": false} {
		s := state.Start{Party: state.Party{Session: id, Name: id}}
		if _, got := reseedable(&state.State{}, s, done, live, transcript); got != want {
			t.Errorf("%s: reseedable %v, want %v", id, got, want)
		}
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
	if host := oweArchive(st, state.Party{Session: holderPerson, Name: name}, "handed over", archiveNow); host != "" || len(st.Archives) != 2 {
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

// A relay's successor taking the role owes no archive of the relieved run:
// its session stays the person's to close, and the start message says how.
// An archive owed before the fix is owed no longer; the doctor neither
// seeds nor reseeds one (https://github.com/giantswarm/beekeeper/issues/2195).
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestRelievedRunStaysThePersons(t *testing.T) {
	st := handOverState()
	st.Starts = []state.Start{{Party: supA, Role: guideName}, {Party: supB}}
	if _, _, err := guideRole.Relay(st, supA, supB, relayNow, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	msg, _, err := guideRole.Start(st, supB, true, false, relayNow.Add(time.Minute))
	if err != nil || !strings.Contains(msg, "stays in the person's session list until they archive it") {
		t.Fatalf("start: %q, %v", msg, err)
	}
	if len(st.Archives) != 0 {
		t.Fatalf("archives %+v, want none owed", st.Archives)
	}
	if st.Starts[1].Role != guideName {
		t.Errorf("the successor's start has role %q, want %q", st.Starts[1].Role, guideName)
	}
	if !keepsRole(st, supB) || !keepsRole(st, supA) {
		t.Errorf("keepsRole: holder %v, relieved %v", keepsRole(st, supB), keepsRole(st, supA))
	}
	// Once its relief is forgotten, its start still keeps it.
	st.Guide.Relieved = nil
	if !keepsRole(st, supA) {
		t.Error("keepsRole: relieved run whose relief was dropped")
	}
	// An archive owed by an earlier version is dropped, never asked for.
	st.Archives = []state.Archive{{Party: supA, Host: "local_A", Why: "owed on a relief"}}
	idle := func(state.Party) bool { return false }
	if p := planArchives(st, unarchived, idle, relayNow.Add(3*time.Minute)); !strings.Contains(p[0].drop, "the person closes it") {
		t.Errorf("owed: %s", p[0])
	}
	st.Archives, st.FinishedSeeded, st.DoneSeeded = nil, false, false
	seedArchives(st, unarchived, func(*state.State, state.Start) (time.Time, bool) { return time.Time{}, true }, relayNow)
	if slices.ContainsFunc(st.Archives, func(ar state.Archive) bool { return ar.Is(supA) }) {
		t.Errorf("seeded %+v", st.Archives)
	}
}

// A guide taken over after a crash or a reboot, its session gone without a
// relay, stays the person's too: its start took the role.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestTakenOverGuideStaysThePersons(t *testing.T) {
	// Names that are no role's runs: only the start says they guided.
	old := state.Party{Session: "gA", HostSession: "local_gA", Name: "desk guide"}
	next := state.Party{Session: "gB", HostSession: "local_gB", Name: "desk guide again"}
	st := &state.State{Starts: []state.Start{{Party: old}, {Party: next}}}
	if keepsRole(st, old) {
		t.Fatal("keepsRole before it guided")
	}
	if _, _, err := guideRole.Start(st, old, false, false, relayNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := guideRole.Start(st, next, false, false, relayNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if holdsRole(st, old) || !keepsRole(st, old) {
		t.Errorf("taken over: holds %v, keeps %v", holdsRole(st, old), keepsRole(st, old))
	}
	if _, why := (&app{}).archivableRecord(st, old); !strings.Contains(why, "the person closes it") {
		t.Errorf("archivable: %q", why)
	}
}

func TestRetryGivenUpArchive(t *testing.T) {
	for _, id := range []string{"w", retryHostSession} {
		t.Run(id, func(t *testing.T) {
			st := &state.State{FinishedSeeded: true, DoneSeeded: true, Starts: []state.Start{{Party: worker}}}
			st.Starts[0].HostSession = retryHostSession
			failed := archiveOutcome{agent: worker, host: retryHostSession, asked: true, line: "not archived"}
			now := archiveNow
			for cycle := 0; cycle < 2; cycle++ {
				if err := retryArchive(st, id, unarchived, now); err != nil {
					t.Fatal(err)
				}
				ar := st.Archives[0]
				if len(st.Archives) != 1 || ar.Tries != 0 || !ar.Tried.IsZero() || !ar.Looked.IsZero() || !ar.Warmed.IsZero() || !ar.Since.Equal(now) {
					t.Fatalf("retry did not reset budget: %+v", ar)
				}
				plan := planArchives(st, unarchived, func(state.Party) bool { return false }, now)
				if len(plan) != 1 || plan[0].wait != "" || plan[0].drop != "" {
					t.Fatalf("retry not due: %+v", plan)
				}
				for try := 0; try < archiveTries; try++ {
					line := owe(st, []archiveOutcome{failed}, now)[0]
					if try == archiveTries-1 && !strings.Contains(line, "gives it up: 5 stewards' turns") {
						t.Fatal(line)
					}
					now = now.Add(archiveAgain)
				}
				if len(st.Archives) != 0 {
					t.Fatal("not given up")
				}
			}
			if err := retryArchive(st, id, unarchived, now); err != nil {
				t.Fatal(err)
			}
			owe(st, []archiveOutcome{{agent: worker, line: "archive done"}}, now)
			if len(st.Archives) != 0 {
				t.Fatal("successful retry still owed")
			}
		})
	}
}

func TestRetryArchiveSafety(t *testing.T) {
	for _, kind := range []string{"unknown start", "on roster", "keeps role", "other harness", "no desktop", "missing desktop", "already archived"} {
		t.Run(kind, func(t *testing.T) {
			st := &state.State{Starts: []state.Start{{Party: worker}}}
			st.Starts[0].HostSession = retryHostSession
			record := unarchived
			switch kind {
			case "unknown start":
				st.Starts = nil
			case "on roster":
				st.Agents = []state.Agent{{Party: worker}}
			case "keeps role":
				st.Guide = &state.Role{Holder: &state.Supervisor{Party: worker}}
			case "other harness":
				st.Starts[0].Harness = transcriptTestCodex
			case "no desktop":
				st.Starts[0].HostSession = ""
			case "missing desktop":
				record = func(string) (*claude.Record, bool) { return nil, false }
			case "already archived":
				record = func(string) (*claude.Record, bool) { return &claude.Record{IsArchived: true}, true }
			}
			if err := retryArchive(st, worker.Session, record, archiveNow); err == nil {
				t.Fatal("unsafe retry accepted")
			}
			if len(st.Archives) != 0 {
				t.Fatal("rejected retry changed archives")
			}
		})
	}
}

func TestRetryArchiveResetsExistingBudget(t *testing.T) {
	st := &state.State{Starts: []state.Start{{Party: worker}}, Archives: []state.Archive{{Party: worker, Host: retryHostSession, Since: archiveNow.Add(-48 * time.Hour), Tries: 4, Tried: archiveNow, Looked: archiveNow, Warmed: archiveNow}}}
	st.Starts[0].HostSession = retryHostSession
	if err := retryArchive(st, worker.Session, unarchived, archiveNow); err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 1 || st.Archives[0].Tries != 0 || !st.Archives[0].Tried.IsZero() || !st.Archives[0].Looked.IsZero() || !st.Archives[0].Warmed.IsZero() {
		t.Fatalf("budget: %+v", st.Archives)
	}
}

func TestDoctorRetryArchiveCommand(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	writeT(t, filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", "local_retry.json"), `{}`)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		p := worker
		p.HostSession = retryHostSession
		st.Starts = append(st.Starts, state.Start{Party: p})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cmd := a.doctorCmd()
	cmd.SetArgs([]string{"retry-archive", retryHostSession})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Archives) != 1 || st.Archives[0].Host != retryHostSession || st.Archives[0].Tries != 0 {
		t.Fatalf("queued: %+v", st.Archives)
	}
	if !strings.Contains(out.String(), "archive retry queued") {
		t.Fatal(out.String())
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == orphanArchiveVerb })
	if err != nil || len(evs) != 1 {
		t.Fatalf("events: %+v, %v", evs, err)
	}
}
