package state

import "testing"

func TestPartyIsDistinguishesHostSessionIdentities(t *testing.T) {
	const name = "same named worker"
	one := Party{HostSession: "codex_first", Name: name, Person: "owner@example.com", Host: "fixture-host"}
	for _, tc := range []struct {
		name  string
		other Party
		want  bool
	}{
		{name: "same host session renamed", other: Party{HostSession: one.HostSession, Name: "renamed"}, want: true},
		{name: "another host session", other: Party{HostSession: "codex_second", Name: name}},
		{name: "another harness", other: Party{HostSession: "agy_second", Name: name}},
		{name: "person with the same name", other: Party{Name: name}},
		{name: "known other person", other: Party{HostSession: one.HostSession, Person: "other@example.com"}},
		{name: "known other host", other: Party{HostSession: one.HostSession, Host: "another-host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := one.Is(tc.other); got != tc.want {
				t.Errorf("Is(%+v) = %v, want %v", tc.other, got, tc.want)
			}
			if got := tc.other.Is(one); got != tc.want {
				t.Errorf("reverse Is = %v, want %v", got, tc.want)
			}
		})
	}
	if !(Party{Name: name}).Is(Party{Name: name}) {
		t.Error("two person records with the same name no longer match")
	}
	st := &State{Agents: []Agent{{Party: one}}}
	if st.Knows(Party{HostSession: "codex_second", Name: name}) {
		t.Error("a roster name made an unrelated host session known")
	}
}
