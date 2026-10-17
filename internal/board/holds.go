package board

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// HeldWord starts the skip reason of an item a hold keeps back: its own
// open blocker, or an open blocker of an issue above it.
const HeldWord = "held: "

// HeldText is the hold of an item with open blockers as priority and board
// next show it: "held: blocked by owner/repo#n; 1 of 1 blockers open",
// "held: 1 of 1 blockers open" when its blockers were not read by name; ""
// for an item without an open blocker.
func (it Item) HeldText() string {
	if it.OpenBlockers == 0 {
		return ""
	}
	if len(it.OpenBlockedBy) == 0 {
		return HeldWord + blockersOpen(it)
	}
	return fmt.Sprintf("%sblocked by %s; %s", HeldWord, strings.Join(it.OpenBlockedBy, ", "), blockersOpen(it))
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

// Seen is a ranked epic as the last read of the priority list found it,
// which the next read is compared with.
type Seen struct {
	Ref  string `json:"ref"`
	Rank int    `json:"rank"`
	Team string `json:"team,omitempty"`
}

// SeenOf is the record of list.
func SeenOf(list Priorities) []Seen {
	out := make([]Seen, 0, len(list))
	for _, e := range list {
		out = append(out, Seen{Ref: e.Ref, Rank: e.Rank, Team: e.Team})
	}
	return out
}

// Change is a change of a ranked epic's Team or Priority between two reads
// of the priority list that beekeeper did not write: the values before and
// after (0 or "" for none) and who wrote the new ones where GitHub names
// them, the field value's creator.
type Change struct {
	Ref      string    `json:"ref"`
	FromRank int       `json:"fromRank,omitempty"`
	ToRank   int       `json:"toRank,omitempty"`
	RankBy   string    `json:"rankBy,omitempty"`
	FromTeam string    `json:"fromTeam,omitempty"`
	ToTeam   string    `json:"toTeam,omitempty"`
	TeamBy   string    `json:"teamBy,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

// Text says what changed and who changed it.
func (c Change) Text() string {
	var parts []string
	if c.FromTeam != c.ToTeam {
		parts = append(parts, fmt.Sprintf("Team %s → %s%s", none(c.FromTeam), none(c.ToTeam), byText(c.TeamBy)))
	}
	if c.FromRank != c.ToRank {
		parts = append(parts, fmt.Sprintf("Priority %s → %s%s", rankText(c.FromRank), rankText(c.ToRank), byText(c.RankBy)))
	}
	s := c.Ref + ": " + strings.Join(parts, ", ")
	if !c.At.IsZero() {
		s += ", at " + c.At.UTC().Format(time.RFC3339)
	}
	return s + ", not by beekeeper"
}

func none(s string) string { return cmp.Or(s, "none") }

func rankText(n int) string {
	if n == 0 {
		return "none"
	}
	return fmt.Sprint(n)
}

func byText(login string) string {
	if login == "" {
		return " (writer unknown)"
	}
	return " by " + login
}

// Changes compares the record of the last read with the list read now. An
// epic on both whose rank or Team differs is a change, with the writers the
// list read; an epic that left the list is looked up with gone (its own
// item on the board, nil when GitHub has none or it is closed: no change).
// Epics that joined the list are no change of a ranked epic.
func Changes(was []Seen, now Priorities, gone func(ref string) *RankedEpic) []Change {
	var out []Change
	for _, w := range was {
		at := slices.IndexFunc(now, func(e RankedEpic) bool { return strings.EqualFold(e.Ref, w.Ref) })
		var cur *RankedEpic
		if at >= 0 {
			cur = &now[at]
		} else if cur = gone(w.Ref); cur == nil {
			continue
		}
		if cur.Rank == w.Rank && cur.Team == w.Team {
			continue
		}
		c := Change{Ref: w.Ref, FromRank: w.Rank, ToRank: cur.Rank, FromTeam: w.Team, ToTeam: cur.Team}
		if c.FromRank != c.ToRank {
			c.RankBy, c.At = cur.RankBy, cur.RankAt
		}
		if c.FromTeam != c.ToTeam {
			c.TeamBy, c.At = cur.TeamBy, cur.TeamAt
		}
		out = append(out, c)
	}
	return out
}
