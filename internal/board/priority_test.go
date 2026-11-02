package board

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// The refs the priority tests share.
const (
	ref1 = "o/r#1"
	ref2 = "o/r#2"
	ref3 = "o/r#3"
	// cycleA and cycleB are each other's parent.
	cycleA = "o/r#17"
	cycleB = "o/r#18"
	ref4   = "o/r#4"
)

func TestRanksOfInheritsTheNearestRankedAncestor(t *testing.T) {
	r := Ranks{ref1: 2, issue9: 1}
	parents := map[string]string{ref3: ref2, ref2: ref1, ref1: issue9, cycleA: cycleB, cycleB: cycleA}
	parent := func(ref string) string { return parents[ref] }
	for _, tc := range []struct {
		ref  string
		rank int
		of   string
	}{
		{ref3, 2, ref1},       // the nearest ranked ancestor wins over a higher one above it
		{"O/R#1", 2, "O/R#1"}, // its own rank, case ignored
		{issue5, 0, ""},       // no parent
		{cycleA, 0, ""},       // a cycle ends
	} {
		if n, of := r.Of(tc.ref, parent); n != tc.rank || of != tc.of {
			t.Errorf("Of(%s) = %d, %q; want %d, %q", tc.ref, n, of, tc.rank, tc.of)
		}
	}
}

func TestStatusThenBoardOrderDecideAndPriorityOnlyAnnotates(t *testing.T) {
	// Every step has workScope, the setting that used to gate on the
	// priority list: it is read and ignored.
	o, err := ResolveOrder([]config.BoardStep{
		{Name: inProgress, Status: []string{inProgress}, WorkScope: true},
		{Name: upNext, Status: []string{upNext}, WorkScope: true},
		{Name: backlogStep, Status: []string{backlog}, WorkScope: true},
	}, statuses, []string{epic})
	if err != nil {
		t.Fatal(err)
	}
	it := func(n int, status string) Item {
		return Item{Ref: fmt.Sprintf("o/r#%d", n), OnBoard: true, Status: status, Created: days(5), Updated: days(1), Parent: "o/r#99"}
	}
	// The board's items in its position order, the statuses mixed.
	snap := &Snapshot{Order: o, Statuses: statuses, Items: []Item{
		it(30, backlog), it(20, upNext), it(11, inProgress), it(31, backlog), it(21, upNext), it(10, inProgress),
	}}
	want := []string{"o/r#11", "o/r#10", "o/r#20", "o/r#21", "o/r#30", "o/r#31"}
	for _, ranks := range []Ranks{nil, {"o/r#31": 1, "o/r#99": 2}} {
		cands := Rank(snap, config.Board{People: []string{"me"}}, now)
		Annotate(cands, ranks, func(string) string { return "" })
		var got []string
		for _, c := range cands {
			if c.Skip != "" {
				t.Errorf("ranks %v: %s skipped: %s", ranks, c.Ref, c.Skip)
			}
			got = append(got, c.Ref)
		}
		if !slices.Equal(got, want) {
			t.Errorf("ranks %v: order %v, want %v (Status, then the board's order)", ranks, got, want)
		}
		if len(ranks) > 0 {
			if p := cands[5].PriorityText(); p != "priority 1" {
				t.Errorf("o/r#31 annotated %q, want priority 1", p)
			}
			if p := cands[0].PriorityText(); p != "priority 2 of epic o/r#99" {
				t.Errorf("o/r#11 annotated %q, want its epic's rank", p)
			}
		}
	}
}

func TestSnapshotParentReadsItemsAndSubIssues(t *testing.T) {
	snap := &Snapshot{Items: []Item{
		{Ref: ref1, Parent: issue9, SubIssues: []Item{{Ref: ref2}, {Ref: ref3, Parent: ref1}}},
	}}
	for ref, want := range map[string]string{ref1: issue9, "O/R#2": ref1, ref3: ref1, ref4: ""} {
		if got := snap.Parent(ref); got != want {
			t.Errorf("Parent(%s) = %q, want %q", ref, got, want)
		}
	}
}

