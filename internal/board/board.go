// Package board picks the next item of work from a GitHub project board: it
// reads the board's open items once, ranks them by the configured order and
// says for every item above the pick why it was skipped. Which items a
// session already serves is the caller's to decide; the rank is pure.
package board

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/giantswarm/beekeeper/internal/config"
)

// The board's fields the order and the moves read.
const (
	StatusField = "Status"
	KindField   = "Kind"
	TeamField   = "Team"
)

// Item is an open issue: a board item or a sub-issue or search result.
type Item struct {
	// Ref is owner/repo#n.
	Ref   string `json:"ref"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// OnBoard says it is an item of the board project, Status and Kind
	// its board fields.
	OnBoard   bool      `json:"onBoard,omitempty"`
	Status    string    `json:"status,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Assignees []string  `json:"assignees,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	// Blockers counts the recorded blockers, OpenBlockers the open ones;
	// OpenBlockedBy names those read (owner/repo#n), from a read of the
	// board's items or the priority list only.
	Blockers      int      `json:"blockers,omitempty"`
	OpenBlockers  int      `json:"openBlockers,omitempty"`
	OpenBlockedBy []string `json:"openBlockedBy,omitempty"`
	// StatusSet is when its Status was last set, BlockersClosed when the
	// last of its recorded blockers closed; zero when unknown.
	StatusSet      time.Time `json:"statusSet,omitzero"`
	BlockersClosed time.Time `json:"blockersClosed,omitzero"`
	// OpenSubIssues counts the open sub-issues; SubIssues are they, read
	// for a SubIssues step only.
	OpenSubIssues int    `json:"openSubIssues,omitempty"`
	SubIssues     []Item `json:"subIssues,omitempty"`
	// Parent is the issue it is a sub-issue of, owner/repo#n.
	Parent string `json:"parent,omitempty"`
	// Missing names the fields of it GitHub answered with an error instead
	// of their value ("<field>: <message>"): they are unknown, not absent.
	Missing []string `json:"missing,omitempty"`
	// subIssues counts its sub-issues, closed ones too: what a read of
	// them returns.
	subIssues int
}

// missingBoardFields is the start of Missing's entry for a sub-issue's
// board fields (its projectItems).
const missingBoardFields = "projectItems:"

// boardFieldsUnknown says GitHub answered its board fields with an error.
func (it Item) boardFieldsUnknown() bool {
	return slices.ContainsFunc(it.Missing, func(m string) bool { return strings.HasPrefix(m, missingBoardFields) })
}

// HeldByStatus reports whether its Status was set after the last of its
// recorded blockers closed: set for a reason no blocker records (a person,
// a plan), the Status holds it until it changes.
func (it Item) HeldByStatus() bool {
	return !it.StatusSet.IsZero() && !it.BlockersClosed.IsZero() && it.StatusSet.After(it.BlockersClosed)
}

// HeldPrefix starts the refusal of an item its Status holds.
const HeldPrefix = "held by its Status"

// LeaseLabel is the prefix of an issue label naming a lease the work needs:
// lease/agentlab-1.
const LeaseLabel = "lease/"

// Leases are the leases the item's labels say its work needs.
func (it Item) Leases() []string {
	var out []string
	for _, l := range it.Labels {
		if res, ok := strings.CutPrefix(strings.ToLower(l), LeaseLabel); ok && res != "" {
			out = append(out, res)
		}
	}
	return out
}

// Snapshot is what one read of the board returned.
type Snapshot struct {
	// Order is the configured order, its Status and Kind values the
	// board's canonical names.
	Order []config.BoardStep
	// Items are the open board items in the board's order.
	Items []Item
	// Search holds each search step's issues by its index in Order.
	Search map[int][]Item
	// Statuses are the board's Status values, in the board's order.
	Statuses []string
	// Gate is the epic gate; Plans the plans of the epics it watches by
	// their lower-cased ref.
	Gate  Gate
	Plans map[string]*Plan
	// Missing names what GitHub answered with an error and no item's
	// Missing holds ("<path>: <message>"), a search step's issues for one.
	Missing []string
	// Priorities is the priority list Read read with the items.
	Priorities Priorities
}

// Candidate is an item the order offers, in its place.
type Candidate struct {
	Item
	// Step is the name of the step that offers it; Epic the epic whose
	// sub-issue it is.
	Step string `json:"step"`
	Epic string `json:"epic,omitempty"`
	// Skip says why it is not free, empty when it is.
	Skip string `json:"skip,omitempty"`
	// Priority is the rank ByPriority applied, 0 for none; PriorityOf the
	// epic it is the rank of, the candidate itself or an ancestor.
	Priority   int    `json:"priority,omitempty"`
	PriorityOf string `json:"priorityOf,omitempty"`
}

// Why says why a free candidate is picked.
func (c Candidate) Why() string {
	s := c.Step
	if c.Epic != "" {
		s += ", sub-issue of " + c.Epic
	}
	if c.Status != "" {
		s += " (" + c.Status + ")"
	}
	if p := c.PriorityText(); p != "" {
		s += ", " + p
	}
	return s
}

