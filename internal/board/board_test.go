package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
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
)

var statuses = []string{"Inbox 📥", backlog, upNext, inProgress, blocked, "Validation ☑️", "Done ✅"}

func days(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }

// order is a desk's order, written the way a person types it.
func order() []config.BoardStep {
	return []config.BoardStep{
		{Name: "page", Kind: []string{"alert"}},
		{Name: "In Progress", Status: []string{"in progress"}, SubIssues: true},
		{Name: "blocker cleared", Status: []string{"blocked"}, Unblocked: true},
		{Name: upNextEpic, Status: []string{typedNext}, Kind: []string{"epic"}, SubIssues: true},
		{Name: "Up Next", Status: []string{typedNext}},
		{Name: bugStep, Kind: []string{bugStep}},
		{Name: "queue", Search: "repo:o/tools label:team/x"},
		{Name: "Backlog", Status: []string{"backlog"}, CreatedWithin: config.Duration{Duration: 90 * 24 * time.Hour}},
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
	o, err := ResolveOrder(order(), statuses, []string{epic, bug, "Alert 🚨"})
	if err != nil {
		t.Fatal(err)
	}
	return &Snapshot{Order: o, Items: fixture(), Statuses: statuses, Search: map[int][]Item{6: {{Ref: "o/tools#1", Created: days(5), Updated: days(1)}}}}
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
		"o/r#19 In Progress: 1 of 1 blockers open",
		"o/r#40 In Progress < o/r#4",
		"o/r#41 In Progress < o/r#4: 4 of 4 blockers open",
		"o/r#16 In Progress < o/r#15",
		"o/r#17 In Progress < o/r#15: created 2026-06-24: Backlog takes items created within 90 days",
		"o/r#18 In Progress < o/r#15: no step of board.order offers Inbox 📥",
		"o/r#20 In Progress < o/r#15: 3 of 4 blockers open",
		"o/r#21 In Progress < o/r#15: no step of board.order offers Inbox 📥",
		"o/r#22 In Progress < o/r#15: created 2026-06-24: Backlog takes items created within 90 days",
		"o/r#23 In Progress < o/r#15: 1 of 2 blockers open",
		"o/r#6 blocker cleared",
		"o/r#80 Up Next epic < o/r#8",
		"o/r#9 Up Next epic",
		"o/r#10 Up Next",
		"o/r#11 bug",
		"o/tools#1 queue",
		"o/r#13 Backlog",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rank:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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
		{Item{Status: blocked, Blockers: 3, OpenBlockers: 2}, "2 of 3 blockers open"},
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
	o, _ := ResolveOrder(order()[1:5], statuses, []string{epic})
	if got, want := filter(deskBoard(), o), `is:open team:"X" status:"In Progress ⛏️","Blocked / Waiting ⛔️","Up Next ➡️"`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
	// A step without statuses reads every status.
	if got, want := filter(config.Board{}, []config.BoardStep{{Name: bugStep, Kind: []string{bug}}}), "is:open"; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// fakeGH answers the board's GraphQL requests from a fixture and records the
// mutations.
type fakeGH struct {
	t         *testing.T
	pages     []string
	extras    string
	item      string
	mutations []string
}

const metaJSON = `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{},
{"id":"FS","name":"Status","options":[{"id":"s1","name":"Backlog 📦"},{"id":"s2","name":"Up Next ➡️"},{"id":"s3","name":"In Progress ⛏️"}]},
{"id":"FK","name":"Kind","options":[{"id":"k1","name":"Epic 🎯"}]}]}}}}}`

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	q := args[3]
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	switch {
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
	case strings.Contains(q, "subIssues(first:50)") || strings.Contains(q, "search("):
		if !strings.Contains(q, `e1:repository(owner:"o",name:"r"){issue(number:2){subIssues(first:50){nodes{...I projectItems(`) ||
			!strings.Contains(q, `s2:search(query:"repo:o/q is:issue is:open sort:created-asc"`) {
			f.t.Errorf("extras query %s", q)
		}
		return []byte(f.extras), nil
	case strings.Contains(q, "projectItems"):
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

func TestReadPagesTheBoardAndReadsSubIssuesAndSearches(t *testing.T) {
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":true,"endCursor":"C"},"nodes":[` +
			node(inProgress, "null", issue(1, `,"assignees":{"nodes":[{"login":"pat"}]}`)) + "," +
			node(upNext, `{"name":"Epic 🎯"}`, issue(2, `,"subIssuesSummary":{"total":3,"completed":1}`)) + "," +
			`{"status":{"name":"Up Next ➡️"},"content":{}}]}}}}}`,
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[` +
			node(upNext, "null", strings.Replace(issue(3, ""), "OPEN", "CLOSED", 1)) + "," + node(upNext, "null", issue(4, "")) + `]}}}}}`,
	}, extras: `{"data":{"e1":{"issue":{"subIssues":{"nodes":[` +
		// 20 is on another board only, 22 on this one in Inbox without a Team.
		issue(20, `,"projectItems":{"nodes":[{"project":{"id":"Q"},"status":{"name":"Inbox 📥"}}]}`) + "," +
		strings.Replace(issue(21, ""), "OPEN", "CLOSED", 1) + "," +
		issue(22, `,"projectItems":{"nodes":[{"project":{"id":"Q"}},{"project":{"id":"P"},"status":{"name":"Inbox 📥"},"kind":null}]}`) +
		`]}}},"s2":{"nodes":[` + issue(30, "") + `]}}}`}
	b := config.Board{Owner: "o", Project: 7, Team: "X", People: []string{"me"}, Order: []config.BoardStep{
		{Name: "In Progress", Status: []string{"progress"}},
		{Name: upNextEpic, Status: []string{typedNext}, Kind: []string{"epic"}, SubIssues: true},
		{Name: "queue", Search: "repo:o/q"},
		{Name: "Up Next", Status: []string{"next"}},
	}}
	snap, err := (&Client{GH: f.run, Board: b}).Read(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range Rank(snap, b, now) {
		got = append(got, c.Ref+" "+c.Why()+" "+c.Skip)
	}
	want := []string{
		"o/r#1 In Progress (In Progress ⛏️) assigned to pat",
		"o/r#20 Up Next epic, sub-issue of o/r#2 ",
		"o/r#22 Up Next epic, sub-issue of o/r#2 (Inbox 📥) no step of board.order offers Inbox 📥",
		"o/r#30 queue ",
		"o/r#4 Up Next (Up Next ➡️) ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("read and ranked:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMoveTakesOnlyTheBoardsStatuses(t *testing.T) {
	f := &fakeGH{t: t, item: `{"data":{"repository":{"issue":{"projectItems":{"nodes":[{"id":"other","project":{"id":"Q"}},{"id":"I1","project":{"id":"P"},"status":{"name":"Backlog 📦"}}]}}}}}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7}}
	mv, err := c.Move(context.Background(), "https://github.com/o/r/issues/5", "in progress")
	if err != nil || *mv != (Moved{Ref: "o/r#5", From: backlog, To: inProgress}) || !slices.Equal(f.mutations, []string{"I1=s3"}) {
		t.Fatalf("Move = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	_, err = c.Move(context.Background(), "o/r#5", "Doing")
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "none of the values: Backlog 📦, Up Next ➡️, In Progress ⛏️") || len(f.mutations) != 1 {
		t.Errorf("Move to an unknown status = %v; mutations %v", err, f.mutations)
	}
	f.item = `{"data":{"repository":{"issue":{"projectItems":{"nodes":[]}}}}}`
	if _, err := c.Move(context.Background(), "o/r#5", "backlog"); !errors.As(err, &r) || !strings.Contains(r.Reason, "not on the board o/7") {
		t.Errorf("Move of an item not on the board = %v", err)
	}
	if _, err := (&Client{GH: f.run}).Move(context.Background(), "o/r#5", "backlog"); err == nil || !strings.Contains(err.Error(), "no board configured") {
		t.Errorf("Move without a board = %v", err)
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