// priorityGH is a board with a Priority field: the ranked epics it holds
// (their items Ii for issue i), the issue a set reads and its item.
type priorityGH struct {
	t      *testing.T
	ranked map[int]int // issue → rank
	// labelled are the ranked epics the team label finds, issue → rank:
	// those with another Team are missing from the Team's query.
	labelled map[int]int
	kind     string // the Kind of the issue set reads
	offBoard bool   // the issue's projectItems name no item of the board
	// added is the Kind of the item addProjectV2ItemById answers, "" a new
	// item without fields; refused, GitHub refuses the add.
	added   string
	refused bool
	// hidden are the ranked epics the first read of the list leaves out, as
	// the board's search can: every later read names them.
	hidden    map[int]bool
	listReads int
	mutations []string
	queries   []string
}

const priorityMetaJSON = `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[
{"id":"FS","name":"Status","options":[{"id":"s1","name":"Backlog 📦"},{"id":"s2","name":"Up Next ➡️"},{"id":"s3","name":"In Progress ⛏️"}]},
{"id":"FK","name":"Kind","options":[{"id":"k1","name":"Epic 🎯"},{"id":"k2","name":"Task 🗒️"}]},
{"id":"FT","name":"Team","options":[{"id":"t1","name":"X 🐝"}]},
{"id":"FP","name":"Priority","dataType":"NUMBER"}]}}}}}`

func (f *priorityGH) run(_ context.Context, args ...string) ([]byte, error) {
	checkSchema(f.t, args)
	q := args[3]
	f.queries = append(f.queries, q)
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	switch {
	case strings.Contains(q, "clearProjectV2ItemFieldValue"):
		f.mutations = append(f.mutations, vars["i"]+" cleared")
		f.write(vars["i"], 0)
		return []byte(`{"data":{"clearProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
	case strings.Contains(q, "updateProjectV2ItemFieldValue"):
		f.mutations = append(f.mutations, vars["i"]+"="+vars["v"])
		v := 0
		_, _ = fmt.Sscan(vars["v"], &v)
		f.write(vars["i"], v)
		return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
	case strings.Contains(q, "fields(first:50)"):
		return []byte(priorityMetaJSON), nil
	case strings.Contains(q, "items(first:100"):
		ranked, team := f.ranked, teamX
		switch vars["q"] {
		case `is:open team:"X" kind:"Epic 🎯"`:
		case `is:open label:"team/x" kind:"Epic 🎯"`:
			ranked, team = f.labelled, "Y"
		default:
			f.t.Errorf("priority query %q", vars["q"])
		}
		f.listReads++
		var nodes []string
		for _, n := range slices.Sorted(maps.Keys(ranked)) {
			if f.hidden[n] && f.listReads <= 2 {
				continue
			}
			nodes = append(nodes, fmt.Sprintf(`{"id":"I%d","status":{"name":"In Progress ⛏️"},"kind":{"name":"Epic 🎯"},"priority":{"number":%d,"updatedAt":"2026-10-12T11:31:09Z","creator":{"login":"teemow"}},"team":{"name":%q,"updatedAt":"2026-10-16T06:53:56Z","creator":{"login":"giantswarm-project-automation"}},"content":%s}`, n, ranked[n], team, issue(n, "")))
		}
		// An epic without a rank is no entry.
		nodes = append(nodes, `{"id":"I99","status":{"name":"Backlog 📦"},"kind":{"name":"Epic 🎯"},"priority":null,"content":`+issue(99, "")+`}`)
		return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[` + strings.Join(nodes, ",") + `]}}}}}`), nil
	case strings.Contains(q, "addProjectV2ItemById"):
		f.mutations = append(f.mutations, "add "+vars["c"])
		if f.refused {
			return []byte(`{"data":{"addProjectV2ItemById":null},"errors":[{"message":"Resource not accessible by integration"}]}`), nil
		}
		if f.added == "" {
			return []byte(`{"data":{"addProjectV2ItemById":{"item":{"id":"I7","project":{"id":"P"},"status":null,"kind":null,"team":null}}}}`), nil
		}
		return []byte(fmt.Sprintf(`{"data":{"addProjectV2ItemById":{"item":{"id":"I7","project":{"id":"P"},"status":{"name":"Backlog 📦"},"kind":{"name":%q},"team":null}}}}`, f.added)), nil
	case strings.Contains(q, "projectItems") && f.offBoard:
		return []byte(`{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[]}}}}}`), nil
	case strings.Contains(q, "projectItems"):
		return []byte(fmt.Sprintf(`{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[{"id":"I%s","project":{"id":"P"},"status":{"name":"Backlog 📦"},"kind":{"name":%q},"team":{"name":"X 🐝"}}]}}}}}`, vars["n"], f.kind)), nil
	case strings.Contains(q, "issue(number:$n){...I}"):
		n := 0
		_, _ = fmt.Sscan(vars["n"], &n)
		return []byte(`{"data":{"repository":{"issue":` + issue(n, "") + `}}}`), nil
	}
	f.t.Fatalf("unexpected request %v", args)
	return nil, nil
}

