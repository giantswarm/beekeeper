package state

import "testing"

func TestPartyIsHostSession(t *testing.T) {
	first := Party{HostSession: "codex_first", Name: "worker"}
	for _, tc := range []struct {
		name  string
		other Party
		want  bool
	}{
		{"different host session", Party{HostSession: "codex_second", Name: "worker"}, false},
		{"name only", Party{Name: "worker"}, false},
		{"renamed", Party{HostSession: "codex_first", Name: "renamed"}, true},
		{"restarted CLI", Party{Session: "new-cli", HostSession: "codex_first", Name: "renamed"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if first.Is(tc.other) != tc.want || tc.other.Is(first) != tc.want {
				t.Fatalf("identity match should be %v in both directions", tc.want)
			}
		})
	}
}

func TestKnowsHostSession(t *testing.T) {
	first := Party{HostSession: "codex_first", Name: "worker"}
	for _, st := range []*State{
		{Starts: []Start{{Party: first}}},
		{Agents: []Agent{{Party: first}}},
		{Guide: &Role{Holder: &Supervisor{Party: first}}},
	} {
		if st.Knows(Party{HostSession: "codex_second", Name: "worker"}) {
			t.Error("knows an unrelated host session by name")
		}
		if !st.Knows(Party{HostSession: "codex_first", Name: "renamed"}) {
			t.Error("does not know the renamed host session")
		}
	}
}
