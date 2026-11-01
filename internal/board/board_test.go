package board

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const (
	inProgress = "In Progress ⛏️"
	blocked    = "Blocked / Waiting ⛔️"
	upNext     = "Up Next ➡️"
	backlog    = "Backlog 📦"
	epic       = "Epic 🎯"
	bug        = "Bug 🐞"
	upNextEpic = "Up Next epic"
	bugStep    = "bug"
	typedNext  = "up next"
	// backlogStep is the name of the order's Backlog step.
	backlogStep = "Backlog"
)

var statuses = []string{"Inbox 📥", backlog, upNext, inProgress, blocked, "Validation ☑️", "Done ✅"}

func days(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }

// order is a desk's order, written the way a person types it.
func order() []config.BoardStep {
	return []config.BoardStep{
		{Name: "page", Kind: []string{"alert"}},
		{Name: "In Progress", Status: []string{"in progress"}, SubIssues: true},
		{Name: "blocker cleared", Status: []string{"blocked"}, Unblocked: true},
		{Name: "Up Next", Status: []string{typedNext}},
		{Name: bugStep, Kind: []string{bugStep}},
		{Name: "queue", Search: "repo:o/tools label:team/x"},
		{Name: backlogStep, Status: []string{"backlog"}, CreatedWithin: config.Duration{Duration: 90 * 24 * time.Hour}},
	}
}

func deskBoard() config.Board {
	return config.Board{Owner: "o", Project: 7, Team: "X", People: []string{"me"}, StaleAfter: config.Duration{Duration: 365 * 24 * time.Hour}, Order: order()}
}

// fixture is a board of every case the order distinguishes, in the board's
// order.
func fixture() []Item {
	it := func(n int, status, kind string, created, updated int) Item {
		// Every item of the fixture with a Status is a board item.
		return Item{Ref: fmt.Sprintf("o/r#%d", n), URL: fmt.Sprintf("https://github.com/o/r/issues/%d", n), Title: fmt.Sprintf("item %d", n),
			OnBoard: status != "", Status: status, Kind: kind, Created: days(created), Updated: days(updated)}
	}
	stale := it(1, inProgress, "", 800, 400)
	theirs := it(2, inProgress, "", 30, 1)
	theirs.Assignees = []string{"pat"}
	mine := it(3, inProgress, "", 30, 1)
	mine.Assignees = []string{"Me"}
	waiting := it(19, inProgress, "", 30, 1)
	waiting.Blockers, waiting.OpenBlockers = 1, 1
	inProgressEpic := it(4, inProgress, epic, 60, 1)
	// A sub-issue off the board with its recorded blockers open.
	waitingSub := it(41, "", "", 50, 2)
	waitingSub.Blockers, waitingSub.OpenBlockers = 4, 4
	inProgressEpic.OpenSubIssues = 2
	inProgressEpic.SubIssues = []Item{it(40, "", "", 50, 2), waitingSub}
	stillBlocked := it(5, blocked, "", 30, 1)
	stillBlocked.Blockers, stillBlocked.OpenBlockers = 2, 1
	cleared := it(6, blocked, "", 30, 1)
	cleared.Blockers = 1
	waitingOnPeople := it(7, blocked, "", 30, 1)
	epicItem := it(8, upNext, epic, 20, 1)
	epicItem.OpenSubIssues = 2
	epicItem.SubIssues = []Item{it(80, "", "", 10, 1), it(3, inProgress, "", 30, 1)}
	closableEpic := it(9, upNext, epic, 20, 1)
	// A fresh Backlog item a step offers, its recorded blockers open.
	blockedSlice := it(20, backlog, "", 5, 1)
	blockedSlice.Blockers, blockedSlice.OpenBlockers = 4, 3
	// The sub-issues of an epic in progress are board items of their own:
	// a fresh and an old one in Backlog, one in Inbox, the blocked slice.
	// Those without a Team (21, 22, 23: in Inbox, old in Backlog, still
	// blocked) are missing from the team's snapshot, held to the order all
	// the same.
	teamless := it(23, blocked, "", 5, 1)
	teamless.Blockers, teamless.OpenBlockers = 2, 1
	mixedEpic := it(15, inProgress, epic, 200, 1)
	mixedEpic.OpenSubIssues = 7
	mixedEpic.SubIssues = []Item{it(16, backlog, "", 20, 1), it(17, backlog, "", 99, 1), it(18, statuses[0], "", 5, 1), blockedSlice,
		it(21, statuses[0], "", 5, 1), it(22, backlog, "", 99, 1), teamless}
	return []Item{stale, theirs, mine, waiting, inProgressEpic, stillBlocked, cleared, waitingOnPeople, epicItem, closableEpic,
		it(10, upNext, "", 20, 1), it(11, backlog, bug, 400, 3), it(12, backlog, "", 100, 1), it(13, backlog, "", 10, 1), it(14, "Done ✅", "", 10, 1),
		mixedEpic, it(16, backlog, "", 20, 1), it(17, backlog, "", 99, 1), it(18, statuses[0], "", 5, 1), blockedSlice}
}

func snapshot(t *testing.T) *Snapshot {
	t.Helper()
	kinds := []string{epic, bug, "Alert 🚨"}
	o, err := ResolveOrder(order(), statuses, kinds)
	if err != nil {
		t.Fatal(err)
	}
	g, err := ResolveGate(config.EpicGate{}, statuses, kinds)
	if err != nil {
		t.Fatal(err)
	}
	return &Snapshot{Order: o, Items: fixture(), Statuses: statuses, Search: map[int][]Item{5: {{Ref: "o/tools#1", Created: days(5), Updated: days(1)}}}, Gate: g}
}

