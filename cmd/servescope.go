package cmd

import (
	"strings"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// localPrefix starts a local agent's address, local:<machine>/<name>.
const localPrefix = "local:"

// rosterScope is what of the roster a caller reads: their own local agents,
// their team's local agents that work on a shared installation (hold an
// Environment's lease), and every remote agent, which its installation
// authorizes. list_agents, the roster and the feed, and their
// notifications, apply it alike.
type rosterScope struct {
	who identity.Caller
	// holders are the Environments' holders: the agents that work on a
	// shared installation.
	holders []state.Party
}

// scopeOf is who's scope over the Environments' current holders.
func scopeOf(who identity.Caller, envs []v1alpha1.Environment) rosterScope {
	sc := rosterScope{who: who}
	for _, e := range envs {
		if h := e.Status.Holder; h != nil {
			sc.holders = append(sc.holders, kube.PartyOf(h.Party))
		}
	}
	return sc
}

func (s *server) scope(who identity.Caller) (rosterScope, error) {
	envs, err := s.store.Environments()
	if err != nil {
		return rosterScope{}, err
	}
	return scopeOf(who, envs), nil
}

// own reports whether the agent is the caller's.
func (sc rosterScope) own(p state.Party) bool {
	return p.Person != "" && strings.EqualFold(p.Person, sc.who.Email)
}

// reads reports whether the caller reads the local agent p.
func (sc rosterScope) reads(p state.Party) bool {
	if sc.own(p) {
		return true
	}
	if p.Team == "" || p.Team != sc.who.Team {
		return false
	}
	for _, h := range sc.holders {
		if h.Is(p) {
			return true
		}
	}
	return false
}

// readsAgent applies the rule to a roster entry as it is shown: a remote
// agent's address is not local:, and every caller reads it.
func (sc rosterScope) readsAgent(a addressedAgent) bool {
	if !strings.HasPrefix(a.Address, localPrefix) {
		return true
	}
	return sc.reads(state.Party{Name: a.Name, Person: a.Person, Team: a.Team, Host: a.Host})
}

// readsEvent reports whether the caller reads a feed event: one about a
// roster entry is read with that entry, or, once the entry is gone, only by
// its actor's person. Every other event is the organization's.
func (sc rosterScope) readsEvent(ev feed.Event, agents []state.Agent) bool {
	name, ok := strings.CutPrefix(ev.Subject, rosterKind+"/")
	if !ok {
		return true
	}
	for _, a := range agents {
		if kube.RosterObject(a.Party).GetName() == name {
			return sc.reads(a.Party)
		}
	}
	return sc.own(state.Party{Person: ev.Actor.Person})
}

// rosterKind is the RosterEntry's kind, as a feed event's subject names it.
const rosterKind = "RosterEntry"
