package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

const verbRunStart = "run.start"

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
	// A held lock drops the event after the bounded wait instead of blocking.
	l := flock.New(s.path("state.lock"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Unlock() }()
	t0 := time.Now()
	if err := s.Log(Event{Verb: verbRunStart}); err == nil || time.Since(t0) > 3*logWait {
		t.Errorf("Log under a held lock = %v after %s", err, time.Since(t0))
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
  "notes": [{"id": 1, "text": "q", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "urgency": "high"}],
  "timers": [{"id": 1, "due": "2026-10-02T15:00:00Z", "what": "w", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "jitter": "1m"}],
  "records": [{"session": {"name": "a"}, "issue": "o/r#1", "by": {"name": "a"}, "at": "2026-10-02T14:00:00Z", "phase": "ci"}],
  "starts": [{"session": "s1", "name": "Agent one", "mode": "bypassPermissions", "dir": "/w", "by": {"name": "s"}, "at": "2026-10-02T14:00:00Z", "model": "m"}],
  "merges": [{"repo": "o/r", "pr": 1, "lane": "l", "by": {"name": "a"}, "pid": 1, "phase": "waiting", "joined": "2026-10-02T14:00:00Z", "seen": "2026-10-02T14:00:00Z", "priority": 2}],
  "budget": {"remaining": 1, "limit": 2, "reset": "2026-10-02T15:00:00Z", "at": "2026-10-02T14:00:00Z", "graphql": 3},
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
// it was, with its entry.
func TestOlderSaveKeepsANewerSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(newerSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.version = "v8.0.0"
	err = s.Update(func(st *State) ([]Event, error) {
		st.Agents = []Agent{st.Agents[1], st.Agents[0], {Party: Party{Session: "s3", Name: "Agent three"}}}
		st.Agents[1].Keep.Reason = "parked"
		st.Notes = append(st.Notes, Note{ID: 2, Text: "r"})
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
		{[]any{keySupervisor, "term"}, 9.0},
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
		{[]any{"budget", "graphql"}, 3.0},
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
}

func TestAStaleWriterIsLoggedOnce(t *testing.T) {
	const newest = "v0.73.0"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"writer":{"version":"v0.72.0"},"nextNote":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.version = "v0.71.1"
	for range 2 {
		if err := s.Update(func(st *State) ([]Event, error) { st.NextNote++; return nil, nil }); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.Events(0, func(e Event) bool { return e.Verb == VerbStaleWriter })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "pid "+strconv.Itoa(os.Getpid())) || !strings.Contains(evs[0].Detail, "v0.71.1, older than the v0.72.0") {
		t.Fatalf("stale-writer events = %+v", evs)
	}
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.NextNote != 3 || st.Writer.Version != "v0.72.0" || len(st.StaleWriters) != 1 || st.StaleWriters[0].PID != os.Getpid() {
		t.Errorf("state = %+v, writer %+v, stale %+v", st, st.Writer, st.StaleWriters)
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
			t.Fatal(err)
		}
		if st, _ := s.Read(); st.Writer.Version != newest {
			t.Errorf("a %s build stamped: %+v", v, st.Writer)
		}
	}
}

// A branch build's stamp judges no release: the next release overwrites it.
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
	if st, _ := s.Read(); st.Writer.Version != "v0.89.0" || len(st.StaleWriters) != 0 {
		t.Errorf("writer %+v, stale %+v", st.Writer, st.StaleWriters)
	}
}

func TestStaleWritersOfEndedProcessesGo(t *testing.T) {
	dir := t.TempDir()
	gone := `{"writer":{"version":"v0.72.0"},"staleWriters":[{"pid":2147483646,"command":"beekeeper watch","version":"v0.71.0","newer":"v0.72.0","at":"2026-10-02T14:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(gone), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.version = "v0.72.0"
	if err := s.Update(func(*State) ([]Event, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Read(); len(st.StaleWriters) != 0 {
		t.Errorf("stale writers = %+v", st.StaleWriters)
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
		st.Notes = append(st.Notes, Note{ID: 1, Text: "q", By: by})
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
	doc := `{"notes":[{"id":1,"text":"q","by":{"session":"s1","name":"Worker"}}]}`
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