func TestRankAppliesTheOrderToAFixtureBoard(t *testing.T) {
	var got []string
	for _, c := range Rank(snapshot(t), deskBoard(), now) {
		line := c.Ref + " " + c.Step
		if c.Epic != "" {
			line += " < " + c.Epic
		}
		if c.Skip != "" {
			line += ": " + c.Skip
		}
		got = append(got, line)
	}
	want := []string{
		"o/r#1 In Progress: no activity since 2025-08-27",
		"o/r#2 In Progress: assigned to pat",
		"o/r#3 In Progress",
		"o/r#19 In Progress: held: 1 of 1 blockers open",
		"o/r#40 In Progress < o/r#4",
		"o/r#41 In Progress < o/r#4: held: 4 of 4 blockers open",
		"o/r#16 In Progress < o/r#15",
		"o/r#17 In Progress < o/r#15: created 2026-06-24: Backlog takes items created within 90 days",
		"o/r#18 In Progress < o/r#15: no step of board.order offers Inbox 📥",
		"o/r#20 In Progress < o/r#15: held: 3 of 4 blockers open",
		"o/r#21 In Progress < o/r#15: no step of board.order offers Inbox 📥",
		"o/r#22 In Progress < o/r#15: created 2026-06-24: Backlog takes items created within 90 days",
		"o/r#23 In Progress < o/r#15: held: 1 of 2 blockers open",
		"o/r#6 blocker cleared",
		"o/r#8 Up Next: epic in planning (Up Next ➡️), no plan pull request names it: its planning session's",
		"o/r#9 Up Next: epic in planning (Up Next ➡️), no plan pull request names it: its planning session's",
		"o/r#10 Up Next",
		"o/r#11 bug",
		"o/tools#1 queue",
		"o/r#13 Backlog",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rank:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestKindStepTakesAnItemWithoutAKindByItsLabel(t *testing.T) {
	snap := snapshot(t)
	it := func(n int, kind string, labels ...string) Item {
		return Item{Ref: fmt.Sprintf("o/r#%d", n), OnBoard: true, Status: backlog, Kind: kind, Labels: labels, Created: days(5), Updated: days(1)}
	}
	snap.Items = []Item{
		it(1, ""),
		it(2, "Feature ✨", "bug"),
		it(3, "", "team/x", "kind/bug"),
		it(4, "", "Bug"),
		it(5, bug),
		it(6, "", "bugfix"),
	}
	snap.Search = nil
	var got []string
	for _, c := range Rank(snap, deskBoard(), now) {
		got = append(got, c.Ref+" "+c.Step+": "+c.Match)
	}
	want := []string{
		"o/r#3 bug: kind Bug 🐞 by label kind/bug",
		"o/r#4 bug: kind Bug 🐞 by label Bug",
		"o/r#5 bug: kind Bug 🐞",
		"o/r#1 Backlog: status Backlog 📦, created within 90 days",
		"o/r#2 Backlog: status Backlog 📦, created within 90 days",
		"o/r#6 Backlog: status Backlog 📦, created within 90 days",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rank:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMatchNamesTheMatchersOfTheStep(t *testing.T) {
	snap := snapshot(t)
	var got []string
	for _, c := range Rank(snap, deskBoard(), now) {
		if slices.Contains([]string{"o/r#3", "o/r#40", "o/r#6", "o/r#11", "o/tools#1"}, c.Ref) {
			got = append(got, c.Ref+": "+c.Match)
		}
	}
	want := []string{
		"o/r#3: status In Progress ⛏️",
		"o/r#40: status In Progress ⛏️",
		"o/r#6: status Blocked / Waiting ⛔️, blockers closed",
		"o/r#11: kind Bug 🐞",
		"o/tools#1: search",
	}
	if !slices.Equal(got, want) {
		t.Errorf("match:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	step := config.BoardStep{Name: "labelled", Labels: []string{"Urgent"}}
	if m := matched(step, Item{Labels: []string{"team/x", "urgent"}}); m != "label urgent" {
		t.Errorf("labels step: %q", m)
	}
	if m := matched(config.BoardStep{Name: "all"}, Item{}); m != "every item" {
		t.Errorf("empty step: %q", m)
	}
}

func TestPositionIsThePriorityWithinAStep(t *testing.T) {
	// The board's items in position order: a rank that moves #12 above #10
	// makes it the first Up Next item, still after every In Progress one.
	const three, ten, twelve = "o/r#3", "o/r#10", "o/r#12"
	before, after := []string{three, ten, twelve}, []string{three, twelve, ten}
	snap := snapshot(t)
	it := func(n int, status string) Item {
		return Item{Ref: fmt.Sprintf("o/r#%d", n), OnBoard: true, Status: status, Created: days(5), Updated: days(1)}
	}
	for _, tc := range []struct {
		items []Item
		want  []string
	}{
		{[]Item{it(3, inProgress), it(10, upNext), it(12, upNext)}, before},
		{[]Item{it(12, upNext), it(3, inProgress), it(10, upNext)}, after},
	} {
		snap.Items, snap.Search = tc.items, nil
		var got []string
		for _, c := range Rank(snap, deskBoard(), now) {
			got = append(got, c.Ref)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("picks %v, want %v", got, tc.want)
		}
	}
}

func TestBlockersClosedIsTheLastCloseOfEveryBlocker(t *testing.T) {
	at := func(d int) *time.Time { t := days(d); return &t }
	j := func(total int, closed ...*time.Time) issueJSON {
		var i issueJSON
		i.Deps.TotalBlockedBy = total
		for _, c := range closed {
			i.BlockedBy.Nodes = append(i.BlockedBy.Nodes, blockerNode{ClosedAt: c})
		}
		return i
	}
	for _, tc := range []struct {
		name string
		j    issueJSON
		want time.Time
	}{
		{"all closed", j(2, at(5), at(2)), days(2)},
		{"one open", j(2, at(5), nil), time.Time{}},
		{"not every one read", j(3, at(5), at(2)), time.Time{}},
		{"none", j(0), time.Time{}},
	} {
		if got := tc.j.blockersClosed(); !got.Equal(tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUnofferedSaysWhyNoStepTakesAnItem(t *testing.T) {
	o := snapshot(t).Order
	for _, tc := range []struct {
		it   Item
		want string
	}{
		{Item{Status: backlog, Created: days(10)}, ""},
		{Item{Status: backlog, Kind: bug, Created: days(400)}, ""},
		{Item{Status: backlog, Created: days(100)}, "created 2026-06-23: Backlog takes items created within 90 days"},
		{Item{Status: blocked}, "blocker cleared takes items with recorded blockers, it has none"},
		{Item{Status: blocked, Blockers: 3, OpenBlockers: 2}, "held: 2 of 3 blockers open"},
		{Item{Status: blocked, Blockers: 1, StatusSet: days(1), BlockersClosed: days(2)},
			"held by its Status Blocked / Waiting ⛔️, set 2026-09-30 after its last blocker closed 2026-09-29: it holds until the Status changes"},
		{Item{Status: blocked, Blockers: 1, StatusSet: days(3), BlockersClosed: days(2)}, ""},
		{Item{Status: blocked, Blockers: 1, StatusSet: days(1)}, ""},
		{Item{}, "no step of board.order offers an item without a Status"},
	} {
		if got := Unoffered(o, tc.it, now); got != tc.want {
			t.Errorf("Unoffered(%s %s) = %q, want %q", tc.it.Status, tc.it.Kind, got, tc.want)
		}
	}
}

func TestCandidateWhy(t *testing.T) {
	c := Candidate{Item: Item{Status: upNext}, Step: upNextEpic, Epic: "o/r#8"}
	if got, want := c.Why(), "Up Next epic, sub-issue of o/r#8 (Up Next ➡️)"; got != want {
		t.Errorf("Why() = %q, want %q", got, want)
	}
}

func TestResolve(t *testing.T) {
	for in, want := range map[string]string{
		"Up Next ➡️": upNext, "up next": upNext, "UP-NEXT": upNext, "progress": inProgress, "blocked": blocked, "done": "Done ✅", "back": backlog,
	} {
		if got, err := Resolve(StatusField, statuses, in); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"Doing": `Status "Doing" is none of the values: Inbox 📥, Backlog 📦, Up Next ➡️`,
		"":      "is none of the values",
		"in":    `Status "in" is ambiguous (Inbox 📥, In Progress ⛏️); the values: Inbox 📥, Backlog 📦`,
	} {
		if _, err := Resolve(StatusField, statuses, in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve(%q) = %v, want %q", in, err, want)
		}
	}
	if _, err := ResolveOrder([]config.BoardStep{{Name: "x", Status: []string{"doing"}}}, statuses, nil); err == nil || !strings.Contains(err.Error(), `board.order "x": Status "doing"`) {
		t.Errorf("ResolveOrder with an unknown status = %v", err)
	}
}

func TestRef(t *testing.T) {
	for _, in := range []string{"o/r#12", "https://github.com/o/r/issues/12", "github.com/o/r/issues/12"} {
		if o, r, n, err := Ref(in); err != nil || o != "o" || r != "r" || n != 12 {
			t.Errorf("Ref(%q) = %s %s %d %v", in, o, r, n, err)
		}
	}
	for _, in := range []string{"o/r", "r#12", "o/r#x", "o/r/x#1", "o/r#12a", "https://github.com/o/r/pull/1"} {
		if _, _, _, err := Ref(in); err == nil {
			t.Errorf("Ref(%q) accepted", in)
		}
	}
}

func TestFilter(t *testing.T) {
	o, _ := ResolveOrder(order()[1:4], statuses, []string{epic})
	if got, want := filter(deskBoard(), o), `is:open team:"X" status:"In Progress ⛏️","Blocked / Waiting ⛔️","Up Next ➡️"`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
	// The epic gate's statuses are read too.
	if got, want := filter(deskBoard(), o[2:], upNext, inProgress), `is:open team:"X" status:"Up Next ➡️","In Progress ⛏️"`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
	// A step without statuses reads every status.
	if got, want := filter(config.Board{}, []config.BoardStep{{Name: bugStep, Kind: []string{bug}}}), "is:open"; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// fakeGH answers the board's GraphQL requests from a fixture and records the
// mutations, every request's query and the repository each acted on.
type fakeGH struct {
	t      *testing.T
	pages  []string
	extras string
	// extrasBy answers an extras request by its query, over extras.
	extrasBy func(q string) string
	item     string
	// added answers addProjectV2ItemById.
	added string
	// byIssue answers an issue's projectItems by its number, over item;
	// moved answers updateProjectV2ItemPosition.
	byIssue   map[string]string
	moved     string
	mutations []string
	queries   []string
	repos     []string
}

const metaJSON = `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{},
{"id":"FS","name":"Status","options":[{"id":"s1","name":"Backlog 📦"},{"id":"s2","name":"Up Next ➡️"},{"id":"s3","name":"In Progress ⛏️"}]},
{"id":"FK","name":"Kind","options":[{"id":"k1","name":"Epic 🎯"}]},
{"id":"FT","name":"Team","options":[{"id":"t1","name":"X 🐝"},{"id":"t2","name":"Y"}]}]}}}}}`

func (f *fakeGH) run(ctx context.Context, args ...string) ([]byte, error) {
	q := args[3]
	f.queries = append(f.queries, q)
	f.repos = append(f.repos, github.Repo(ctx))
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	switch {
	case strings.Contains(q, "addProjectV2ItemById"):
		f.mutations = append(f.mutations, "add "+vars["c"]+" to "+vars["p"])
		return []byte(f.added), nil
	case strings.Contains(q, "updateProjectV2ItemPosition"):
		f.mutations = append(f.mutations, fmt.Sprintf("%s after %q", vars["i"], vars["a"]))
		return []byte(cmp.Or(f.moved, `{"data":{"updateProjectV2ItemPosition":{"clientMutationId":null}}}`)), nil
	case strings.HasPrefix(q, "query=mutation"):
		f.mutations = append(f.mutations, vars["i"]+"="+vars["v"])
		return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"I"}}}}`), nil
	case strings.Contains(q, "fields(first:50)"):
		return []byte(metaJSON), nil
	case strings.Contains(q, "items(first:100"):
		if vars["q"] != `is:open team:"X" status:"In Progress ⛏️","Up Next ➡️"` {
			f.t.Errorf("board query %q", vars["q"])
		}
		page := 0
		if vars["c"] != "" {
			page = 1
		}
		return []byte(f.pages[page]), nil
	case f.extrasBy != nil && (strings.Contains(q, "subIssues(first:50)") || strings.Contains(q, "search(")):
		return []byte(f.extrasBy(q)), nil
	case strings.Contains(q, "subIssues(first:50)") || strings.Contains(q, "search("):
		if !strings.Contains(q, `e1:repository(owner:"o",name:"r"){issue(number:2){subIssues(first:50){nodes{...I projectItems(`) ||
			!strings.Contains(q, `s2:search(query:"repo:o/q is:issue is:open sort:created-asc"`) {
			f.t.Errorf("extras query %s", q)
		}
		return []byte(f.extras), nil
	case strings.Contains(q, "projectItems"):
		if a, ok := f.byIssue[vars["n"]]; ok {
			return []byte(a), nil
		}
		return []byte(f.item), nil
	}
	f.t.Fatalf("unexpected request %v", args)
	return nil, nil
}

func issue(n int, extra string) string {
	return fmt.Sprintf(`{"number":%d,"url":"https://github.com/o/r/issues/%d","title":"t%d","state":"OPEN","createdAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-30T00:00:00Z","repository":{"nameWithOwner":"o/r"}%s}`, n, n, n, extra)
}

func node(status, kind, content string) string {
	return fmt.Sprintf(`{"status":{"name":%q},"kind":%s,"content":%s}`, status, kind, content)
}

// readBoard is the board the reads take: the epics' sub-issues, In
// Progress, a search step and Up Next.
func readBoard() config.Board {
	return config.Board{Owner: "o", Project: 7, Team: "X", People: []string{"me"}, Order: []config.BoardStep{
		{Name: "epics", Status: []string{"in progress"}, Kind: []string{defaultEpicKind}, SubIssues: true},
		{Name: "In Progress", Status: []string{"progress"}},
		{Name: "queue", Search: "repo:o/q"},
		{Name: "Up Next", Status: []string{"next"}},
	}}
}

func TestReadPagesTheBoardAndReadsSubIssuesAndSearches(t *testing.T) {
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":true,"endCursor":"C"},"nodes":[` +
			node(inProgress, "null", issue(1, `,"assignees":{"nodes":[{"login":"pat"}]}`)) + "," +
			node(inProgress, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":3,"completed":1}`)) + "," +
			`{"status":{"name":"Up Next ➡️"},"content":{}}]}}}}}`,
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[` +
			node(upNext, "null", strings.Replace(issue(3, ""), "OPEN", "CLOSED", 1)) + "," + node(upNext, "null", issue(4, `,"parent":{"number":2,"repository":{"nameWithOwner":"o/r"}}`)) + `]}}}}}`,
	}, extras: `{"data":{"e1":{"issue":{"subIssues":{"nodes":[` +
		// 20 is on another board only, 22 on this one in Inbox without a Team.
		issue(20, `,"projectItems":{"nodes":[{"project":{"id":"Q"},"status":{"name":"Inbox 📥"}}]}`) + "," +
		strings.Replace(issue(21, ""), "OPEN", "CLOSED", 1) + "," +
		issue(22, `,"projectItems":{"nodes":[{"project":{"id":"Q"}},{"project":{"id":"P"},"status":{"name":"Inbox 📥"},"kind":null}]}`) +
		`]}}},"s2":{"nodes":[` + issue(30, "") + `]}}}`}
	b := readBoard()
	snap, err := (&Client{GH: f.run, Board: b}).Read(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range Rank(snap, b, now) {
		got = append(got, c.Ref+" "+c.Why()+" "+c.Skip)
	}
	want := []string{
		"o/r#20 epics, sub-issue of o/r#2 ",
		"o/r#22 epics, sub-issue of o/r#2 (Inbox 📥) no step of board.order offers Inbox 📥",
		"o/r#1 In Progress (In Progress ⛏️) assigned to pat",
		"o/r#30 queue ",
		"o/r#4 Up Next (Up Next ➡️) ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("read and ranked:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Every issue reads its parent; without plans repositories no plan is
	// searched for.
	if i := slices.IndexFunc(snap.Items, func(it Item) bool { return it.Ref == "o/r#4" }); i < 0 || snap.Items[i].Parent != "o/r#2" || snap.Plans != nil {
		t.Errorf("items %+v, plans %v", snap.Items, snap.Plans)
	}
	if es := snap.Epics(); len(es) != 1 || es[0].Ref != "o/r#2" || es[0].Status != inProgress || es[0].Plan != nil {
		t.Errorf("epics %+v", es)
	}
}

// itemsRead matches a read of a project's items; an issue's projectItems
// is none.
var itemsRead = regexp.MustCompile(`\bitems\s*\(`)

// readsItems says whether any of queries reads a project's items.
func readsItems(queries []string) bool {
	return slices.ContainsFunc(queries, itemsRead.MatchString)
}

// issue5 is the issue the moves act on.
const issue5 = "o/r#5"

// onBoard is an issue whose projectItems name its item I1 on the board, in
// Backlog, beside one on another board.
const onBoard = `{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[{"id":"other","project":{"id":"Q"}},{"id":"I1","project":{"id":"P"},"status":{"name":"Backlog 📦"},"team":{"name":"X 🐝"}}]}}}}}`

func TestMoveTakesOnlyTheBoardsStatuses(t *testing.T) {
	f := &fakeGH{t: t, item: onBoard}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	mv, err := c.Move(context.Background(), "https://github.com/o/r/issues/5", "in progress")
	if err != nil || *mv != (Moved{Ref: issue5, From: backlog, To: inProgress}) || !slices.Equal(f.mutations, []string{"I1=s3"}) {
		t.Fatalf("Move = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	// An item on the board is moved by the issue's projectItems: no add,
	// no read of the board's items, every call on the issue's repository.
	if len(f.queries) != 3 || readsItems(f.queries) || !slices.Equal(slices.Compact(slices.Clone(f.repos)), []string{"o/r"}) {
		t.Errorf("queries %q acting on %q", f.queries, f.repos)
	}
	_, err = c.Move(context.Background(), issue5, "Doing")
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "none of the values: Backlog 📦, Up Next ➡️, In Progress ⛏️") || len(f.mutations) != 1 {
		t.Errorf("Move to an unknown status = %v; mutations %v", err, f.mutations)
	}
	if mv, err := c.Move(context.Background(), issue5, "backlog"); err != nil || *mv != (Moved{Ref: issue5, From: backlog, To: backlog}) || len(f.mutations) != 1 {
		t.Errorf("Move to the item's own status = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	c.Board.Team = "nobody"
	if _, err := c.Move(context.Background(), issue5, "backlog"); !errors.As(err, &r) || !strings.Contains(r.Reason, `board.team: Team "nobody" is none of the values: X 🐝, Y`) || len(f.mutations) != 1 {
		t.Errorf("Move with a board.team the board has not = %v; mutations %v", err, f.mutations)
	}
	if _, err := (&Client{GH: f.run}).Move(context.Background(), issue5, "backlog"); err == nil || !strings.Contains(err.Error(), "no board configured") {
		t.Errorf("Move without a board = %v", err)
	}
}

func TestMoveAddsAnIssueMissingFromTheBoard(t *testing.T) {
	// The issue is on another board only; the add answers a new item.
	f := &fakeGH{t: t, item: `{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[{"id":"other","project":{"id":"Q"}}]}}}}}`,
		added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I2","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	mv, err := c.Move(context.Background(), issue5, "backlog")
	if err != nil || *mv != (Moved{Ref: issue5, To: backlog, Added: true, Team: teamX}) || !slices.Equal(f.mutations, []string{"add ISSUE to P", "I2=t1", "I2=s1"}) || readsItems(f.queries) {
		t.Errorf("Move = %+v, %v; mutations %v, queries %q", mv, err, f.mutations, f.queries)
	}
	// A board without a Team, or none configured, adds without one.
	f.mutations = nil
	c.Board.Team = ""
	if mv, err := c.Move(context.Background(), issue5, "backlog"); err != nil || *mv != (Moved{Ref: issue5, To: backlog, Added: true}) || !slices.Equal(f.mutations, []string{"add ISSUE to P", "I2=s1"}) {
		t.Errorf("Move without board.team = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	// An add GitHub refuses (a login that does not reach the issue) is an
	// error with its reason, nothing moved.
	f.mutations = nil
	f.added = `{"data":{"addProjectV2ItemById":null},"errors":[{"message":"Could not resolve to a node with the global id of 'ISSUE'."}]}`
	if _, err := c.Move(context.Background(), issue5, "backlog"); err == nil || !strings.Contains(err.Error(), "adding o/r#5 to the board o/7: Could not resolve to a node") || len(f.mutations) != 1 {
		t.Errorf("Move whose add is refused = %v; mutations %v", err, f.mutations)
	}
}

// labIssue7 is an issue of a personal account's repository, outside the
// board's owner o.
const labIssue7 = "teemow/lab#7"

// homeItem is an item of the issue on project number of owner, whose
// Status field FU has the options u1 Backlog and u2 In Progress.
func homeItem(id, project, owner string, number int, status string) string {
	st := "null"
	if status != "" {
		st = fmt.Sprintf(`{"name":%q}`, status)
	}
	return fmt.Sprintf(`{"id":%q,"project":{"id":%q},"status":%s,"home":{"number":%d,"owner":{"login":%q},
"status":{"id":"FU","options":[{"id":"u1","name":"Backlog"},{"id":"u2","name":"In Progress"}]}}}`, id, project, st, number, owner)
}

func TestMoveAddsAnIssueOutsideTheBoardOwner(t *testing.T) {
	for _, owner := range []string{"o", personalOwner} {
		for _, nodes := range []string{"", homeItem("U1", "Q", "teemow", 1, "Backlog"), homeItem("U1", "Q", "other", 1, "Backlog") + "," + homeItem("U2", "R", "teemow", 2, "Backlog")} {
			t.Run(owner+nodes, func(t *testing.T) {
				f := &fakeGH{t: t, item: `{"data":{"repository":{"issue":{"id":"I7","projectItems":{"nodes":[` + nodes + `]}}}}}`,
					added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I3","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`}
				c := &Client{GH: f.run, Board: config.Board{Owner: owner, Project: 7, Team: "X"}}
				mv, err := c.Move(context.Background(), "https://github.com/teemow/lab/issues/7", "progress")
				if err != nil || *mv != (Moved{Ref: labIssue7, To: inProgress, Added: true, Team: teamX}) || !slices.Equal(f.mutations, []string{addIssue7, "I3=t1", "I3=s3"}) {
					t.Fatalf("Move = %+v, %v; mutations %v", mv, err, f.mutations)
				}
				if readsItems(f.queries) || !slices.Equal(slices.Compact(slices.Clone(f.repos)), []string{labRepo}) {
					t.Errorf("queries %q acting on %q", f.queries, f.repos)
				}
			})
		}
	}
}

