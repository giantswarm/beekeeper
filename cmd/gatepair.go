package cmd

import (
	"fmt"
	"slices"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// pairLines are the pair each merge of the lanes lands with, by key: its
// sibling and where that is.
func pairLines(st *state.State) map[string]string {
	out := map[string]string{}
	for _, p := range st.Pairs {
		for _, key := range p.Keys {
			sib := p.Sibling(key)
			where := "no place in a lane yet"
			if j := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Key() == sib }); j >= 0 {
				m := st.Merges[j]
				where = m.Phase + " in lane " + m.Lane
				if m.Phase == state.Waiting {
					where = fmt.Sprintf("position %d in lane %s", merge.Queue(st, m.Lane).Position(m.Repo, m.PR), m.Lane)
				}
			}
			text := fmt.Sprintf("lands with %s (%s)", sib, where)
			if p.Refused != "" {
				text += fmt.Sprintf(", %s refused: %s", p.RefusedKey, p.Refused)
			}
			out[key] = text
		}
	}
	return out
}
