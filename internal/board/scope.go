package board

import "strings"

// OutOfScope is the skip reason of an item a workScope step offers outside
// the work scope.
const OutOfScope = "not in a priority epic"

// Scope marks skipped, as OutOfScope, each candidate of a workScope step
// that is outside the work scope; ByPriority has ranked cands. The work
// scope is an issue with a rank, its own or its ranked ancestor's (a
// priority epic's sub-issue), an order (a candidate of a step without
// workScope: what the person's own moves put there), and an open recorded
// blocker of either, however deep. An item under an unranked epic, or
// without one, is skipped, not ordered last. A skip already given stays.
func (snap *Snapshot) Scope(cands []Candidate) {
	scoped := map[string]bool{}
	for _, st := range snap.Order {
		if st.WorkScope {
			scoped[st.Name] = true
		}
	}
	if len(scoped) == 0 {
		return
	}
	ranks := snap.Priorities.Ranks()
	blockedBy := map[string][]string{}
	in := map[string]bool{}
	var queue []string
	add := func(ref string) {
		if k := strings.ToLower(ref); !in[k] {
			in[k] = true
			queue = append(queue, k)
		}
	}
	for _, it := range snap.Items {
		blockedBy[strings.ToLower(it.Ref)] = it.OpenBlockedBy
		if n, _ := ranks.Of(it.Ref, snap.Parent); n > 0 {
			add(it.Ref)
		}
	}
	for _, c := range cands {
		if len(c.OpenBlockedBy) > 0 {
			blockedBy[strings.ToLower(c.Ref)] = c.OpenBlockedBy
		}
		if c.Priority > 0 || !scoped[c.Step] {
			add(c.Ref)
		}
	}
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		for _, b := range blockedBy[ref] {
			add(b)
		}
	}
	for i := range cands {
		c := &cands[i]
		if scoped[c.Step] && c.Skip == "" && !in[strings.ToLower(c.Ref)] {
			c.Skip = OutOfScope
		}
	}
}
