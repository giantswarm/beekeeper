package state

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
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
	if !(Party{Name: "timo"}).Is(Party{Name: "timo"}) {
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
	evs, err := s.Events(5)
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

func TestHoldActive(t *testing.T) {
	now := time.Now()
	if !(Hold{}).Active(now) {
		t.Error("a hold without Until is not active")
	}
	if (Hold{Until: now.Add(-time.Minute)}).Active(now) {
		t.Error("an expired hold is active")
	}
}
