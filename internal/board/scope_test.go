package board

import (
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// scopeRef is the scope tests' issue n.
func scopeRef(n int) string { return fmt.Sprintf("s/r#%d", n) }

func TestScopeOffersOnlyPriorityEpicsOrdersAndTheirBlockers(t *testing.T) {
	const scoped, order, search = "scoped", "order", "search"
	ranked, unranked := scopeRef(100), scopeRef(200) // epics in and outside the priority list
	snap := &Snapshot{
		Order: []config.BoardStep{
			{Name: order},
			{Name: scoped, WorkScope: true},
			{Name: search, Search: "repo:s/r"},
		},
		Items: []Item{
			{Ref: ranked, OpenBlockedBy: []string{scopeRef(5)}},
			{Ref: unranked},
		},
		Priorities: Priorities{{Rank: 1, Ref: ranked}},
	}
	cand := func(n int, step, parent string, blockedBy ...string) Candidate {
		return Candidate{Item: Item{Ref: scopeRef(n), Parent: parent, OpenBlockedBy: blockedBy}, Step: step}
	}
	stale := cand(9, scoped, "")
	stale.Skip = "no activity since 2025-01-01"
	cands := []Candidate{
		cand(1, scoped, ranked),                  // a priority epic's sub-issue
		cand(2, scoped, unranked),                // under an unranked epic
		cand(3, scoped, ""),                      // parentless
		cand(4, order, "", scopeRef(6)),          // an order, blocked by #6
		cand(5, scoped, ""),                      // the ranked epic's blocker
		cand(6, scoped, "", scopeRef(7)),         // the order's blocker, blocked by #7
		{Item: Item{Ref: "S/R#7"}, Step: scoped}, // a blocker's blocker, case ignored
		cand(8, search, ""),                      // a step without workScope
		stale,                                    // a skip already given stays
	}
	ByPriority(cands, snap.Priorities.Ranks(), snap.Parent)
	snap.Scope(cands)
	got := map[string]string{}
	for _, c := range cands {
		got[strings.ToLower(c.Ref)] = c.Skip
	}
	for n, skip := range map[int]string{1: "", 2: OutOfScope, 3: OutOfScope, 4: "", 5: "", 6: "", 7: "", 8: "", 9: stale.Skip} {
		if got[scopeRef(n)] != skip {
			t.Errorf("%s: skip %q, want %q", scopeRef(n), got[scopeRef(n)], skip)
		}
	}
	if cands[0].Ref != scopeRef(1) {
		t.Errorf("first %s, want the ranked sub-issue %s", cands[0].Ref, scopeRef(1))
	}
}

func TestScopeWithoutAWorkScopeStepSkipsNothing(t *testing.T) {
	snap := &Snapshot{Order: []config.BoardStep{{Name: "any"}}}
	cands := []Candidate{{Item: Item{Ref: scopeRef(1)}, Step: "any"}}
	snap.Scope(cands)
	if cands[0].Skip != "" {
		t.Errorf("skip %q, want none", cands[0].Skip)
	}
}