// write sets the rank of the item I<n>, 0 its clear, where the reads find it.
func (f *priorityGH) write(item string, rank int) {
	n := 0
	_, _ = fmt.Sscanf(item, "I%d", &n)
	m := f.ranked
	if _, ok := f.labelled[n]; ok {
		m = f.labelled
	}
	if rank == 0 {
		delete(m, n)
		return
	}
	m[n] = rank
}

func priorityClient(f *priorityGH) *Client {
	return &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
}

func listText(l Priorities) string {
	parts := make([]string, len(l))
	for i, e := range l {
		parts[i] = fmt.Sprintf("%d %s", e.Rank, e.Ref)
	}
	return strings.Join(parts, ", ")
}

func TestPrioritiesReadsTheRankedEpicsByRank(t *testing.T) {
	f := &priorityGH{t: t, ranked: map[int]int{1: 2, 2: 1, 3: 3}}
	l, err := priorityClient(f).Priorities(context.Background())
	if err != nil || listText(l) != "1 o/r#2, 2 o/r#1, 3 o/r#3" || l[0].URL != "https://github.com/o/r/issues/2" || l[0].Status != inProgress {
		t.Fatalf("Priorities = %+v, %v", l, err)
	}
	if r := l.Ranks(); r[ref1] != 2 || r["o/r#99"] != 0 {
		t.Errorf("Ranks = %v", r)
	}
}

func TestPrioritiesOfABoardWithoutTheField(t *testing.T) {
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		if !strings.Contains(args[3], "fields(first:50)") {
			t.Fatalf("unexpected request %v", args)
		}
		return []byte(metaJSON), nil
	}
	l, err := (&Client{GH: gh, Board: config.Board{Owner: "o", Project: 7}}).Priorities(context.Background())
	if err != nil || len(l) != 0 {
		t.Errorf("Priorities = %v, %v; want an empty list", l, err)
	}
}

func TestSetPriorityInsertsAndRenumbers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ref       string
		rank      int
		from, to  int
		list      string
		mutations []string
	}{
		{"a new epic at the top moves the others down", issue5, 1, 0, 1, "1 o/r#5, 2 o/r#1, 3 o/r#2, 4 o/r#3", []string{"I5=1", rankOneSecond, "I2=3", "I3=4"}},
		{"a new epic past the end is the end", issue5, 9, 0, 4, "1 o/r#1, 2 o/r#2, 3 o/r#3, 4 o/r#5", []string{"I5=4"}},
		{"a ranked epic moves up", ref3, 1, 3, 1, "1 o/r#3, 2 o/r#1, 3 o/r#2", []string{"I3=1", rankOneSecond, "I2=3"}},
		{"its own rank writes nothing", ref2, 2, 2, 2, listOf123, nil},
		{"unset clears it and moves the ones below up", ref1, 0, 1, 0, "1 o/r#2, 2 o/r#3", []string{"I1 cleared", "I2=1", "I3=2"}},
		{"unset of an unranked epic writes nothing", issue5, 0, 0, 0, listOf123, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &priorityGH{t: t, ranked: map[int]int{1: 1, 2: 2, 3: 3}, kind: epic}
			p, err := priorityClient(f).SetPriority(context.Background(), tc.ref, tc.rank)
			if err != nil {
				t.Fatal(err)
			}
			if p.From != tc.from || p.To != tc.to || listText(p.List) != tc.list || !slices.Equal(f.mutations, tc.mutations) {
				t.Errorf("SetPriority = %d→%d [%s], mutations %v; want %d→%d [%s], %v", p.From, p.To, listText(p.List), f.mutations, tc.from, tc.to, tc.list, tc.mutations)
			}
			// Only the items named are read, besides the ranked epics.
			for _, q := range f.queries {
				if strings.Contains(q, "items(first:100") && !strings.Contains(q, "priority:fieldValueByName") {
					t.Errorf("a board read beside the priority list: %s", q)
				}
			}
		})
	}
}

