package board

import (
	"fmt"
	"slices"
	"strings"
)

// HeldWord starts the skip reason of an item a hold keeps back: its own
// open blocker, or an open blocker of an issue above it.
const HeldWord = "held: "

// HeldText is the hold of an item with open blockers as board next shows
// it: "held: blocked by owner/repo#n; 1 of 1 blockers open",
// "held: 1 of 1 blockers open" when its blockers were not read by name; ""
// for an item without an open blocker.
func (it Item) HeldText() string {
	if it.OpenBlockers == 0 {
		return ""
	}
	if len(it.OpenBlockedBy) == 0 {
		return HeldWord + blockersOpen(it)
	}
	details := it.OpenBlockedBy
	if len(it.BlockerDetails) > 0 {
		details = it.BlockerDetails
	}
	return fmt.Sprintf("%sblocked by %s; %s", HeldWord, strings.Join(details, ", "), blockersOpen(it))
}

// heldAbove is the skip reason of an issue under a held one: the nearest
// issue above it through up, among byRef, with an open blocker that is not
// ref itself; "" for none.
func heldAbove(ref string, up func(string) string, byRef map[string]Item) string {
	seen := map[string]bool{strings.ToLower(ref): true}
	for at := up(ref); at != "" && !seen[strings.ToLower(at)] && len(seen) <= maxDepth; at = up(at) {
		seen[strings.ToLower(at)] = true
		// The issue a hold waits on is never held by it: its blocker that
		// is its sub-issue stays free to work.
		if p, ok := byRef[strings.ToLower(at)]; ok && p.OpenBlockers > 0 && !containsFold(p.OpenBlockedBy, ref) {
			return fmt.Sprintf("%ssub-issue of %s, %s", HeldWord, p.Ref, strings.TrimPrefix(p.HeldText(), HeldWord))
		}
	}
	return ""
}

// assignedOutside returns the assignees of it that are not in people.
func assignedOutside(it Item, people []string) []string {
	return slices.DeleteFunc(slices.Clone(it.Assignees), func(a string) bool { return containsFold(people, a) })
}

// assignedAbove inherits ownership only for an unassigned item. The nearest
// assigned ancestor decides; shared ownership with people leaves work available.
func assignedAbove(it Item, up func(string) string, byRef map[string]Item, people []string) string {
	if len(it.Assignees) > 0 {
		return ""
	}
	seen := map[string]bool{strings.ToLower(it.Ref): true}
	for at := up(it.Ref); at != "" && !seen[strings.ToLower(at)] && len(seen) <= maxDepth; at = up(at) {
		seen[strings.ToLower(at)] = true
		if p, ok := byRef[strings.ToLower(at)]; ok && len(p.Assignees) > 0 {
			for _, a := range p.Assignees {
				if containsFold(people, a) {
					return ""
				}
			}
			if others := assignedOutside(p, people); len(others) > 0 {
				return fmt.Sprintf("assigned to %s (sub-issue of %s)", strings.Join(others, ", "), p.Ref)
			}
		}
	}
	return ""
}
