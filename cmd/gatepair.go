package cmd

import (
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// pairFresh is how recently a paired merge's gate must have found only the
// pair holding it back for its sibling to start: a gate polls every 5s.
const pairFresh = 30 * time.Second

// declarePair records the pair the merge's --pair names, sibling as
// owner/repo#n or its URL, before the merge takes its place. A declaration
// that names no pull request, the merge itself, or a merge already paired
// with another is refused with nothing queued. The pair's merge refused
// before declares it anew; its sibling refused before leaves the refusal for
// the step, which refuses this merge with it.
func (g *gateRun) declarePair(arg string) error {
	ref, ok := parseRef(arg)
	switch {
	case g.pr == 0:
		return gateRefused("%s %s is for devctl pr merge, not a promotion: nothing promoted", merge.PairFlag, arg)
	case !ok:
		return gateRefused("%s %q names no pull request (owner/repo#n or its URL): nothing merged, run it again with the sibling named", merge.PairFlag, arg)
	}
	sib := state.Merge{Repo: ref.Repo, PR: ref.N}.Key()
	if sib == g.key() {
		return gateRefused("%s %s names the merge itself: nothing merged, name the pull request it lands with", merge.PairFlag, arg)
	}
	var taken string
	err := g.store.Update(func(st *state.State) ([]state.Event, error) {
		prunePairs(st, g.now, g.cfg.Merge.SeedTTL.Duration)
		for _, key := range []string{g.key(), sib} {
			if i := st.PairOf(key); i >= 0 && !samePair(st.Pairs[i], g.key(), sib) {
				p := st.Pairs[i]
				taken = fmt.Sprintf("%s lands with %s already (declared by %q at %s)", key, p.Sibling(key), p.By.Name, clock(g.now, p.At))
				return nil, nil
			}
		}
		if i := st.PairOf(g.key()); i >= 0 {
			if p := st.Pairs[i]; p.Refused == "" || p.RefusedKey != g.key() {
				return nil, nil
			}
			st.Pairs = slices.Delete(st.Pairs, i, i+1)
		}
		st.Pairs = append(st.Pairs, state.Pair{Keys: []string{g.key(), sib}, By: g.me, At: g.now.UTC()})
		return []state.Event{event(g.me, "merge.paired", "%s lands with %s", g.key(), sib)}, nil
	})
	switch {
	case err != nil:
		return g.stateRefused(err)
	case taken != "":
		_ = g.store.Log(event(g.me, "merge.refused", "%s: %s", g.key(), taken))
		return gateRefused("%s: nothing merged; a merge lands with one other at most", taken)
	}
	return nil
}

// samePair says whether p is the pair of a and b.
func samePair(p state.Pair, a, b string) bool { return p.Sibling(a) == b }

// prunePairs drops the pairs neither of whose merges has a place any more,
// once one started or the declaration is older than ttl.
func prunePairs(st *state.State, now time.Time, ttl time.Duration) {
	st.Pairs = slices.DeleteFunc(st.Pairs, func(p state.Pair) bool {
		for _, m := range st.Merges {
			if slices.Contains(p.Keys, m.Key()) {
				return false
			}
		}
		return p.Started != "" || now.Sub(p.At) > ttl
	})
}

// pairRefusal is the refusal of the merge's sibling that refuses this merge
// too, "" when none.
func (g *gateRun) pairRefusal(st *state.State) string {
	i := st.PairOf(g.key())
	if i < 0 {
		return ""
	}
	if p := st.Pairs[i]; p.Refused != "" && p.RefusedKey != g.key() {
		return fmt.Sprintf("%s lands with %s, which was refused: %s; nothing merged, both run again once that is fixed", g.key(), p.RefusedKey, p.Refused)
	}
	return ""
}

// refusePair records the merge's refusal on its pair, which refuses its
// sibling at its next step; a refusal the sibling's caused ends the pair.
func (g *gateRun) refusePair(st *state.State, why string) {
	i := st.PairOf(g.key())
	if i < 0 {
		return
	}
	p := &st.Pairs[i]
	if p.Refused != "" && p.RefusedKey != g.key() {
		st.Pairs = slices.Delete(st.Pairs, i, i+1)
		return
	}
	p.Refused, p.RefusedKey = why, g.key()
}

// pairTurn decides a paired merge at its lane turn: why is what it waits
// for, "" when it starts; need is the merge.cap and merge.sessionCap slots
// its start takes: two for the first of the pair, none for its sibling,
// whose slot the first took. An unpaired merge needs one.
func (g *gateRun) pairTurn(st *state.State) (why string, need int) {
	i := st.PairOf(g.key())
	if i < 0 {
		return "", 1
	}
	p := &st.Pairs[i]
	sib := p.Sibling(g.key())
	if p.Started == sib {
		return "", 0
	}
	if p.Ready == nil {
		p.Ready = map[string]time.Time{}
	}
	p.Ready[g.key()] = g.now.UTC()
	j := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Key() == sib && m.Phase == state.Waiting })
	switch {
	case j < 0:
		return fmt.Sprintf("next in lane %s, it lands with %s, which has no place in a lane yet: its merge (devctl pr merge … %s %s) runs, then both start",
			g.lane.Name, sib, merge.PairFlag, g.key()), 0
	case !proc.Alive(st.Merges[j].PID):
		return fmt.Sprintf("next in lane %s, it lands with %s, queued in lane %s, whose merge is not in the gate: both start once it is next there",
			g.lane.Name, sib, st.Merges[j].Lane), 0
	case g.now.Sub(p.Ready[sib]) > pairFresh:
		return fmt.Sprintf("next in lane %s, it lands with %s, not next in lane %s yet: both start once it is",
			g.lane.Name, sib, st.Merges[j].Lane), 0
	}
	return "", 2
}

// startPair records the merge as its pair's first start.
func (g *gateRun) startPair(st *state.State) {
	if i := st.PairOf(g.key()); i >= 0 && st.Pairs[i].Started == "" {
		st.Pairs[i].Started = g.key()
	}
}

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