func TestSetPriorityRefusesWhatIsNoEpic(t *testing.T) {
	f := &priorityGH{t: t, ranked: map[int]int{1: 1}, kind: "Task 🗒️"}
	_, err := priorityClient(f).SetPriority(context.Background(), issue5, 1)
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "no epic") || len(f.mutations) != 0 {
		t.Errorf("SetPriority of a task = %v, mutations %v", err, f.mutations)
	}
}

func TestSetPriorityAddsAnEpicOutsideTheBoardOwner(t *testing.T) {
	// An epic of a repository outside the board's owner is added as board
	// move adds one: the item the add answers, one put on the board by hand
	// that its projectItems do not list, carries its Kind and is ranked.
	f := &priorityGH{t: t, ranked: map[int]int{1: 1}, kind: epic, offBoard: true, added: epic}
	p, err := priorityClient(f).SetPriority(context.Background(), labIssue7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.To != 1 || p.Added || p.Team != teamX || listText(p.List) != "1 teemow/lab#7, 2 o/r#1" ||
		!slices.Equal(f.mutations, []string{addPriorityIssue, "I7=t1", "I7=1", rankOneSecond}) {
		t.Errorf("SetPriority of an epic outside the owner = %+v [%s], mutations %v", p, listText(p.List), f.mutations)
	}
	// A new item has no Kind yet: refused as no epic, the add GitHub's own.
	f = &priorityGH{t: t, ranked: map[int]int{1: 1}, kind: epic, offBoard: true}
	_, err = priorityClient(f).SetPriority(context.Background(), labIssue7, 1)
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "no epic") || !slices.Equal(f.mutations, []string{addPriorityIssue}) {
		t.Errorf("SetPriority of a new item = %v, mutations %v", err, f.mutations)
	}
	// GitHub refusing the add is the error, nothing ranked.
	f = &priorityGH{t: t, ranked: map[int]int{1: 1}, kind: epic, offBoard: true, refused: true}
	if _, err := priorityClient(f).SetPriority(context.Background(), labIssue7, 1); err == nil || !strings.Contains(err.Error(), "Resource not accessible") || !slices.Equal(f.mutations, []string{addPriorityIssue}) {
		t.Errorf("SetPriority whose add GitHub refuses = %v, mutations %v", err, f.mutations)
	}
}

func TestSetPriorityShiftsOnlyTheEpicsAtAndBelowItsRank(t *testing.T) {
	// The board's first read of the list leaves out the epics at 1, 4 and 9;
	// renumbering the rest gave duplicates and moved epics up.
	before := []int{38343, 36700, 38388, 37365, 38449, 38450, 38451, 38452, 37946, 37947, 938, 38157, 38118, 38453}
	f := &priorityGH{t: t, ranked: map[int]int{}, kind: epic, hidden: map[int]bool{38343: true, 37365: true, 37946: true}}
	for i, n := range before {
		f.ranked[n] = i + 1
	}
	p, err := priorityClient(f).SetPriority(context.Background(), "o/r#38688", 8)
	if err != nil {
		t.Fatal(err)
	}
	after := slices.Insert(slices.Clone(before), 7, 38688)
	want := make([]string, len(after))
	for i, n := range after {
		want[i] = fmt.Sprintf("%d o/r#%d", i+1, n)
		if f.ranked[n] != i+1 {
			t.Errorf("the board ranks o/r#%d %d; want %d", n, f.ranked[n], i+1)
		}
	}
	if p.From != 0 || p.To != 8 || listText(p.List) != strings.Join(want, ", ") {
		t.Errorf("SetPriority = %d→%d [%s]; want 0→8 [%s]", p.From, p.To, listText(p.List), strings.Join(want, ", "))
	}
	for _, m := range f.mutations {
		var n, r int
		_, _ = fmt.Sscanf(m, "I%d=%d", &n, &r)
		if i := slices.Index(before, n); i >= 0 && i < 7 {
			t.Errorf("an epic above rank 8 was written: %s", m)
		}
	}
}

