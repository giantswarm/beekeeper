package board

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// heldBy is the open blocker that holds the epic in the hold tests.
const heldBy = "o/r#99"

// A held epic, one with an open recorded blocker, is offered as itself and
// skipped naming its blocker, never through its sub-issues; a board item
// under it is skipped naming the epic, whatever step offers it, except the
// one the hold waits on.
func TestRankHoldsABlockedEpicAndEverythingUnderIt(t *testing.T) {
	snap := snapshot(t)
	for i := range snap.Items {
		switch snap.Items[i].Ref {
		case "o/r#15":
			snap.Items[i].Blockers, snap.Items[i].OpenBlockers, snap.Items[i].OpenBlockedBy = 2, 2, []string{heldBy, "O/R#17"}
		case "o/r#16", "o/r#17":
			snap.Items[i].Parent = "o/r#15"
		}
	}
	got := map[string]Candidate{}
	for _, c := range Rank(snap, deskBoard(), now) {
		if c.Epic == "o/r#15" {
			t.Errorf("%s offered through the held epic", c.Ref)
		}
		got[c.Ref] = c
	}
	if c := got["o/r#15"]; c.Skip != "held: blocked by o/r#99, O/R#17; 2 of 2 blockers open" {
		t.Errorf("held epic: skip %q", c.Skip)
	}
	if c := got["o/r#16"]; c.Step != "Backlog" || c.Skip != "held: sub-issue of o/r#15, blocked by o/r#99, O/R#17; 2 of 2 blockers open" {
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

func TestTeamLabelDefaultsToTheTeamsFirstWord(t *testing.T) {
	for _, tc := range []struct {
		b    config.Board
		want string
	}{
		{config.Board{Team: "Bumblebee🐝"}, "team/bumblebee"},
		{config.Board{Team: "Team Planeteers 🪐"}, "team/team"},
		{config.Board{Team: "X", TeamLabel: "squad/x"}, "squad/x"},
		{config.Board{}, ""},
	} {
		if got := (&Client{Board: tc.b}).TeamLabel(); got != tc.want {
			t.Errorf("TeamLabel(%+v) = %q, want %q", tc.b, got, tc.want)
		}
	}
}

// A ranked epic the label sync moved to another Team stays on the list by
// its team label, at its rank and with its new Team and its writer; an epic
// both queries find is listed once.
func TestPrioritiesKeepARankedEpicWhoseTeamMoved(t *testing.T) {
	f := &priorityGH{t: t, ranked: map[int]int{1: 1, 3: 3}, labelled: map[int]int{1: 1, 2: 2}}
	l, err := priorityClient(f).Priorities(context.Background())
	if err != nil || listText(l) != "1 o/r#1, 2 o/r#2, 3 o/r#3" {
		t.Fatalf("Priorities = %s, %v", listText(l), err)
	}
	if e := l[1]; e.Team != "Y" || e.TeamBy != "giantswarm-project-automation" || e.RankBy != "teemow" {
		t.Errorf("moved epic = %+v", e)
	}
	if e := l[0]; e.Team != "X 🐝" {
		t.Errorf("epic both queries find: Team %q", e.Team)
	}
}

func TestChangesNameWhatMovedAndWhoMovedIt(t *testing.T) {
	at := time.Date(2026, 10, 16, 6, 53, 56, 0, time.UTC)
	was := []Seen{{Ref: ref1, Rank: 1, Team: "X 🐝"}, {Ref: ref2, Rank: 2, Team: "X 🐝"}, {Ref: ref3, Rank: 3, Team: "X 🐝"}, {Ref: ref4, Rank: 4, Team: "X 🐝"}}
	now := Priorities{
		{Ref: ref1, Rank: 1, Team: "X 🐝"},                                 // unchanged
		{Ref: ref2, Rank: 2, Team: "Y", TeamBy: "automation", TeamAt: at}, // Team moved, kept by its label
		{Ref: "o/r#5", Rank: 5, Team: "X 🐝"},                              // joined: no change of a ranked epic
	}
	gone := map[string]*RankedEpic{
		ref3: {Ref: ref3, Rank: 0, Team: "X 🐝", RankBy: "pat", RankAt: at}, // unranked by somebody
		// ref4 closed: nil
	}
	var looked []string
	got := Changes(was, now, func(ref string) *RankedEpic {
		looked = append(looked, ref)
		return gone[ref]
	})
	if !slices.Equal(looked, []string{ref3, ref4}) {
		t.Errorf("looked up %v, want the epics that left the list only", looked)
	}
	var texts []string
	for _, c := range got {
		texts = append(texts, c.Text())
	}
	want := []string{
		"o/r#2: Team X 🐝 → Y by automation, at 2026-10-16T06:53:56Z, not by beekeeper",
		"o/r#3: Priority 3 → none by pat, at 2026-10-16T06:53:56Z, not by beekeeper",
	}
	if !slices.Equal(texts, want) {
		t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	if c := (Change{Ref: ref1, FromRank: 1, ToRank: 2}); !strings.Contains(c.Text(), "Priority 1 → 2 (writer unknown)") {
		t.Errorf("unknown writer: %s", c.Text())
	}
}