func TestMoveCrossOwnerUsesGitHubPermissions(t *testing.T) {
	f := &fakeGH{t: t, item: issue7OffBoard,
		added: `{"data":{"addProjectV2ItemById":null},"errors":[{"message":"Resource not accessible by integration"}]}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	var refused *Refusal
	if _, err := c.Move(context.Background(), labIssue7, "unknown"); !errors.As(err, &refused) || len(f.mutations) != 0 {
		t.Fatalf("unknown status: %v; mutations %v", err, f.mutations)
	}
	if _, err := c.Move(context.Background(), labIssue7, "progress"); err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") || !slices.Equal(f.mutations, []string{addIssue7}) {
		t.Fatalf("permission denied: %v; mutations %v", err, f.mutations)
	}
	f.mutations = nil
	f.item = `{"data":{"repository":{"issue":{"id":"I7","projectItems":{"nodes":[{"id":"I3","project":{"id":"P"},"status":{"name":"Backlog 📦"},"team":{"name":"X 🐝"}}]}}}}}`
	if mv, err := c.Move(context.Background(), labIssue7, "progress"); err != nil || *mv != (Moved{Ref: labIssue7, From: backlog, To: inProgress}) || !slices.Equal(f.mutations, []string{"I3=s3"}) {
		t.Fatalf("existing item: %+v, %v; mutations %v", mv, err, f.mutations)
	}
	f.mutations = nil
	f.item = `{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository"}]}`
	if _, err := c.Move(context.Background(), labIssue7, "progress"); !errors.As(err, &refused) || !strings.Contains(err.Error(), "is no issue: Could not resolve") || len(f.mutations) != 0 {
		t.Fatalf("unresolved issue: %v; mutations %v", err, f.mutations)
	}
}

// issue9 is the issue the ranks place an item against, labRepo a
// personal account's repository.
const (
	issue9  = "o/r#9"
	labRepo = "teemow/lab"
)

// item9 is the issue the ranks place an item against: its item I9 is on
// the board.
const item9 = `{"data":{"repository":{"issue":{"id":"ISSUE9","projectItems":{"nodes":[{"id":"I9","project":{"id":"P"},"status":{"name":"Up Next ➡️"},"team":{"name":"X 🐝"}}]}}}}}`

func TestRankMovesTheItemByItsPosition(t *testing.T) {
	f := &fakeGH{t: t, item: onBoard, byIssue: map[string]string{"9": item9},
		moved: `{"data":{"updateProjectV2ItemPosition":{"items":{"nodes":[{"id":"I1","content":{"number":5,"repository":{"nameWithOwner":"o/r"}}},{"id":"I9","content":{"number":9,"repository":{"nameWithOwner":"o/r"}}}]}}}}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	ctx := context.Background()
	rk, err := c.Rank(ctx, issue5, Place{Top: true})
	if err != nil || *rk != (Ranked{Ref: issue5, Top: true, Below: issue9}) || !slices.Equal(f.mutations, []string{`I1 after ""`}) {
		t.Errorf("Rank top = %+v, %v; mutations %v", rk, err, f.mutations)
	}
	if !strings.Contains(f.queries[len(f.queries)-1], "updateProjectV2ItemPosition(input:{projectId:$p,itemId:$i}){clientMutationId items(first:2)") {
		t.Errorf("a move to the top sends no afterId and reads the two top items: %s", f.queries[len(f.queries)-1])
	}
	f.mutations, f.moved = nil, ""
	rk, err = c.Rank(ctx, issue5, Place{After: "https://github.com/o/r/issues/9"})
	if err != nil || *rk != (Ranked{Ref: issue5, Above: issue9}) || !slices.Equal(f.mutations, []string{`I1 after "I9"`}) {
		t.Errorf("Rank after = %+v, %v; mutations %v", rk, err, f.mutations)
	}
	// Before is the item after the other one, then the other one after the
	// item: the other one's predecessor is never read.
	f.mutations = nil
	rk, err = c.Rank(ctx, issue5, Place{Before: issue9})
	if err != nil || *rk != (Ranked{Ref: issue5, Below: issue9}) || !slices.Equal(f.mutations, []string{`I1 after "I9"`, `I9 after "I1"`}) {
		t.Errorf("Rank before = %+v, %v; mutations %v", rk, err, f.mutations)
	}
	if readsItems(slices.DeleteFunc(slices.Clone(f.queries), func(q string) bool { return strings.Contains(q, "items(first:2)") })) {
		t.Errorf("a rank read the board's items: %q", f.queries)
	}
}

func TestRankAddsAMissingIssueAndRefusesWhatIsNotOnTheBoard(t *testing.T) {
	notOnBoard := `{"data":{"repository":{"issue":{"id":"ISSUE8","projectItems":{"nodes":[{"id":"other","project":{"id":"Q"}}]}}}}}`
	f := &fakeGH{t: t, item: notOnBoard, byIssue: map[string]string{"9": item9},
		added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I8","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	ctx := context.Background()
	rk, err := c.Rank(ctx, "o/s#8", Place{After: issue9})
	if err != nil || *rk != (Ranked{Ref: "o/s#8", Above: issue9, Added: true, Team: teamX}) ||
		!slices.Equal(f.mutations, []string{"add ISSUE8 to P", "I8=t1", `I8 after "I9"`}) {
		t.Errorf("Rank of an issue missing from the board = %+v, %v; mutations %v", rk, err, f.mutations)
	}
	// The other issue is read on its own repository, the ranked one on its.
	if !slices.Contains(f.repos, "o/r") || f.repos[0] != "o/s" {
		t.Errorf("acting on %q", f.repos)
	}
	var r *Refusal
	for _, tc := range []struct {
		at   Place
		ref  string
		want string
	}{
		{Place{Before: "o/r#3"}, issue5, "o/r#3 is not on the board o/7"},
		{Place{After: "https://github.com/o/r/issues/5"}, issue5, "o/r#5 cannot be placed against itself"},
		{Place{Top: true}, "o/r#4", "o/r#4 is no issue: Could not resolve to an Issue with the number of 4."},
	} {
		f.mutations = nil
		f.byIssue = map[string]string{"3": notOnBoard, "7": notOnBoard, "9": item9, "4": `{"data":{"repository":{"issue":null}},"errors":[{"message":"Could not resolve to an Issue with the number of 4."}]}`}
		f.item = onBoard
		if _, err := c.Rank(ctx, tc.ref, tc.at); !errors.As(err, &r) || r.Reason != tc.want || len(f.mutations) != 0 {
			t.Errorf("Rank(%s, %+v) = %v, want refusal %q; mutations %v", tc.ref, tc.at, err, tc.want, f.mutations)
		}
	}
	for _, at := range []Place{{}, {Top: true, After: issue9}, {Before: issue9, After: issue9}} {
		if _, err := c.Rank(ctx, issue5, at); err == nil || !strings.Contains(err.Error(), "one of top, before and after") {
			t.Errorf("Rank(%+v) = %v", at, err)
		}
	}
}

func TestRankAddsAnIssueOutsideTheBoardOwner(t *testing.T) {
	// An organisation's issue on a personal account's board, and the reverse:
	// both are added as Move adds them, GitHub deciding whether they may be.
	for _, tc := range []struct{ owner, ref, repo string }{
		{personalOwner, "org/engine#7", "org/engine"},
		{"o", labIssue7, labRepo},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			f := &fakeGH{t: t, item: issue7OffBoard, byIssue: map[string]string{"9": item9},
				added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I3","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`}
			c := &Client{GH: f.run, Board: config.Board{Owner: tc.owner, Project: 7, Team: "X"}}
			rk, err := c.Rank(context.Background(), tc.ref, Place{After: issue9})
			if err != nil || *rk != (Ranked{Ref: tc.ref, Above: issue9, Added: true, Team: teamX}) ||
				!slices.Equal(f.mutations, []string{addIssue7, "I3=t1", `I3 after "I9"`}) {
				t.Fatalf("Rank = %+v, %v; mutations %v", rk, err, f.mutations)
			}
			if f.repos[0] != tc.repo {
				t.Errorf("acting on %q, want %s first", f.repos, tc.repo)
			}
		})
	}
	// GitHub refusing the add is the refusal, nothing else written.
	f := &fakeGH{t: t, item: issue7OffBoard, byIssue: map[string]string{"9": item9},
		added: `{"data":{"addProjectV2ItemById":null},"errors":[{"message":"Resource not accessible by integration"}]}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: personalOwner, Project: 7, Team: "X"}}
	if _, err := c.Rank(context.Background(), "org/engine#7", Place{Top: true}); err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") || !slices.Equal(f.mutations, []string{addIssue7}) {
		t.Errorf("Rank whose add GitHub refuses = %v; mutations %v", err, f.mutations)
	}
}

func TestOpenActsOnTheRepository(t *testing.T) {
	f := &fakeGH{t: t}
	c := &Client{GH: func(ctx context.Context, args ...string) ([]byte, error) {
		f.repos = append(f.repos, github.Repo(ctx))
		return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN"}}}}`), nil
	}}
	if open, err := c.Open(context.Background(), "teemow/lab#7"); err != nil || !open || !slices.Equal(f.repos, []string{labRepo}) {
		t.Errorf("Open = %v, %v acting on %q", open, err, f.repos)
	}
}

