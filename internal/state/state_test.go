package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

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
	if err := s.Log(Event{Verb: "run.start", Detail: "a"}, Event{Verb: "run.end", Detail: "b"}); err != nil {
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
	if err := s.Log(Event{Verb: "run.start"}); err == nil || time.Since(t0) > 3*logWait {
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