// PriorityText says the rank applied: "priority 2", "priority 2 of epic
// owner/repo#n" when inherited; "" for none.
func (c Candidate) PriorityText() string {
	switch {
	case c.Priority == 0:
		return ""
	case strings.EqualFold(c.PriorityOf, c.Ref):
		return fmt.Sprintf("priority %d", c.Priority)
	}
	return fmt.Sprintf("priority %d of epic %s", c.Priority, c.PriorityOf)
}

// Rank returns the candidates of every step in order, each item once, in
// the first step that matches it. A SubIssues step offers an item with open
// sub-issues through them (its remainder) and is the item's only step; an
// item without is offered itself. A sub-issue that is a board item, of
// any Team or Status, is held to the order like any other: one no step
// offers on its own (an old Backlog item, a blocked one, one in Inbox) is
// marked skipped with the reason. An item with an open recorded blocker is
// held: marked skipped naming its blockers ("held: blocked by …"), an epic
// offered as itself, never through its sub-issues, and an issue under a
// held one through the sub-issue relation is marked skipped naming it, a blocked one whose Status was set after
// its recorded blockers closed too (HeldByStatus). Items with an open recorded blocker,
// without activity for b.StaleAfter or assigned to anybody outside
// b.People are marked skipped too. The epic gate (snap.Gate) holds an epic
// in planning and every issue under it whatever the step: the epic is
// marked skipped with its plan, never offered through its sub-issues, and
// an issue whose nearest epic among its parents on the board is in
// planning is marked skipped naming it. What GitHub answered with an error
// is never read as absent: a sub-issue whose board fields are unknown takes
// them from its item among snap.Items, else is marked skipped naming them,
// and an item whose sub-issues are unknown is offered skipped itself.
func Rank(snap *Snapshot, b config.Board, now time.Time) []Candidate {
	seen := map[string]bool{}
	byRef := map[string]Item{}
	for _, it := range snap.Items {
		byRef[strings.ToLower(it.Ref)] = it
	}
	var out []Candidate
	offer := func(it Item, step, epic, skip string) {
		if seen[it.Ref] {
			return
		}
		seen[it.Ref] = true
		up := func(ref string) string {
			if strings.EqualFold(ref, it.Ref) {
				return cmp.Or(it.Parent, epic)
			}
			return snap.Parent(ref)
		}
		out = append(out, Candidate{Item: it, Step: step, Epic: epic, Skip: cmp.Or(skip, heldAbove(it.Ref, up, byRef), snap.planning(it, byRef), skipReason(it, b, now))})
	}
	for i, st := range snap.Order {
		if st.Search != "" {
			for _, it := range snap.Search[i] {
				offer(it, st.Name, "", "")
			}
			continue
		}
		for _, it := range snap.Items {
			if seen[it.Ref] || !Matches(st, it, now) {
				continue
			}
			if !st.SubIssues || it.OpenSubIssues == 0 || snap.planning(it, byRef) != "" || it.OpenBlockers > 0 {
				offer(it, st.Name, "", "")
				continue
			}
			if len(it.SubIssues) == 0 && len(it.Missing) > 0 {
				offer(it, st.Name, "", "sub-issues unknown: "+strings.Join(it.Missing, "; "))
				continue
			}
			seen[it.Ref] = true
			for _, sub := range it.SubIssues {
				// A sub-issue that is a board item read with the board
				// carries its board fields from there, whatever its own
				// read answered.
				// Its blockers' last close is read with the board's items
				// only.
				if b, ok := byRef[strings.ToLower(sub.Ref)]; ok {
					if !sub.OnBoard || sub.boardFieldsUnknown() {
						b.SubIssues = nil
						sub = b
					}
					sub.BlockersClosed = b.BlockersClosed
				}
				skip := ""
				switch {
				case sub.boardFieldsUnknown():
					skip = "board fields unknown: " + strings.Join(sub.Missing, "; ")
				case sub.OnBoard:
					skip = Unoffered(snap.Order, sub, now)
				}
				offer(sub, st.Name, it.Ref, skip)
			}
		}
	}
	return out
}

// Matches reports whether the step st offers the board item it.
func Matches(st config.BoardStep, it Item, now time.Time) bool {
	return selects(st, it) && refusal(st, it, now) == ""
}

// Unoffered says why no step of order offers the board item it on its own,
// the first refusal of a step that selects it; empty when a step offers it.
func Unoffered(order []config.BoardStep, it Item, now time.Time) string {
	why := ""
	for _, st := range order {
		if st.Search != "" || !selects(st, it) {
			continue
		}
		r := refusal(st, it, now)
		if r == "" {
			return ""
		}
		why = cmp.Or(why, r)
	}
	return cmp.Or(why, "no step of board.order offers "+cmp.Or(it.Status, "an item without a Status"))
}