func TestGraphQLErrorsAreReported(t *testing.T) {
	gh := func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"data":null,"errors":[{"message":"Could not resolve to a ProjectV2"}]}`), nil
	}
	_, err := (&Client{GH: gh, Board: deskBoard()}).Read(context.Background(), now)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve to a ProjectV2") {
		t.Errorf("Read = %v", err)
	}
}

func TestItemLeases(t *testing.T) {
	it := Item{Labels: []string{"team/bumblebee", "Lease/AgentLab-1", "lease/", "lease/graveler"}}
	if got := it.Leases(); !slices.Equal(got, []string{"agentlab-1", "graveler"}) {
		t.Errorf("leases %q", got)
	}
}

func TestClosesReadsTheClosingReferencesInOneRequestPerOwner(t *testing.T) {
	var queries, acting []string
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		queries = append(queries, args[3])
		acting = append(acting, github.Repo(ctx))
		if strings.Contains(args[3], `owner:"teemow"`) {
			// A personal account's repository resolves only on the login
			// that reaches it: acting on it (github.ActingOn).
			if github.Repo(ctx) != "teemow/lab" {
				return []byte(`{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository with the name 'teemow/lab'."}]}`), errors.New("exit status 1")
			}
			return []byte(`{"data":{"r0":{"issueOrPullRequest":{"closingIssuesReferences":{"nodes":[{"number":3,"repository":{"nameWithOwner":"teemow/lab"}}]}}}}}`), nil
		}
		return []byte(`{"data":{
			"r0":{"issueOrPullRequest":{"closingIssuesReferences":{"nodes":[{"number":1,"repository":{"nameWithOwner":"O/R"}},{"number":2,"repository":{"nameWithOwner":"other/x"}}]}}},
			"r1":{"issueOrPullRequest":{}}}}`), nil
	}
	got, err := (&Client{GH: gh}).Closes(t.Context(), []string{"O/R#100", "teemow/lab#9", issue5, "not a ref"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"o/r#100": {ref1, "other/x#2"}, "teemow/lab#9": {"teemow/lab#3"}}
	if len(queries) != 2 || !strings.Contains(queries[0], `r1:repository(owner:"o",name:"r")`) || len(got) != len(want) ||
		!slices.Equal(got["o/r#100"], want["o/r#100"]) || !slices.Equal(got["teemow/lab#9"], want["teemow/lab#9"]) {
		t.Errorf("closes %v, queries %q", got, queries)
	}
	if !slices.Equal(acting, []string{"O/R", "teemow/lab"}) {
		t.Errorf("requests acted on %q, want one per owner", acting)
	}
	queries = nil
	if got, err := (&Client{GH: gh}).Closes(t.Context(), nil); err != nil || got != nil || len(queries) != 0 {
		t.Errorf("no refs asked GitHub: %v, %v, %d queries", got, err, len(queries))
	}
}

