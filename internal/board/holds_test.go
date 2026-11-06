package board

import (
	"strings"
	"testing"
)

// The hold tests' refs: the open blocker that holds the epic, the epic.
const (
	heldBy   = "o/r#99"
	heldEpic = "o/r#15"
	heldSub  = "o/r#16"
)

// A held epic, one with an open recorded blocker, is offered as itself and
// skipped naming its blocker, never through its sub-issues; a board item
// under it is skipped naming the epic, whatever step offers it, except the
// one the hold waits on.
func TestRankHoldsABlockedEpicAndEverythingUnderIt(t *testing.T) {
	snap := snapshot(t)
	for i := range snap.Items {
		switch snap.Items[i].Ref {
		case heldEpic:
			snap.Items[i].Blockers, snap.Items[i].OpenBlockers, snap.Items[i].OpenBlockedBy = 2, 2, []string{heldBy, "O/R#17"}
		case heldSub, "o/r#17":
			snap.Items[i].Parent = heldEpic
		}
	}
	got := map[string]Candidate{}
	for _, c := range Rank(snap, deskBoard(), now) {
		if c.Epic == heldEpic {
			t.Errorf("%s offered through the held epic", c.Ref)
		}
		got[c.Ref] = c
	}
	if c := got[heldEpic]; c.Skip != "held: blocked by o/r#99, O/R#17; 2 of 2 blockers open" {
		t.Errorf("held epic: skip %q", c.Skip)
	}
	if c := got[heldSub]; c.Step != backlogStep || c.Skip != "held: sub-issue of o/r#15, blocked by o/r#99, O/R#17; 2 of 2 blockers open" {
		t.Errorf("sub-issue of the held epic: %s, skip %q", c.Step, c.Skip)
	}
	// The sub-issue the hold waits on is not held by it (17 is old for
	// Backlog, so only the order turns it away).
	if c := got["o/r#17"]; strings.HasPrefix(c.Skip, HeldWord) {
		t.Errorf("the epic's blocker is held by it: %q", c.Skip)
	}
	if c := got["o/r#13"]; c.Skip != "" {
		t.Errorf("an item under no hold is skipped: %q", c.Skip)
	}
}

func TestHeldTextNamesTheOpenBlockersRead(t *testing.T) {
	for _, tc := range []struct {
		it   Item
		want string
	}{
		{Item{}, ""},
		{Item{Blockers: 2, OpenBlockers: 1}, "held: 1 of 2 blockers open"},
		{Item{Blockers: 2, OpenBlockers: 2, OpenBlockedBy: []string{"o/r#1", "p/q#2"}}, "held: blocked by o/r#1, p/q#2; 2 of 2 blockers open"},
	} {
		if got := tc.it.HeldText(); got != tc.want {
			t.Errorf("HeldText(%+v) = %q, want %q", tc.it, got, tc.want)
		}
	}
}

func TestRankSkipsSubIssuesUnderEpicAssignedOutsidePeople(t *testing.T) {
	snap := snapshot(t)
	for i := range snap.Items {
		switch snap.Items[i].Ref {
		case heldEpic:
			snap.Items[i].Assignees = []string{"Me", "pat"}
		case heldSub:
			snap.Items[i].Parent = heldEpic
		case "o/r#13":
			snap.Items[i].Parent = heldSub
		}
	}
	got := map[string]Candidate{}
	for _, c := range Rank(snap, deskBoard(), now) {
		if c.Epic == heldEpic {
			t.Errorf("%s offered through epic assigned outside people", c.Ref)
		}
		got[c.Ref] = c
	}
	if c := got[heldEpic]; c.Skip != "assigned to pat" {
		t.Errorf("assigned epic: skip %q", c.Skip)
	}
	if c := got[heldSub]; c.Step != backlogStep || c.Skip != "assigned to pat (sub-issue of o/r#15)" {
		t.Errorf("sub-issue of assigned epic: %s, skip %q", c.Step, c.Skip)
	}
	if c := got["o/r#13"]; c.Step != backlogStep || c.Skip != "assigned to pat (sub-issue of o/r#15)" {
		t.Errorf("nested sub-issue of assigned epic: %s, skip %q", c.Step, c.Skip)
	}
	if c := got["o/r#10"]; c.Skip != "" {
		t.Errorf("unassigned item skipped: %q", c.Skip)
	}
}