func TestSetPriorityInTurnRepairsDuplicateRanks(t *testing.T) {
	// The board after the partial read: two epics at 1, at 4 and at 9.
	f := &priorityGH{t: t, kind: epic, ranked: map[int]int{
		36700: 1, 38343: 1, 38388: 2, 38449: 3, 37365: 4, 38450: 4, 38451: 5, 38452: 6,
		37947: 7, 38688: 8, 37946: 9, 938: 9, 38157: 10, 38118: 11, 38453: 12,
	}}
	order := []int{38343, 36700, 38388, 37365, 38449, 38450, 38451, 38688, 38452, 37946, 37947, 938, 38157, 38118, 38453}
	for i, n := range order {
		if _, err := priorityClient(f).SetPriority(context.Background(), fmt.Sprintf("o/r#%d", n), i+1); err != nil {
			t.Fatal(err)
		}
	}
	for i, n := range order {
		if f.ranked[n] != i+1 {
			t.Errorf("the board ranks o/r#%d %d; want %d", n, f.ranked[n], i+1)
		}
	}
}

const (
	rankOneSecond    = "I1=2"
	addPriorityIssue = "add ISSUE"
)

func TestAnnotateInheritsRankFromBlockedCandidate(t *testing.T) {
	const task1 = "personal/tasks#1"
	cands := []Candidate{
		{Item: Item{Ref: task1, OpenBlockedBy: []string{"org/engine#99"}}},
		{Item: Item{Ref: "org/engine#99"}},
	}
	Annotate(cands, Ranks{task1: 1}, func(string) string { return "" })
	if cands[1].Priority != 1 || cands[1].PriorityOf != task1 {
		t.Fatalf("blocker candidate priority = %d of %q, want 1 of %s", cands[1].Priority, cands[1].PriorityOf, task1)
	}
}

func TestRankAnchorPlacesBlockingOrFollowUpIssueAfterHighRankedItem(t *testing.T) {
	ctx := context.Background()
	c := &Client{Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	c.GH = func(_ context.Context, args ...string) ([]byte, error) {
		q := args[3]
		switch {
		case strings.Contains(q, "fields(first:50)"):
			return []byte(priorityMetaJSON), nil
		case strings.Contains(q, "items(first:100"):
			return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[
				{"id":"I1","priority":{"number":1},"status":{"name":"In Progress ⛏️"},"team":{"name":"X 🐝"},"content":{"number":1,"url":"https://github.com/o/r/issues/1","title":"epic 1","state":"OPEN","repository":{"nameWithOwner":"o/r"},"blockedBy":{"nodes":[{"number":50,"state":"OPEN","repository":{"nameWithOwner":"org/engine"}}]}}},{"id":"I2","priority":{"number":2},"status":{"name":"Up Next ➡️"},"team":{"name":"X 🐝"},"content":{"number":2,"url":"https://github.com/o/r/issues/2","title":"epic 2","state":"OPEN","repository":{"nameWithOwner":"o/r"}}}]}}}}}`), nil
		case strings.Contains(q, "projectItems"):
			if slices.Contains(args, "n=12") {
				return []byte(`{"data":{"repository":{"issue":{"id":"SUB12","parent":{"number":1,"repository":{"nameWithOwner":"o/r"}},"projectItems":{"nodes":[{"id":"IS12","project":{"id":"P"},"status":{"name":"In Progress ⛏️"}}]}}}}} `), nil
			}
			return []byte(`{"data":{"repository":{"issue":{"id":"NEW","body":"","projectItems":{"nodes":[]}}}}}`), nil
		}
		return nil, fmt.Errorf("unexpected query: %s", q)
	}

	// 1. An issue that blocks epic 1 (priority 1) anchors right after epic 1.
	if got, err := c.RankAnchor(ctx, "org/engine#50", ""); err != nil || got != ref1 {
		t.Fatalf("RankAnchor(blocker of epic 1) = %q, %v; want %s", got, err, ref1)
	}

	// 2. An issue filed from o/r#12 (a sub-issue on the board under epic 1)
	// anchors right after o/r#12.
	if got, err := c.RankAnchor(ctx, "org/engine#51", "o/r#12"); err != nil || got != "o/r#12" {
		t.Fatalf("RankAnchor(filed from o/r#12) = %q, %v; want o/r#12", got, err)
	}

	// 3. A general issue not tied to a higher-ranked item anchors after the
	// last ranked epic (o/r#2) instead of at the bottom of the board.
	if got, err := c.RankAnchor(ctx, "org/engine#52", ""); err != nil || got != ref2 {
		t.Fatalf("RankAnchor(general issue) = %q, %v; want %s", got, err, ref2)
	}
}
