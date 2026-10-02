package cmd

import (
	"slices"
	"testing"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

func TestRosterScope(t *testing.T) {
	const ana, bo, pia = "ana@example.com", "bo@example.com", "pia@example.com"
	anaPrivate := state.Party{Name: "ana-private", Person: ana, Team: ourTeam, Host: lab}
	anaShared := state.Party{Name: "ana-shared", Person: ana, Team: ourTeam, Host: lab}
	boAgent := state.Party{Name: "bo-agent", Person: bo, Team: ourTeam, Host: "bo-laptop"}
	piaAgent := state.Party{Name: "pia-agent", Person: pia, Team: "planeteers", Host: "pia-laptop"}
	agents := []state.Agent{{Party: anaPrivate}, {Party: anaShared}, {Party: boAgent}, {Party: piaAgent}}
	// ana-shared works on graveler: its team reads it.
	envs := []v1alpha1.Environment{
		{Status: v1alpha1.EnvironmentStatus{Holder: &v1alpha1.Holder{Party: kube.APIParty(anaShared)}}},
		{},
	}
	remote := addressedAgent{Address: "kagent:graveler/kagent/sess-1", Name: "sess-1"}

	for _, tc := range []struct {
		who  identity.Caller
		want []string
	}{
		{identity.Caller{Email: ana, Team: ourTeam}, []string{"ana-private", "ana-shared", "sess-1"}},
		{identity.Caller{Email: "Ana@Example.com", Team: ourTeam}, []string{"ana-private", "ana-shared", "sess-1"}},
		{identity.Caller{Email: bo, Team: ourTeam}, []string{"ana-shared", "bo-agent", "sess-1"}},
		{identity.Caller{Email: pia, Team: "planeteers"}, []string{"pia-agent", "sess-1"}},
		{identity.Caller{Email: "eve@example.com"}, []string{"sess-1"}},
	} {
		sc := scopeOf(tc.who, envs)
		var got []string
		for _, a := range agents {
			if ag := addressed(a); sc.readsAgent(ag) {
				got = append(got, ag.Name)
			}
		}
		if sc.readsAgent(remote) {
			got = append(got, remote.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s reads %v, want %v", tc.who.Email, got, tc.want)
		}
	}

	// The feed: an event about a roster entry is read with the entry, or,
	// the entry gone, by its actor's person; the rest by everyone.
	event := func(p state.Party) feed.Event {
		return feed.Event{Kind: "agents.register", Subject: rosterKind + "/" + kube.RosterObject(p).GetName(), Actor: feed.Party{Name: p.Name, Person: p.Person, Team: p.Team, Host: p.Host}}
	}
	gone := state.Party{Name: "gone", Person: ana, Team: ourTeam, Host: lab}
	lease := feed.Event{Kind: "lease.claim", Subject: "Environment/graveler", Actor: feed.Party{Name: "ana-private", Person: ana, Team: ourTeam}}
	anaSc := scopeOf(identity.Caller{Email: ana, Team: ourTeam}, envs)
	boSc := scopeOf(identity.Caller{Email: bo, Team: ourTeam}, envs)
	for _, tc := range []struct {
		name     string
		ev       feed.Event
		ana, bob bool
	}{
		{"ana's private agent", event(anaPrivate), true, false},
		{"ana's shared agent", event(anaShared), true, true},
		{"bo's agent", event(boAgent), false, true},
		{"ana's agent, gone from the roster", event(gone), true, false},
		{"a lease's claim", lease, true, true},
	} {
		if got := anaSc.readsEvent(tc.ev, agents); got != tc.ana {
			t.Errorf("%s: ana reads it %v", tc.name, got)
		}
		if got := boSc.readsEvent(tc.ev, agents); got != tc.bob {
			t.Errorf("%s: bo reads it %v", tc.name, got)
		}
	}
}
