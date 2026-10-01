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
	Ref       string    `json:"ref"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Status    string    `json:"status,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Assignees []string  `json:"assignees,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	// Blockers counts the recorded blockers, OpenBlockers the open ones.
	Blockers     int `json:"blockers,omitempty"`
	OpenBlockers int `json:"openBlockers,omitempty"`
	// OpenSubIssues counts the open sub-issues; SubIssues are they, read
	// for a SubIssues step only.
	OpenSubIssues int    `json:"openSubIssues,omitempty"`
	SubIssues     []Item `json:"subIssues,omitempty"`
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
	return s
}

// Rank returns the candidates of every step in order, each item once, in
// the first step that matches it. A SubIssues step offers an item with open
// sub-issues through them (its remainder) and is the item's only step; an
// item without is offered itself. A sub-issue that is a board item is held
// to the order like any other: one no step offers on its own (an old
// Backlog item, a blocked one) is marked skipped with the reason. Items
// without activity for b.StaleAfter and items assigned to anybody outside
// b.People are marked skipped too.
func Rank(snap *Snapshot, b config.Board, now time.Time) []Candidate {
	onBoard := make(map[string]Item, len(snap.Items))
	for _, it := range snap.Items {
		onBoard[it.Ref] = it
	}
	seen := map[string]bool{}
	var out []Candidate
	offer := func(it Item, step, epic, skip string) {
		if seen[it.Ref] {
			return
		}
		seen[it.Ref] = true
		out = append(out, Candidate{Item: it, Step: step, Epic: epic, Skip: cmp.Or(skip, skipReason(it, b, now))})
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
			if !st.SubIssues || it.OpenSubIssues == 0 {
				offer(it, st.Name, "", "")
				continue
			}
			seen[it.Ref] = true
			for _, sub := range it.SubIssues {
				skip := ""
				if bi, ok := onBoard[sub.Ref]; ok {
					sub, skip = bi, Unoffered(snap.Order, bi, now)
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

// refusal says why st turns away an item it selects: its blockers or its
// age; empty when st offers it.
func refusal(st config.BoardStep, it Item, now time.Time) string {
	switch {
	case st.Unblocked && it.Blockers == 0:
		return st.Name + " takes items with recorded blockers, it has none"
	case st.Unblocked && it.OpenBlockers > 0:
		return fmt.Sprintf("%d of %d blockers open", it.OpenBlockers, it.Blockers)
	case st.CreatedWithin.Duration > 0 && now.Sub(it.Created) > st.CreatedWithin.Duration:
		return fmt.Sprintf("created %s: %s takes items created within %s", it.Created.Format(time.DateOnly), st.Name, span(st.CreatedWithin.Duration))
	}
	return ""
}

// span is d in days when it is whole days.
func span(d time.Duration) string {
	if day := 24 * time.Hour; d%day == 0 {
		return fmt.Sprintf("%d days", d/day)
	}
	return d.String()
}

func skipReason(it Item, b config.Board, now time.Time) string {
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