func TestClosesAsksInBatches(t *testing.T) {
	var queries []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		queries = append(queries, args[3])
		return []byte(`{"data":{"r0":{"issueOrPullRequest":{"closingIssuesReferences":{"nodes":[{"number":1,"repository":{"nameWithOwner":"o/r"}}]}}}}}`), nil
	}
	refs := make([]string, 2*closesBatch+1)
	for i := range refs {
		refs[i] = fmt.Sprintf("o/r#%d", i+100)
	}
	got, err := (&Client{GH: gh}).Closes(t.Context(), refs)
	if err != nil {
		t.Fatal(err)
	}
	// Each request's first ref closes o/r#1; a request holds closesBatch refs.
	if len(queries) != 3 || strings.Count(queries[0], "repository(") != closesBatch || strings.Count(queries[2], "repository(") != 1 ||
		len(got) != 3 || !slices.Equal(got["o/r#200"], []string{"o/r#1"}) {
		t.Errorf("%d queries, closes %v", len(queries), got)
	}
}

const (
	personalOwner  = "personal"
	addIssue7      = "add I7 to P"
	issue7OffBoard = `{"data":{"repository":{"issue":{"id":"I7","projectItems":{"nodes":[]}}}}}`
)

func TestIntakeAddsMissingIssueUnlessWorkedOnAnotherBoard(t *testing.T) {
	ctx := context.Background()
	c := &Client{GH: nil, Board: config.Board{Owner: personalOwner, Project: 1, Team: "X"}}

	// 1. A foreign issue not on any board (or only in Backlog on another board)
	// lands on the configured board in Backlog.
	f := &fakeGH{t: t,
		item:  `{"data":{"repository":{"issue":{"id":"I7","projectItems":{"nodes":[` + homeItem("U1", "Q", "other", 2, "Backlog") + `]}}}}}`,
		added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I3","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`,
	}
	c.GH = f.run
	mv, err := c.Intake(ctx, "org/engine#7", "backlog")
	if err != nil || *mv != (Moved{Ref: "org/engine#7", To: backlog, Added: true, Team: teamX}) ||
		!slices.Equal(f.mutations, []string{addIssue7, "I3=t1", "I3=s1"}) {
		t.Fatalf("Intake of unworked foreign issue = %+v, %v; mutations %v", mv, err, f.mutations)
	}

	// 2. An issue already being worked on another board (In Progress) stays
	// there and is not added to the configured board.
	f = &fakeGH{t: t,
		item: `{"data":{"repository":{"issue":{"id":"I7","projectItems":{"nodes":[` + homeItem("U1", "Q", "other", 2, "In Progress") + `]}}}}}`,
	}
	c.GH = f.run
	mv, err = c.Intake(ctx, "org/engine#7", "backlog")
	if err != nil || *mv != (Moved{Ref: "org/engine#7", From: "In Progress", To: "In Progress", Board: "other/2", Kept: true}) ||
		len(f.mutations) != 0 {
		t.Fatalf("Intake of issue worked on another board = %+v, %v; mutations %v", mv, err, f.mutations)
	}

	// 3. An issue already on the configured board in In Progress keeps its
	// Status instead of being moved back to Backlog.
	f = &fakeGH{t: t,
		item: `{"data":{"repository":{"issue":{"id":"I7","projectItems":{"nodes":[{"id":"I3","project":{"id":"P"},"status":{"name":"In Progress ⛏️"},"team":{"name":"X 🐝"}}]}}}}}` ,
	}
	c.GH = f.run
	mv, err = c.Intake(ctx, "org/engine#7", "backlog")
	if err != nil || *mv != (Moved{Ref: "org/engine#7", From: inProgress, To: inProgress, Kept: true}) ||
		len(f.mutations) != 0 {
		t.Fatalf("Intake of issue already in progress on configured board = %+v, %v; mutations %v", mv, err, f.mutations)
	}
}

func TestRankPlacesAgainstCrossOwnerItemWhoseProjectItemsOmitTheBoard(t *testing.T) {
	// GitHub omits another owner's project from issue.projectItems, so
	// placing an item after a cross-owner anchor on a personal board probes
	// addProjectV2ItemById to resolve the anchor's existing item ID.
	f := &fakeGH{t: t,
		byIssue: map[string]string{
			"7": issue7OffBoard,
			"9": `{"data":{"repository":{"issue":{"id":"ISSUE9","projectItems":{"nodes":[]}}}}}`,
		},
		added: `{"data":{"addProjectV2ItemById":{"item":{"id":"I9","project":{"id":"P"},"status":{"name":"In Progress ⛏️"},"kind":null,"team":null}}}}`,
	}
	origRun := f.run
	c := &Client{Board: config.Board{Owner: personalOwner, Project: 1, Team: "X"}}
	c.GH = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(args[3], "addProjectV2ItemById") && slices.Contains(args, "c=I7") {
			f.mutations = append(f.mutations, addIssue7)
			return []byte(`{"data":{"addProjectV2ItemById":{"item":{"id":"I3","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`), nil
		}
		return origRun(ctx, args...)
	}
	rk, err := c.Rank(context.Background(), "org/engine#7", Place{After: "org/engine#9"})
	if err != nil || *rk != (Ranked{Ref: "org/engine#7", Above: "org/engine#9", Added: true, Team: teamX}) ||
		!slices.Equal(f.mutations, []string{"add ISSUE9 to P", addIssue7, "I3=t1", `I3 after "I9"`}) {
		t.Fatalf("Rank after cross-owner item = %+v, %v; mutations %v", rk, err, f.mutations)
	}
}
