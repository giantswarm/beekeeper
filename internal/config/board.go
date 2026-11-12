package config

import (
	"slices"
	"strings"
	"time"
)

// Plans configures the plans repositories: a note for the guide's person
// that links one of their pull requests is refused while the pull request
// is closed or its stage check is pending, missing or red for anything but
// missing stage outputs (the grill stage), and an epic's plan is
// the open or merged pull request of theirs whose description names it.
type Plans struct {
	// Repositories are owner/repo; empty, no note is checked and no epic
	// has a plan.
	Repositories []string `yaml:"repositories"`
	// Check is the name of the stage check (default plan-stages).
	Check string `yaml:"check"`
}

// DefaultPlansCheck is the stage check's name unless plans.check says
// otherwise.
const DefaultPlansCheck = "plan-stages"

// Covers reports whether repo (owner/repo, any case) is a plans repository.
func (p Plans) Covers(repo string) bool {
	return slices.ContainsFunc(p.Repositories, func(r string) bool { return strings.EqualFold(r, repo) })
}

// Board is a GitHub project board and the order its work is picked in.
type Board struct {
	// Owner is the organisation and Project the number of its board;
	// unset, the board commands refuse.
	Owner   string `yaml:"owner"`
	Project int    `yaml:"project"`
	// Team is the value of the board's Team field whose items are the
	// desk's; empty, every item is.
	Team string `yaml:"team"`
	// TeamLabel is read and ignored so an existing configuration loads.
	TeamLabel string `yaml:"teamLabel"`
	// People are the logins whose assigned items are free; an item
	// assigned to anybody else is skipped.
	People []string `yaml:"people"`
	// StaleAfter skips an item without activity for longer (default a
	// year).
	StaleAfter Duration `yaml:"staleAfter"`
	// Order is the picking order: the first free item of the first step
	// that offers one is picked.
	Order []BoardStep `yaml:"order"`
	// Epics is the epic gate, whatever the order says.
	Epics EpicGate `yaml:"epics"`
	// ArchiveAfter is how long a closed item stays on the board before
	// board tidy archives it (default 336h, two weeks; negative: never,
	// closed items are still moved to Done). TidyEvery is how often the
	// supervisor loop runs board tidy (default 1h; negative: never).
	ArchiveAfter Duration `yaml:"archiveAfter"`
	TidyEvery    Duration `yaml:"tidyEvery"`
	// EscalateEvery is how long board escalate waits before it looks at a
	// blocked item again; it asks the person once more only when what holds
	// the item changed since its note (default 168h, a week; negative: never
	// escalate).
	EscalateEvery Duration `yaml:"escalateEvery"`
	// Claims publishes the desk's session records on their issues, for
	// the other desks working the same repositories.
	Claims BoardClaims `yaml:"claims"`
}

// BoardClaims is a desk's claims on GitHub: each issue a session record
// serves carries one marker comment of the desk (Desk), which board
// next and agents start on every other desk honour, and which the desk
// releases when the record ends.
type BoardClaims struct {
	// Off publishes no claim and reads none.
	Off bool `yaml:"off"`
	// Desk is the name the desk signs its claims with, which other desks
	// see on the issues: a neutral name, never a machine's (the issues may
	// be public). Unset, the desk publishes no claim and still honours the
	// other desks'.
	Desk string `yaml:"desk"`
	// Refresh is how often the desk refreshes a claim that still holds
	// (default 6h).
	Refresh Duration `yaml:"refresh"`
	// StaleAfter is how long another desk's claim holds unrefreshed
	// (default 24h): older, it is shown and not honoured.
	StaleAfter Duration `yaml:"staleAfter"`
}

// DefaultBoardArchiveAfter is board.archiveAfter when unset: two weeks.
const DefaultBoardArchiveAfter = 14 * 24 * time.Hour

// EpicGate is the epic gate: an epic in a Planning status is in planning,
// neither it nor any issue under it offered; moving it to Go is the go,
// from which its open sub-issues are offered. Each value is the board's
// value or an unambiguous part of it; an unset one is its default, which
// turns the gate off on a board without the value.
type EpicGate struct {
	// Kind are the Kind values of an epic (default epic).
	Kind []string `yaml:"kind"`
	// Planning are the statuses of an epic in planning (default up next).
	Planning []string `yaml:"planning"`
	// Go is the status of the go (default in progress).
	Go string `yaml:"go"`
}

// BoardStep is one step of the picking order: the open board items whose
// status, kind and labels match (each list matches any of its values, an
// empty one everything), or the open issues of a GitHub search.
type BoardStep struct {
	// WIPExempt permits new issues through the delivery gate, still counted.
	WIPExempt bool `yaml:"wipExempt"`
	// Name says why an item of the step is picked.
	Name string `yaml:"name"`
	// Status and Kind are the board's Status and Kind values; an
	// unambiguous part of one ("up next") stands for it. Kind takes an
	// item without a Kind by a label naming one: bug or kind/bug for Bug.
	Status []string `yaml:"status"`
	Kind   []string `yaml:"kind"`
	Labels []string `yaml:"labels"`
	// SubIssues offers the open sub-issues of a matching item that has
	// any (an epic's remainder) instead of the item, in their order.
	SubIssues bool `yaml:"subIssues"`
	// Unblocked offers only items with recorded blockers, all of them
	// closed, whose Status was set before the last of them closed: a
	// blocked item whose blocker cleared. A Status set after it holds the
	// item until it changes.
	Unblocked bool `yaml:"unblocked"`
	// CreatedWithin offers only items created within the duration.
	CreatedWithin Duration `yaml:"createdWithin"`
	// Search is a GitHub issue search whose open issues the step offers
	// (oldest first) instead of board items.
	Search string `yaml:"search"`
	// WorkScope is read and ignored so an existing configuration loads.
	WorkScope bool `yaml:"workScope"`
}
