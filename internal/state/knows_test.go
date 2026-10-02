package state

import "testing"

func TestKnows(t *testing.T) {
	st := &State{
		Starts:     []Start{{Party: Party{Session: "started"}}},
		Supervisor: &Supervisor{Party: Party{Session: "supervisor"}},
		Guide:      &Role{Holder: &Supervisor{Party: Party{HostSession: "local_guide"}}},
		Agents:     []Agent{{Party: Party{Session: "agent", Name: "bee"}}},
	}
	for _, c := range []struct {
		p    Party
		want bool
	}{
		{Party{Session: "started"}, true},
		{Party{Session: "supervisor"}, true},
		{Party{Session: "x", HostSession: "local_guide"}, true},
		{Party{Session: "agent"}, true},
		{Party{Session: "own"}, false},
		{Party{Name: "bee"}, false},
		{Party{}, false},
	} {
		if got := st.Knows(c.p); got != c.want {
			t.Errorf("Knows(%+v) = %v, want %v", c.p, got, c.want)
		}
	}
}
