package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

const verbRunStart = "run.start"

// A holder that stops with the lock (a frozen process) fails every wait
// behind it after the bound, naming its PID and age; a holder without a
// record (a reader) is said as such, and the record leaves with the lock.
func TestAStuckHolderFailsTheWaitersWithItsPIDAndAge(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lock(false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	const wait = 50 * time.Millisecond
	t0 := time.Now()
	err = s.UpdateWithin(wait, func(*State) ([]Event, error) { t.Error("an update ran under a stuck holder"); return nil, nil })
	var busy *LockBusyError
	if !errors.As(err, &busy) || busy.PID != os.Getpid() || busy.Command == "" || time.Since(busy.Since) < time.Second || busy.Waited != wait {
		t.Fatalf("the update under a stuck holder = %#v", err)
	}
	if took := time.Since(t0); took > 20*wait {
		t.Errorf("the update waited %s, bound %s", took, wait)
	}
	if msg := err.Error(); !strings.Contains(msg, "pid "+strconv.Itoa(os.Getpid())) || !regexp.MustCompile(`for [1-9][0-9]*s, waited`).MatchString(msg) {
		t.Errorf("the error names neither PID nor age: %s", msg)
	}
	if err := s.Log(Event{Verb: verbRunStart}); !errors.As(err, &busy) || busy.PID != os.Getpid() {
		t.Errorf("Log under a stuck holder = %v", err)
	}
	if err := s.RecordWithin(wait, Event{Verb: verbRunStart}); !errors.As(err, &busy) || busy.PID != os.Getpid() || busy.Waited != wait {
		t.Errorf("RecordWithin under a stuck holder = %v", err)
	}
	unlock()
	if _, err := os.Stat(s.path(holderFile)); !os.IsNotExist(err) {
		t.Errorf("the holder's record outlived the lock: %v", err)
	}
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
		t.Errorf("the update after the release: %v", err)
	}

	unlock, err = s.lock(true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := s.UpdateWithin(wait, func(*State) ([]Event, error) { return nil, nil }); !errors.As(err, &busy) || busy.PID != 0 || !strings.Contains(err.Error(), "reader") {
		t.Errorf("the update under a stuck reader = %v", err)
	}
	if _, err := s.Read(); err != nil {
		t.Errorf("a read beside a reader: %v", err)
	}
}

func TestPartyIs(t *testing.T) {
	const agentOne = "Agent one"
	a := Party{Session: "s1", HostSession: "local_1", Name: agentOne}
	cases := []struct {
		b    Party
		want bool
	}{
		{Party{Session: "s1"}, true},
		{Party{Session: "s9", HostSession: "local_1"}, true}, // a restarted CLI
		{Party{Name: agentOne}, false},                       // a name alone is a person
		{Party{Session: "s2", HostSession: "local_2", Name: agentOne}, false},
	}
	for _, c := range cases {
		if got := a.Is(c.b); got != c.want {
			t.Errorf("Is(%+v) = %v", c.b, got)
		}
	}
	if !(Party{Name: "alex"}).Is(Party{Name: "alex"}) {
		t.Error("a person is not themselves")
	}
}

func TestUpdateIsAtomicAndLogged(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(func(st *State) ([]Event, error) {
				st.NextNote++
				return []Event{{At: time.Now(), Verb: "test", Detail: strconv.Itoa(i)}}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, err := s.Read()
	if err != nil || st.NextNote != 20 {
		t.Fatalf("NextNote = %d, %v", st.NextNote, err)
	}
	evs, err := s.Events(5, nil)
	if err != nil || len(evs) != 5 {
		t.Fatalf("Events = %d, %v", len(evs), err)
	}
	// A failing update writes nothing.
	boom := errors.New("boom")
	if err := s.Update(func(st *State) ([]Event, error) { st.NextNote = 99; return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("Update = %v", err)
	}
	if st, _ := s.Read(); st.NextNote != 20 {
		t.Fatalf("failed update was written: %d", st.NextNote)
	}
}

func TestLogAppendsWithoutTouchingTheState(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) {
		st.NextNote = 7
		return []Event{{Verb: "note"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Log(Event{Verb: verbRunStart, Detail: "a"}, Event{Verb: "run.end", Detail: "b"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Read(); st.NextNote != 7 {
		t.Errorf("Log changed the state: %+v", st)
	}
	runs, err := s.Events(0, func(e Event) bool { return e.Verb != "note" })
	if err != nil || len(runs) != 2 || runs[0].Detail != "a" || runs[1].Detail != "b" {
		t.Errorf("Events = %+v, %v", runs, err)
	}
	// A held lock drops the event after the bounded wait instead of blocking;
	// Record waits for the lock, up to lockWait, and loses nothing.
	l := flock.New(s.path("state.lock"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := s.Log(Event{Verb: verbRunStart}); err == nil || time.Since(t0) > 3*logWait {
		t.Errorf("Log under a held lock = %v after %s", err, time.Since(t0))
	}
	recorded := make(chan error, 1)
	go func() { recorded <- s.Record(Event{Verb: "secret.copy", Detail: "c"}) }()
	time.Sleep(2 * logWait)
	select {
	case err := <-recorded:
		t.Fatalf("Record under a held lock returned %v before the lock was released", err)
	default:
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-recorded; err != nil {
		t.Fatalf("Record after the lock was released: %v", err)
	}
	if evs, err := s.Events(0, func(e Event) bool { return e.Verb == "secret.copy" }); err != nil || len(evs) != 1 || evs[0].Detail != "c" {
		t.Errorf("the recorded event = %+v, %v", evs, err)
	}
}

func TestHoldActive(t *testing.T) {
	now := time.Now()
	if !(Hold{}).Active(now) {
		t.Error("a hold without Until is not active")
	}
	if (Hold{Until: now.Add(-time.Minute)}).Active(now) {
		t.Error("an expired hold is active")
	}
	if (Hold{LiftedBy: &Party{Name: "x"}}).Active(now) {
		t.Error("a lifted hold is active")
	}
}

func TestHoldExcepts(t *testing.T) {
	h := Hold{Except: "giantswarm/Model-Manager#172"}
	if !h.Excepts("giantswarm/model-manager", 172) || h.Excepts("giantswarm/model-manager", 17) || h.Excepts("giantswarm/model-manager", 0) {
		t.Error("a pull request exception")
	}
	h.Except = "giantswarm/devctl"
	if !h.Excepts("giantswarm/devctl", 3) || h.Excepts("giantswarm/marge", 3) || (Hold{}).Excepts("giantswarm/devctl", 3) {
		t.Error("a repository exception")
	}
}

func TestUpdateKeepsFieldsItDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	newer := `{"nextNote":2,"rota":{"next":"Supervisor run 13"},"quietHours":"4h"}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) { st.NextNote++; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "state.json")))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["nextNote"] != 3.0 || got["quietHours"] != "4h" || got["rota"].(map[string]any)["next"] != "Supervisor run 13" {
		t.Errorf("state.json = %s", raw)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		due, fired time.Time
		want       bool
	}{
		{time.Time{}, time.Time{}, false},
		{now.Add(time.Minute), time.Time{}, false},
		{now, time.Time{}, true},
		{now.Add(-time.Hour), time.Time{}, true},
		{now.Add(-time.Hour), now.Add(-time.Minute), false}, // reported once
	}
	for _, c := range cases {
		if got := Due(c.due, c.fired, now); got != c.want {
			t.Errorf("Due(%v, %v) = %v", c.due, c.fired, got)
		}
	}
}

func TestEventsSurviveAnUncleanShutdown(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The unclean end zeroed an append's range, and the next event landed
	// right behind the zeros on the same line.
	zeros := strings.Repeat("\x00", 275)
	raw := `{"at":"2026-09-27T13:53:18Z","verb":"timer.add"}` + "\n" +
		zeros + `{"at":"2026-09-27T14:09:56Z","verb":"reporter.start"}` + "\n" + zeros
	if err := os.WriteFile(s.path("events.jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Log(Event{At: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC), Verb: "note.add"}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, e := range events {
		verbs = append(verbs, e.Verb)
	}
	if got, want := strings.Join(verbs, " "), "timer.add reporter.start note.add"; got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
	after, err := os.ReadFile(s.path("events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(after), "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, `{"at":"2026-09-30T10:00:00Z"`) {
		t.Errorf("the new event shares a line with the zeros: %q", last)
	}
}

func TestEventsAreWrittenInUTC(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	local := time.Date(2026, 9, 28, 19, 6, 26, 0, time.FixedZone("CEST", 2*60*60))
	if err := s.Update(func(st *State) ([]Event, error) {
		return []Event{{At: local, Verb: "note.add"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Log(Event{At: local, Verb: verbRunStart}, Event{At: local, Verb: "hook.allow"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.path("events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct{ At, Verb string }
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.At != "2026-09-28T17:06:26Z" {
			t.Errorf("%s at = %q, want UTC", e.Verb, e.At)
		}
		lines++
	}
	if lines != 3 {
		t.Errorf("%d events, want 3", lines)
	}
}

// newerSchema is a state a newer beekeeper wrote: every object that carries
// per-entry data has a member this binary does not know.
const newerSchema = `{
  "writer": {"version": "v9.0.0", "build": "b1"},
  "supervisor": {"session": "s0", "name": "Supervisor run 9", "since": "2026-10-02T14:00:00Z", "term": 9},
  "guide": {"holder": {"session": "g0", "name": "Guide", "since": "2026-10-02T14:00:00Z"}, "mood": "calm"},
  "grants": [{"resource": "agentlab-1", "to": {"name": "a"}, "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "ttl": "2h"}],
  "holds": [{"target": "o/r", "reason": "x", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "scope": "merge"}],
  "agents": [
    {"session": "s1", "name": "Agent one", "registered": "2026-10-02T14:00:00Z", "keep": {"by": {"name": "s"}, "at": "2026-10-02T14:22:00Z", "ticket": "o/r#1"}, "lane": "x"},
    {"session": "s2", "name": "Agent two", "registered": "2026-10-02T14:00:00Z", "badge": "two"}
  ],
  "notes": [{"id": 1, "kind": "decision", "text": "q", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "urgency": "high"}],
  "timers": [{"id": 1, "due": "2026-10-02T15:00:00Z", "what": "w", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "jitter": "1m"}],
  "records": [{"session": {"name": "a"}, "issue": "o/r#1", "by": {"name": "a"}, "at": "2026-10-02T14:00:00Z", "phase": "ci"}],
  "starts": [{"session": "s1", "name": "Agent one", "mode": "bypassPermissions", "dir": "/w", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "model": "m"}],
  "merges": [{"repo": "o/r", "pr": 1, "lane": "l", "by": {"name": "a"}, "pid": 1, "phase": "waiting", "joined": "2026-10-02T14:00:00Z", "seen": "2026-10-02T14:00:00Z", "priority": 2}],
  "budget": {"remaining": 1, "limit": 2, "reset": "2026-10-02T15:00:00Z", "at": "2026-10-02T14:00:00Z", "search": 3},
  "rota": {"next": "Supervisor run 13"}
}`

// The members of the document the test looks up.
const (
	keySupervisor = "supervisor"
	keyAgents     = "agents"
)

// TestOlderSaveKeepsANewerSchema loads and saves a newer beekeeper's state
// with this binary's types, which lack a member in every object, while it
// changes the roster: every member it does not know is written back where
// it was, with its entry. A build without a release version saves it (a
// release older than the writer is refused).
func TestOlderSaveKeepsANewerSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(newerSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.version = "v8.0.0+dirty"
	err = s.Update(func(st *State) ([]Event, error) {
		st.Agents = []Agent{st.Agents[1], st.Agents[0], {Party: Party{Session: "s3", Name: "Agent three"}}}
		st.Agents[1].Keep.Reason = "parked"
		st.Notes = append(st.Notes, Note{ID: 2, Kind: NoteDecision, Text: "r"})
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "state.json")))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	at := func(path ...any) any {
		var v any = got
		for _, p := range path {
			switch k := p.(type) {
			case string:
				v = v.(map[string]any)[k]
			case int:
				v = v.([]any)[k]
			}
		}
		return v
	}
	want := []struct {
		path []any
		v    any
	}{
		{[]any{"writer", "version"}, "v9.0.0"},
		{[]any{"writer", "build"}, "b1"},
		{[]any{"guide", "mood"}, "calm"},
		{[]any{"grants", 0, "ttl"}, "2h"},
		{[]any{"holds", 0, "scope"}, "merge"},
		{[]any{keyAgents, 0, "badge"}, "two"},
		{[]any{keyAgents, 1, "lane"}, "x"},
		{[]any{keyAgents, 1, "keep", "ticket"}, "o/r#1"},
		{[]any{keyAgents, 1, "keep", "reason"}, "parked"},
		{[]any{"notes", 0, "urgency"}, "high"},
		{[]any{"timers", 0, "jitter"}, "1m"},
		{[]any{"records", 0, "phase"}, "ci"},
		{[]any{"starts", 0, "model"}, "m"},
		{[]any{"merges", 0, "priority"}, 2.0},
		{[]any{"budget", "search"}, 3.0},
		{[]any{"rota", "next"}, "Supervisor run 13"},
	}
	for _, w := range want {
		if v := at(w.path...); v != w.v {
			t.Errorf("%v = %v, want %v", w.path, v, w.v)
		}
	}
	if n := len(at(keyAgents, 2).(map[string]any)); n != 3 {
		t.Errorf("a new entry carries members of another: %v", at(keyAgents, 2))
	}
	if v, ok := got[keySupervisor]; ok {
		t.Errorf("the retired supervisor session's record is written back: %v", v)
	}
}

// The supervisor session's role is gone: an older binary's record of it
// (its holder, relay, reliefs, CLI, run and relay due) is dropped at the
// read, and the next save leaves it out, while every other member it does
// not know is kept.
func TestRetiredSupervisorKeysAreDropped(t *testing.T) {
	dir := t.TempDir()
	old := `{"supervisor":{"session":"s0","name":"Supervisor run 28","since":"2026-11-01T10:00:00Z"},` +
		`"relay":{"from":{"session":"s0"},"to":{"session":"s1","name":"Supervisor run 29"},"at":"2026-11-01T10:00:00Z","expires":"2026-11-01T10:15:00Z"},` +
		`"relieved":[{"session":"s9"}],"supervisorCLI":{"pid":7},"supervisorRun":28,"relayDue":{"contextTokens":1},` +
		`"nextNote":4,"rota":{"next":"x"}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) {
		st.NextNote++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "state.json")))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{keySupervisor, "relay", "relieved", "supervisorCLI", "supervisorRun", "relayDue"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s is written back: %v", k, v)
		}
	}
	if got["nextNote"] != 5.0 || got["rota"] == nil {
		t.Errorf("the rest of the state is not kept: %s", raw)
	}
}

// An older release's save after a newer one wrote the state is refused: the
// state and the log stay as they were, apart from one stale-writer event per
// process, and the error names the process and both versions.
func TestAnOlderReleasesSaveIsRefused(t *testing.T) {
	const newest = "v0.73.0"
	dir := t.TempDir()
	before := `{"writer":{"version":"v0.72.0"},"nextNote":1,"merges":[{"repo":"o/r","pr":7,"lane":"l","phase":"waiting","priority":2}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.version = "v0.71.1"
	for range 2 {
		err := s.Update(func(st *State) ([]Event, error) {
			st.NextNote++
			st.Merges = nil
			return []Event{{Verb: "merge.dropped"}}, nil
		})
		var stale *StaleWriterError
		if !errors.As(err, &stale) || stale.PID != os.Getpid() || stale.Version != "v0.71.1" || stale.Newer != "v0.72.0" ||
			!strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())+" (") || !strings.Contains(err.Error(), "v0.71.1, older than the v0.72.0 that wrote the state: its save is refused") {
			t.Fatalf("err = %v", err)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "state.json")); string(raw) != before { //nolint:gosec // a test file
		t.Errorf("the state changed: %s", raw)
	}
	evs, err := s.Events(0, func(Event) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Verb != VerbStaleWriter || !strings.Contains(evs[0].Detail, "its save is refused") {
		t.Fatalf("events = %+v, want one stale-writer", evs)
	}

	s.version = newest
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Read(); st.Writer.Version != newest {
		t.Errorf("a newer writer did not stamp: %+v", st.Writer)
	}
	for _, v := range []string{"dev", "v0.74.0-rc.1", "v0.74.0-rc.1+dirty", "v0.74.0+dirty"} {
		s.version = v
		if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
			t.Errorf("a %s build was refused: %v", v, err)
		}
		if st, _ := s.Read(); st.Writer.Version != newest {
			t.Errorf("a %s build stamped: %+v", v, st.Writer)
		}
	}
}

// A branch build's stamp judges no release: the next release overwrites it,
// and drops the record of stale saves that releases before the refusal kept.
func TestABranchBuildsStampJudgesNoRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"writer":{"version":"v0.89.1-rc.1+dirty"},"staleWriters":[{"pid":`+strconv.Itoa(os.Getpid())+`,"command":"beekeeper watch","version":"v0.89.0","newer":"v0.89.1-rc.1+dirty","at":"2026-10-05T15:08:00Z"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.version = "v0.89.0"
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Read(); st.Writer.Version != "v0.89.0" || st.rest != nil {
		t.Errorf("writer %+v, rest %s", st.Writer, st.rest)
	}
}

func TestCommand(t *testing.T) {
	for args, want := range map[string]string{
		"/home/u/.go/bin/beekeeper agents start --task brief": "beekeeper agents start",
		"beekeeper timer add 23:58 a timer's text":            "beekeeper timer add",
		"beekeeper watch --once":                              "beekeeper watch",
	} {
		if got := command(strings.Fields(args)); got != want {
			t.Errorf("command(%s) = %q", args, got)
		}
	}
}

const worker = "Worker"

func TestPartyIdentityIsPersisted(t *testing.T) {
	var s Store
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	by := Party{Session: "s1", Name: worker, Person: "alex@example.com", Team: "bumblebee", Host: "lab"}
	if err := s.Update(func(st *State) ([]Event, error) {
		st.Notes = append(st.Notes, Note{ID: 1, Kind: NoteDecision, Text: "q", By: by})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Notes[0].By; got != by {
		t.Errorf("By = %+v, want %+v", got, by)
	}
	if got, want := by.Owner(), "alex@example.com, team bumblebee, on lab"; got != want {
		t.Errorf("Owner() = %q, want %q", got, want)
	}
	if got := (Party{Name: worker}).Owner(); got != "" {
		t.Errorf("Owner() of a party without identity = %q", got)
	}
}

func TestStateWithoutIdentityLoads(t *testing.T) {
	dir := t.TempDir()
	doc := `{"notes":[{"id":1,"kind":"decision","text":"q","by":{"session":"s1","name":"Worker"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.Notes[0].By, (Party{Session: "s1", Name: worker}); got != want {
		t.Errorf("By = %+v, want %+v", got, want)
	}
}

// Follow hands back what it kept and parses only the lines logged since its
// last read; a line still being written waits, and a replaced log is read
// from its start.
func TestFollowReadsOnlyTheNewLines(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keep := func(e Event) bool { return e.Verb != "skip" }
	if evs, err := s.Follow("t", keep); err != nil || len(evs) != 0 {
		t.Fatalf("no log yet: %+v, %v", evs, err)
	}
	if err := s.Record(Event{Verb: "a"}, Event{Verb: "skip"}, Event{Verb: "b"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Follow("t", keep)
	if err != nil || len(evs) != 2 || evs[0].Verb != "a" || evs[1].Verb != "b" {
		t.Fatalf("Follow = %+v, %v", evs, err)
	}
	evs[0].Verb = "changed by its caller"

	// A line without its newline yet is left for the next read.
	f, err := os.OpenFile(s.path("events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"verb":"c"`); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Follow("t", keep); len(evs) != 2 || evs[0].Verb != "a" {
		t.Errorf("Follow with a line being written = %+v", evs)
	}
	if _, err := f.WriteString("}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if evs, _ := s.Follow("t", keep); len(evs) != 3 || evs[2].Verb != "c" {
		t.Errorf("Follow after the line ended = %+v", evs)
	}
	if all, _ := s.Events(0, keep); len(all) != 3 {
		t.Errorf("Events = %+v", all)
	}

	// A log replaced by a shorter one is read from its start.
	if err := os.WriteFile(s.path("events.jsonl"), []byte(`{"verb":"d"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Follow("t", keep); len(evs) != 1 || evs[0].Verb != "d" {
		t.Errorf("Follow after the log was replaced = %+v", evs)
	}
}

// An older beekeeper's memos are nobody's to read: the state document keeps
// them as written through a save, and none of them is a note.
func TestMemosOfAnOlderBeekeeperAreKeptUnread(t *testing.T) {
	doc := `{"nextNote": 4, "notes": [{"id": 4, "kind": "decision", "for": "Pat", "text": "merge it?", "by": {"name": "s"}}],
		"memos": [{"id": 1, "kind": "memo", "pinned": true, "text": "Board skip: o/r#10", "by": {"name": "s"}}]}`
	var st State
	if err := json.Unmarshal([]byte(doc), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].ID != 4 {
		t.Fatalf("notes %+v", st.Notes)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"memos":[{"id":1,"kind":"memo","pinned":true,"text":"Board skip: o/r#10","by":{"name":"s"}}]`) {
		t.Errorf("a save drops or rewrites the memos: %s", raw)
	}
}

// A state a newer release wrote loads in an older one: members it does not
// know are kept. One it cannot decode at all (a field the newer release
// changed) is refused naming both releases; the same document written by
// an older or the same release is a plain decode error.
func TestAStateANewerReleaseWroteLoadsOrNamesTheSkew(t *testing.T) {
	dir := t.TempDir()
	write := func(doc string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenVersion(dir, "v0.81.0")
	if err != nil {
		t.Fatal(err)
	}
	write(`{"writer":{"version":"v0.82.0"},"nextNote":3,"lanesV2":{"l":1}}`)
	if st, err := s.Read(); err != nil || st.NextNote != 3 || st.rest["lanesV2"] == nil {
		t.Fatalf("a newer release's unknown member: %+v, %v", st, err)
	}

	write(`{"writer":{"version":"v0.82.0"},"nextNote":"three"}`)
	_, err = s.Read()
	var stale *StaleWriterError
	if !errors.As(err, &stale) || stale.Version != "v0.81.0" || stale.Newer != "v0.82.0" || stale.Load == nil ||
		!strings.Contains(err.Error(), "runs beekeeper v0.81.0, older than the v0.82.0 that wrote the state: it does not load it (") {
		t.Fatalf("err = %v, want the skew named", err)
	}
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); !errors.As(err, &stale) {
		t.Errorf("update err = %v, want the skew", err)
	}

	write(`{"writer":{"version":"v0.81.0"},"nextNote":"three"}`)
	if _, err := s.Read(); err == nil || errors.As(err, &stale) {
		t.Errorf("err = %v, want a plain decode error", err)
	}
}
