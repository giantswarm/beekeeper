package cmd

import (
	"github.com/giantswarm/beekeeper/internal/state"
)

// startedAs reports whether p's session is a start of beekeeper's as one of
// rl's runs.
func (rl role) startedAs(st *state.State, p state.Party) bool {
	s, ok := st.BypassStartOf(p)
	return ok && s.Role == rl.name
}

// mayTake refuses me rl's role unless me holds it already (a restart of its
// CLI) or beekeeper started me as one of rl's runs: a session started by
// any other means never takes the role.
func (rl role) mayTake(st *state.State, me state.Party) error {
	if h := rl.get(st).Holder; (h != nil && h.Is(me)) || rl.startedAs(st, me) {
		return nil
	}
	return refused("%q is no %s run beekeeper started, and only such a run takes the role: `beekeeper %s start --new` starts the next one", me.Name, rl.name, rl.name)
}
