package cmd

import (
	"github.com/giantswarm/beekeeper/internal/state"
)

// StartedAs reports whether p's session is a start of beekeeper's as one of
// rl's runs.
func (rl role) StartedAs(st *state.State, p state.Party) bool {
	s, ok := st.BypassStartOf(p)
	return ok && s.Role == rl.Name
}

// MayTake refuses me rl's role unless me holds it already (a restart of its
// CLI) or beekeeper started me as one of rl's runs: a session started by
// any other means never takes the role.
func (rl role) MayTake(st *state.State, me state.Party) error {
	if h := rl.Get(st).Holder; (h != nil && h.Is(me)) || rl.StartedAs(st, me) {
		return nil
	}
	return Cmd.Refused("%q is no %s run beekeeper started, and only such a run takes the role: `beekeeper %s start --new` starts the next one", me.Name, rl.Name, rl.Name)
}
