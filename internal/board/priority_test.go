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

func TestRanksOfInheritsTheNearestRankedAncestor(t *testing.T) {
	r := Ranks{"o/r#1": 2, "o/r#9": 1}
	parents := map[string]string{"o/r#3": "o/r#2", "o/r#2": "o/r#1", "o/r#1": "o/r#9", "o/r#7": "o/r#8", "o/r#8": "o/r#7"}
	parent := func(ref string) string { return parents[ref] }
	for _, tc := range []struct {
		ref  string
		rank int
		of   string
	}{
		{"o/r#3", 2, "o/r#1"}, // the nearest ranked ancestor wins over a higher one above it
		{"O/R#1", 2, "O/R#1"}, // its own rank, case ignored
		{"o/r#5", 0, ""},      // no parent
		{"o/r#7", 0, ""},      // a cycle ends
	} {
		if n, of := r.Of(tc.ref, parent); n != tc.rank || of != tc.of {
			t.Errorf("Of(%s) = %d, %q; want %d, %q", tc.ref, n, of, tc.rank, tc.of)
		}
	}
}

func TestByPriorityRanksFirstThenTheOrder(t *testing.T) {
	cand := func(ref, step, epic, parent string) Candidate {
		return Candidate{Item: Item{Ref: ref, Parent: parent}, Step: step, Epic: epic}
	}
	cands := []Candidate{
		cand("o/r#10", "In Progress remainder", "o/r#1", "o/r#1"), // epic 1, unranked
		cand("o/r#20", "In Progress remainder", "o/r#2", "o/r#2"), // epic 2, rank 2
		cand("o/r#30", "Up Next", "", "o/r#33"),                   // grandchild of epic 3, rank 1
		cand("o/r#40", "Backlog", "", ""),                         // unranked
		cand("o/r#3", "Up Next", "", ""),                          // epic 3 itself
		cand("o/r#21", "Backlog", "", "o/r#2"),                    // epic 2, behind #20 in the order
	}
	parent := func(ref string) string {
		return map[string]string{"o/r#33": "o/r#3"}[ref]
	}
	ByPriority(cands, Ranks{"o/r#3": 1, "o/r#2": 2}, parent)
	var got []string
	for _, c := range cands {
		got = append(got, strings.TrimSpace(c.Ref+" "+c.PriorityText()))
	}
	want := []string{
		"o/r#30 priority 1 of epic o/r#3",
		"o/r#3 priority 1",
		"o/r#20 priority 2 of epic o/r#2",
		"o/r#21 priority 2 of epic o/r#2",
		"o/r#10",
		"o/r#40",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ordered:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if w := cands[0].Why(); w != "Up Next, priority 1 of epic o/r#3" {
		t.Errorf("Why = %q", w)
	}
}

func TestSnapshotParentReadsItemsAndSubIssues(t *testing.T) {
	snap := &Snapshot{Items: []Item{
		{Ref: "o/r#1", Parent: "o/r#9", SubIssues: []Item{{Ref: "o/r#2"}, {Ref: "o/r#3", Parent: "o/r#1"}}},
	}}
	for ref, want := range map[string]string{"o/r#1": "o/r#9", "O/R#2": "o/r#1", "o/r#3": "o/r#1", "o/r#4": ""} {
		if got := snap.Parent(ref); got != want {
			t.Errorf("Parent(%s) = %q, want %q", ref, got, want)
		}
	}
}

// priorityGH is a board with a Priority field: the ranked epics it holds
// (their items Ii for issue i), the issue a set reads and its item.
type priorityGH struct {
	t         *testing.T
	ranked    map[int]int // issue → rank
	kind      string      // the Kind of the issue set reads
	mutations []string
	queries   []string
}

const priorityMetaJSON = `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[
{"id":"FS","name":"Status","options":[{"id":"s1","name":"Backlog 📦"},{"id":"s2","name":"Up Next ➡️"},{"id":"s3","name":"In Progress ⛏️"}]},
{"id":"FK","name":"Kind","options":[{"id":"k1","name":"Epic 🎯"},{"id":"k2","name":"Task 🗒️"}]},
{"id":"FT","name":"Team","options":[{"id":"t1","name":"X 🐝"}]},
{"id":"FP","name":"Priority","dataType":"NUMBER"}]}}}}}`

func (f *priorityGH) run(_ context.Context, args ...string) ([]byte, error) {
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
		return []byte(`{"data":{"clearProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
	case strings.Contains(q, "updateProjectV2ItemFieldValue"):
		f.mutations = append(f.mutations, vars["i"]+"="+vars["v"])
		return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
	case strings.Contains(q, "fields(first:50)"):
		return []byte(priorityMetaJSON), nil
	case strings.Contains(q, "items(first:100"):
		if vars["q"] != `is:open team:"X" kind:"Epic 🎯"` {
			f.t.Errorf("priority query %q", vars["q"])
		}
		var nodes []string
		for _, n := range slices.Sorted(maps.Keys(f.ranked)) {
			nodes = append(nodes, fmt.Sprintf(`{"id":"I%d","status":{"name":"In Progress ⛏️"},"kind":{"name":"Epic 🎯"},"priority":{"number":%d},"content":%s}`, n, f.ranked[n], issue(n, "")))
		}
		// An epic without a rank is no entry.
		nodes = append(nodes, `{"id":"I99","status":{"name":"Backlog 📦"},"kind":{"name":"Epic 🎯"},"priority":null,"content":`+issue(99, "")+`}`)
		return []byte(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[` + strings.Join(nodes, ",") + `]}}}}}`), nil
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
	if r := l.Ranks(); r["o/r#1"] != 2 || r["o/r#99"] != 0 {
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
		{"a new epic at the top moves the others down", "o/r#5", 1, 0, 1, "1 o/r#5, 2 o/r#1, 3 o/r#2, 4 o/r#3", []string{"I5=1", "I1=2", "I2=3", "I3=4"}},
		{"a new epic past the end is the end", "o/r#5", 9, 0, 4, "1 o/r#1, 2 o/r#2, 3 o/r#3, 4 o/r#5", []string{"I5=4"}},
		{"a ranked epic moves up", "o/r#3", 1, 3, 1, "1 o/r#3, 2 o/r#1, 3 o/r#2", []string{"I3=1", "I1=2", "I2=3"}},
		{"its own rank writes nothing", "o/r#2", 2, 2, 2, "1 o/r#1, 2 o/r#2, 3 o/r#3", nil},
		{"unset clears it and moves the ones below up", "o/r#1", 0, 1, 0, "1 o/r#2, 2 o/r#3", []string{"I1 cleared", "I2=1", "I3=2"}},
		{"unset of an unranked epic writes nothing", "o/r#5", 0, 0, 0, "1 o/r#1, 2 o/r#2, 3 o/r#3", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &priorityGH{t: t, ranked: map[int]int{1: 1, 2: 2, 3: 3}, kind: "Epic 🎯"}
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
	_, err := priorityClient(f).SetPriority(context.Background(), "o/r#5", 1)
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "no epic") || len(f.mutations) != 0 {
		t.Errorf("SetPriority of a task = %v, mutations %v", err, f.mutations)
	}
}
