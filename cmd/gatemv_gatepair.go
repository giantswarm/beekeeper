package cmd

import (
	"fmt"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// PairFresh is how recently a paired merge's gate must have found only the
// pair holding it back for its sibling to start: a gate polls every 5s.
const PairFresh = 30 * time.Second

// DeclarePair records the pair the merge's --pair names, sibling as
// owner/repo#n or its URL, before the merge takes its place. A declaration
// that names no pull request, the merge itself, or a merge already paired
// with another is refused with nothing queued. The pair's merge refused
// before declares it anew; its sibling refused before leaves the refusal for
// the step, which refuses this merge with it.
func (g *GateRun) DeclarePair(arg string) error {
	ref, ok := Cmd.ParseRef(arg)
	switch {
	case g.Pr == 0:
		return GateRefused("%s %s is for devctl pr merge, not a promotion: nothing promoted", merge.PairFlag, arg)
	case !ok:
		return GateRefused("%s %q names no pull request (owner/repo#n or its URL): nothing merged, run it again with the sibling named", merge.PairFlag, arg)
	}
	sib := state.Merge{Repo: ref.Repo, PR: ref.N}.Key()
	if sib == g.Key() {
		return GateRefused("%s %s names the merge itself: nothing merged, name the pull request it lands with", merge.PairFlag, arg)
	}
	var taken string
	err := g.Host.Store().Update(func(st *state.State) ([]state.Event, error) {
		prunePairs(st, g.Host.Now(), g.Host.Cfg().Merge.SeedTTL.Duration)
		for _, key := range []string{g.Key(), sib} {
			if i := st.PairOf(key); i >= 0 && !samePair(st.Pairs[i], g.Key(), sib) {
				p := st.Pairs[i]
				taken = fmt.Sprintf("%s lands with %s already (declared by %q at %s)", key, p.Sibling(key), p.By.Name, Cmd.Clock(g.Host.Now(), p.At))
				return nil, nil
			}
		}
		if i := st.PairOf(g.Key()); i >= 0 {
			if p := st.Pairs[i]; p.Refused == "" || p.RefusedKey != g.Key() {
				return nil, nil
			}
			st.Pairs = slices.Delete(st.Pairs, i, i+1)
		}
		st.Pairs = append(st.Pairs, state.Pair{Keys: []string{g.Key(), sib}, By: g.Me, At: g.Host.Now().UTC()})
		return []state.Event{Cmd.Event(g.Me, "merge.paired", "%s lands with %s", g.Key(), sib)}, nil
	})
	switch {
	case err != nil:
		return g.stateRefused(err)
	case taken != "":
		_ = g.Host.Store().Log(Cmd.Event(g.Me, "merge.refused", "%s: %s", g.Key(), taken))
		return GateRefused("%s: nothing merged; a merge lands with one other at most", taken)
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
func (g *GateRun) pairRefusal(st *state.State) string {
	i := st.PairOf(g.Key())
	if i < 0 {
		return ""
	}
	if p := st.Pairs[i]; p.Refused != "" && p.RefusedKey != g.Key() {
		return fmt.Sprintf("%s lands with %s, which was refused: %s; nothing merged, both run again once that is fixed", g.Key(), p.RefusedKey, p.Refused)
	}
	return ""
}

// refusePair records the merge's refusal on its pair, which refuses its
// sibling at its next step; a refusal the sibling's caused ends the pair.
func (g *GateRun) refusePair(st *state.State, why string) {
	i := st.PairOf(g.Key())
	if i < 0 {
		return
	}
	p := &st.Pairs[i]
	if p.Refused != "" && p.RefusedKey != g.Key() {
		st.Pairs = slices.Delete(st.Pairs, i, i+1)
		return
	}
	p.Refused, p.RefusedKey = why, g.Key()
}

// PairTurn decides a paired merge at its lane turn: why is what it waits
// for, "" when it starts; need is the merge.cap and merge.sessionCap slots
// its start takes: two for the first of the pair, none for its sibling,
// whose slot the first took. An unpaired merge needs one.
func (g *GateRun) PairTurn(st *state.State) (why string, need int) {
	i := st.PairOf(g.Key())
	if i < 0 {
		return "", 1
	}
	p := &st.Pairs[i]
	sib := p.Sibling(g.Key())
	if p.Started == sib {
		return "", 0
	}
	if p.Ready == nil {
		p.Ready = map[string]time.Time{}
	}
	p.Ready[g.Key()] = g.Host.Now().UTC()
	j := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Key() == sib && m.Phase == state.Waiting })
	switch {
	case j < 0:
		return fmt.Sprintf("next in lane %s, it lands with %s, which has no place in a lane yet: its merge (devctl pr merge … %s %s) runs, then both start",
			g.Lane.Name, sib, merge.PairFlag, g.Key()), 0
	case !proc.Alive(st.Merges[j].PID):
		return fmt.Sprintf("next in lane %s, it lands with %s, queued in lane %s, whose merge is not in the gate: both start once it is next there",
			g.Lane.Name, sib, st.Merges[j].Lane), 0
	case g.Host.Now().Sub(p.Ready[sib]) > PairFresh:
		return fmt.Sprintf("next in lane %s, it lands with %s, not next in lane %s yet: both start once it is",
			g.Lane.Name, sib, st.Merges[j].Lane), 0
	}
	return "", 2
}

// startPair records the merge as its pair's first start.
func (g *GateRun) startPair(st *state.State) {
	if i := st.PairOf(g.Key()); i >= 0 && st.Pairs[i].Started == "" {
		st.Pairs[i].Started = g.Key()
	}
}