// selects reports whether st's statuses, kinds and labels take it.
func selects(st config.BoardStep, it Item) bool {
	return (len(st.Status) == 0 || slices.Contains(st.Status, it.Status)) &&
		(len(st.Kind) == 0 || slices.Contains(st.Kind, it.Kind)) &&
		(len(st.Labels) == 0 || slices.ContainsFunc(it.Labels, func(l string) bool { return containsFold(st.Labels, l) }))
}

// refusal says why st turns away an item it selects: its blockers, a
// Status set after they closed, or its age; empty when st offers it.
func refusal(st config.BoardStep, it Item, now time.Time) string {
	switch {
	case st.Unblocked && it.Blockers == 0:
		return st.Name + " takes items with recorded blockers, it has none"
	case st.Unblocked && it.OpenBlockers > 0:
		return it.HeldText()
	case st.Unblocked && it.HeldByStatus():
		return fmt.Sprintf("%s %s, set %s after its last blocker closed %s: it holds until the Status changes",
			HeldPrefix, it.Status, it.StatusSet.Format(time.DateOnly), it.BlockersClosed.Format(time.DateOnly))
	case st.CreatedWithin.Duration > 0 && now.Sub(it.Created) > st.CreatedWithin.Duration:
		return fmt.Sprintf("created %s: %s takes items created within %s", it.Created.Format(time.DateOnly), st.Name, span(st.CreatedWithin.Duration))
	}
	return ""
}

func blockersOpen(it Item) string {
	return fmt.Sprintf("%d of %d blockers open", it.OpenBlockers, it.Blockers)
}

// span is d in days when it is whole days.
func span(d time.Duration) string {
	if day := 24 * time.Hour; d%day == 0 {
		return fmt.Sprintf("%d days", d/day)
	}
	return d.String()
}

func skipReason(it Item, b config.Board, now time.Time) string {
	if it.OpenBlockers > 0 {
		return it.HeldText()
	}
	if d := b.StaleAfter.Duration; d > 0 && now.Sub(it.Updated) > d {
		return "no activity since " + it.Updated.Format(time.DateOnly)
	}
	others := slices.DeleteFunc(slices.Clone(it.Assignees), func(a string) bool { return containsFold(b.People, a) })
	if len(others) > 0 {
		return "assigned to " + strings.Join(others, ", ")
	}
	return ""
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// Resolve returns the one value of values that in stands for: the value
// itself, or the only one whose words (case, emoji and punctuation
// ignored) begin with or contain in's. Anything else is an error that
// lists the values.
func Resolve(field string, values []string, in string) (string, error) {
	want := words(in)
	if want != "" {
		for _, match := range []func(string) bool{
			func(v string) bool { return v == want },
			func(v string) bool { return strings.HasPrefix(v, want) },
			func(v string) bool { return strings.Contains(v, want) },
		} {
			var hits []string
			for _, v := range values {
				if match(words(v)) {
					hits = append(hits, v)
				}
			}
			if len(hits) == 1 {
				return hits[0], nil
			}
			if len(hits) > 1 {
				return "", fmt.Errorf("%s %q is ambiguous (%s); the values: %s", field, in, strings.Join(hits, ", "), strings.Join(values, ", "))
			}
		}
	}
	return "", fmt.Errorf("%s %q is none of the values: %s", field, in, strings.Join(values, ", "))
}

// words is s lower-cased with everything but letters and digits as single
// spaces.
func words(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// ResolveOrder returns order with its Status and Kind values replaced by
// the board's canonical ones.
func ResolveOrder(order []config.BoardStep, statuses, kinds []string) ([]config.BoardStep, error) {
	out := make([]config.BoardStep, len(order))
	for i, st := range order {
		st.Status = slices.Clone(st.Status)
		st.Kind = slices.Clone(st.Kind)
		for field, vals := range map[string]struct {
			list   []string
			values []string
		}{StatusField: {st.Status, statuses}, KindField: {st.Kind, kinds}} {
			for j, v := range vals.list {
				r, err := Resolve(field, vals.values, v)
				if err != nil {
					return nil, fmt.Errorf("board.order %q: %w", st.Name, err)
				}
				vals.list[j] = r
			}
		}
		out[i] = st
	}
	return out, nil
}

// Ref parses owner/repo#n or an issue's GitHub URL.
func Ref(s string) (owner, repo string, n int, err error) {
	t := strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "github.com/")
	t = strings.Replace(t, "/issues/", "#", 1)
	path, num, ok := strings.Cut(t, "#")
	owner, repo, ok2 := strings.Cut(path, "/")
	if _, err := fmt.Sscanf(num, "%d", &n); !ok || !ok2 || err != nil || owner == "" || repo == "" || strings.Contains(repo, "/") || fmt.Sprint(n) != num {
		return "", "", 0, fmt.Errorf("%q is not an issue: owner/repo#n or its URL", s)
	}
	return owner, repo, n, nil
}

// refRepo is the repository (owner/repo) of the issue ref, "" when ref is
// no issue reference.
func refRepo(ref string) string {
	owner, repo, _, err := Ref(ref)
	if err != nil {
		return ""
	}
	return owner + "/" + repo
}
