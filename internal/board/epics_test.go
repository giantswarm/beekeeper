package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
)

const (
	epicRef   = "o/r#50"
	planURL   = "https://github.com/o/plans/pull/1"
	planHead  = "6a672cdede08fd2e7b00fb904d539d040419a24f"
	plansRepo = "o/plans"
)

// gatedBoard is an Up Next epic with a merged plan, a sub-issue off the
// board and one on it in Backlog, that one's own sub-issue in Up Next, and
// an Up Next item of no epic.
func gatedBoard(t *testing.T, o []config.BoardStep) *Snapshot {
	t.Helper()
	kinds := []string{epic, bug, "Alert 🚨"}
	order, err := ResolveOrder(o, statuses, kinds)
	if err != nil {
		t.Fatal(err)
	}
	g, err := ResolveGate(config.EpicGate{}, statuses, kinds)
	if err != nil {
		t.Fatal(err)
	}
	it := func(n int, status, kind, parent string) Item {
		return Item{Ref: fmt.Sprintf("o/r#%d", n), OnBoard: status != "", Status: status, Kind: kind, Parent: parent, Created: days(5), Updated: days(1)}
	}
	ep := it(50, upNext, epic, "")
	slice := it(52, backlog, "", epicRef)
	ep.OpenSubIssues, ep.SubIssues = 2, []Item{it(51, "", "", epicRef), slice}
	return &Snapshot{Order: order, Statuses: statuses, Gate: g,
		Items: []Item{ep, slice, it(53, upNext, "", "o/r#52"), it(10, upNext, "", "")},
		Plans: map[string]*Plan{epicRef: {Ref: "o/plans#1", URL: planURL, State: "merged", Head: planHead}}}
}

func ranked(snap *Snapshot) []string {
	var got []string
	for _, c := range Rank(snap, deskBoard(), now) {
		got = append(got, strings.TrimSuffix(c.Ref+" "+c.Why()+": "+c.Skip, ": "))
	}
	return got
}

func TestAnUpNextEpicIsInPlanningAndInProgressIsTheGo(t *testing.T) {
	// With the old Up Next epic step and without it: the gate holds whatever
	// the order says.
	old := slices.Insert(order(), 3, config.BoardStep{Name: upNextEpic, Status: []string{typedNext}, Kind: []string{defaultEpicKind}, SubIssues: true})
	for _, o := range [][]config.BoardStep{order(), old} {
		snap := gatedBoard(t, o)
		step := order()[3].Name
		if len(o) != len(order()) {
			step = upNextEpic
		}
		want := []string{
			"o/r#50 " + step + " (Up Next ➡️): epic in planning (Up Next ➡️), plan " + planURL + ` (merged): its sub-issues wait for the go, board move o/r#50 "In Progress ⛏️"`,
			"o/r#53 Up Next (Up Next ➡️): sub-issue of epic o/r#50, in planning (Up Next ➡️)",
			"o/r#10 Up Next (Up Next ➡️)",
			"o/r#52 Backlog (Backlog 📦): sub-issue of epic o/r#50, in planning (Up Next ➡️)",
		}
		if got := ranked(snap); !slices.Equal(got, want) {
			t.Errorf("in planning:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// The go: moved to In Progress, the epic offers its sub-issues in
		// their order, with no session acting.
		snap.Items[0].Status = inProgress
		want = []string{
			"o/r#51 In Progress, sub-issue of o/r#50",
			"o/r#52 In Progress, sub-issue of o/r#50 (Backlog 📦)",
			"o/r#53 Up Next (Up Next ➡️)",
			"o/r#10 Up Next (Up Next ➡️)",
		}
		if got := ranked(snap); !slices.Equal(got, want) {
			t.Errorf("after the go:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if es := snap.Epics(); len(es) != 1 || es[0].Ref != epicRef || es[0].Status != inProgress || es[0].PlanText() != "plan "+planURL+" (merged, head 6a672cd)" {
			t.Errorf("epics %+v", es)
		}
	}
	// An Up Next epic that is a sub-issue of an epic in progress, read
	// without its board fields beside it, is held by its board item.
	snap := gatedBoard(t, order())
	parent := Item{Ref: "o/r#60", OnBoard: true, Status: inProgress, Kind: epic, OpenSubIssues: 1, Created: days(5), Updated: days(1),
		SubIssues: []Item{{Ref: epicRef, Created: days(5), Updated: days(1)}}}
	snap.Items = append([]Item{parent}, snap.Items...)
	if got := ranked(snap)[0]; !strings.HasPrefix(got, "o/r#50 In Progress, sub-issue of o/r#60 (Up Next ➡️): epic in planning") {
		t.Errorf("a gated epic under an epic in progress: %s", got)
	}
	// An Up Next epic without a plan is its planning session's.
	snap = gatedBoard(t, order())
	snap.Plans = nil
	if got := ranked(snap)[0]; got != "o/r#50 Up Next (Up Next ➡️): epic in planning (Up Next ➡️), no plan pull request names it: its planning session's" {
		t.Errorf("without a plan: %s", got)
	}
}

func TestResolveGate(t *testing.T) {
	g, err := ResolveGate(config.EpicGate{}, statuses, []string{epic})
	if err != nil || !slices.Equal(g.Kind, []string{epic}) || !slices.Equal(g.Planning, []string{upNext}) || g.Go != inProgress {
		t.Errorf("default gate = %+v, %v", g, err)
	}
	// A board without the default values has no gate.
	if g, err := ResolveGate(config.EpicGate{}, statuses, []string{bug}); err != nil || len(g.Kind) != 0 {
		t.Errorf("gate on a board without epics = %+v, %v", g, err)
	}
	// A value board.epics names must be the board's.
	if _, err := ResolveGate(config.EpicGate{Kind: []string{"saga"}}, statuses, []string{epic}); err == nil || !strings.Contains(err.Error(), `board.epics: Kind "saga" is none of the values`) {
		t.Errorf("unknown kind = %v", err)
	}
	g, err = ResolveGate(config.EpicGate{Kind: []string{defaultEpicKind, "bug"}, Planning: []string{"backlog", "next"}, Go: "waiting"}, statuses, []string{epic, bug})
	if err != nil || !slices.Equal(g.Kind, []string{epic, bug}) || !slices.Equal(g.Planning, []string{backlog, upNext}) || g.Go != blocked {
		t.Errorf("configured gate = %+v, %v", g, err)
	}
}

// planNode is a pull request a plans search answers.
func planNode(n int, state string, draft, merged bool, body string) string {
	return fmt.Sprintf(`{"id":"PR%d","number":%d,"url":"https://github.com/o/plans/pull/%d","title":"Plan %d","state":%q,"isDraft":%t,"merged":%t,"headRefOid":"h%d","body":%q,"repository":{"nameWithOwner":"o/plans","isPrivate":true}}`,
		n, n, n, n, state, draft, merged, n, body)
}

func TestFindPlansTakesTheOpenPlanNamingTheEpic(t *testing.T) {
	var queries, repos []string
	c := &Client{Plans: []string{plansRepo, "o/more"}, GH: func(ctx context.Context, args ...string) ([]byte, error) {
		queries, repos = append(queries, args[3]), append(repos, github.Repo(ctx))
		return []byte(`{"data":{"p0":{"nodes":[` +
			planNode(5, "OPEN", false, false, "Epic: https://github.com/o/r/issues/9380") + "," + // another epic
			planNode(6, "OPEN", false, false, "Epic: https://github.com/o/r/issues/1\n\n1. https://github.com/o/r/issues/938: a slice") + "," + // a sub-issue
			planNode(4, "CLOSED", false, false, "Epic: https://github.com/o/r/issues/938") + "," + // closed unmerged
			planNode(3, "MERGED", false, true, "Epic: https://github.com/o/r/issues/938") + "," +
			planNode(2, "OPEN", true, false, "**Epic:** [o/r#938](https://github.com/o/r/issues/938), revision 2") + "," +
			planNode(1, "OPEN", false, false, "Epic: https://github.com/o/r/issues/938") +
			`]},"p1":{"nodes":[` + planNode(7, "MERGED", false, true, "Epic: https://github.com/o/r/issues/939") + `,{}]},"p2":{"nodes":[]}}}`), nil
	}}
	got, err := c.FindPlans(context.Background(), []string{"o/r#938", "O/R#939", "o/r#940"})
	if err != nil {
		t.Fatal(err)
	}
	if p := got["o/r#938"]; p == nil || p.URL != "https://github.com/o/plans/pull/2" || p.State != "draft" || p.Head != "h2" || !p.Private || p.Ref != "o/plans#2" {
		t.Errorf("plan of #938 = %+v", p)
	}
	if p := got["o/r#939"]; p == nil || p.State != "merged" || p.Ref != "o/plans#7" {
		t.Errorf("plan of #939 = %+v", p)
	}
	if p, ok := got["o/r#940"]; ok {
		t.Errorf("plan of #940 = %+v", p)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], `p0:search(query:"repo:o/plans repo:o/more is:pr \"o/r/issues/938\" sort:updated-desc",type:ISSUE`) ||
		!slices.Equal(repos, []string{plansRepo}) {
		t.Errorf("queries %q acting on %q", queries, repos)
	}
	if got, err := (&Client{}).FindPlans(context.Background(), []string{"x/y#2"}); got != nil || err != nil {
		t.Errorf("without plans repositories = %v, %v", got, err)
	}
}

// goGH answers a move of the Up Next epic o/r#50: the board, the epic's
// item, the plans search and the epic's repository.
type goGH struct {
	t                  *testing.T
	planPrivate, found bool
	repoPrivate        bool
	mutations, repos   []string
}

func (f *goGH) run(ctx context.Context, args ...string) ([]byte, error) {
	q := args[3]
	f.repos = append(f.repos, github.Repo(ctx))
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	switch {
	case strings.Contains(q, "addComment"):
		f.mutations = append(f.mutations, "comment on "+vars["s"]+": "+vars["b"])
		return []byte(`{"data":{"addComment":{"commentEdge":{"node":{"url":"https://github.com/o/plans/pull/1#issuecomment-1"}}}}}`), nil
	case strings.HasPrefix(q, "query=mutation"):
		f.mutations = append(f.mutations, vars["i"]+"="+vars["v"])
		return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"I"}}}}`), nil
	case strings.Contains(q, "fields(first:50)"):
		return []byte(metaJSON), nil
	case strings.Contains(q, "search("):
		if !f.found {
			return []byte(`{"data":{"p0":{"nodes":[]}}}`), nil
		}
		n := strings.Replace(planNode(1, "OPEN", true, false, "Epic: https://github.com/o/r/issues/50"), `"h1"`, `"`+planHead+`"`, 1)
		if !f.planPrivate {
			n = strings.Replace(n, `"isPrivate":true`, `"isPrivate":false`, 1)
		}
		return []byte(`{"data":{"p0":{"nodes":[` + n + `]}}}`), nil
	case strings.Contains(q, "isPrivate issue("):
		return fmt.Appendf(nil, `{"data":{"repository":{"isPrivate":%t,"issue":{"id":"EPIC"}}}}`, f.repoPrivate), nil
	case strings.Contains(q, "projectItems"):
		return []byte(`{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[{"id":"I1","project":{"id":"P"},"status":{"name":"Up Next ➡️"},"kind":{"name":"Epic 🎯"},"team":{"name":"X 🐝"}}]}}}}}`), nil
	}
	f.t.Fatalf("unexpected request %v", args)
	return nil, nil
}

func TestMovingAnEpicToInProgressIsTheGo(t *testing.T) {
	b := config.Board{Owner: "o", Project: 7, Team: "X"}
	// A public epic with a private plan: the go is recorded on the plan.
	f := &goGH{t: t, found: true, planPrivate: true}
	c := &Client{GH: f.run, Board: b, Plans: []string{plansRepo}}
	mv, err := c.Move(context.Background(), epicRef, "in progress")
	if err != nil || mv.Go == nil || mv.Go.Thread != "o/plans#1" || mv.Go.Plan.Head != planHead || mv.Go.Plan.State != "draft" || !slices.Equal(f.mutations, []string{"I1=s3"}) {
		t.Fatalf("Move = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	url, err := c.RecordGo(context.Background(), mv.Go, mv.Go.Text("guide", mv.To))
	if err != nil || url != "https://github.com/o/plans/pull/1#issuecomment-1" || f.repos[len(f.repos)-1] != plansRepo {
		t.Fatalf("RecordGo = %q, %v; repos %v", url, err, f.repos)
	}
	if got := f.mutations[1]; !strings.HasPrefix(got, "comment on PR1: **Go:** o/r#50 moved to In Progress ⛏️ by guide. Its open sub-issues are offered as work from now on, implemented at this plan at its head "+planHead+" (draft).") ||
		!strings.Contains(got, "Written by an agent") || strings.Contains(got, "@") {
		t.Errorf("comment %q", got)
	}
	// A private epic records the go on itself, naming the plan.
	f = &goGH{t: t, found: true, planPrivate: true, repoPrivate: true}
	c.GH = f.run
	if mv, err := c.Move(context.Background(), epicRef, "in progress"); err != nil || mv.Go.Thread != epicRef || !strings.Contains(mv.Go.Text("guide", mv.To), "implemented at plan "+planURL+" at its head") {
		t.Errorf("Move of a private epic = %+v, %v", mv, err)
	}
	// A public epic with a public plan has no private thread: refused, nothing written.
	f = &goGH{t: t, found: true}
	c.GH = f.run
	var r *Refusal
	if _, err := c.Move(context.Background(), epicRef, "in progress"); !errors.As(err, &r) || !strings.Contains(r.Reason, "has no private thread") || len(f.mutations) != 0 {
		t.Errorf("Move with no private thread = %v; mutations %v", err, f.mutations)
	}
	// Without a plan the go moves and records nothing.
	f = &goGH{t: t}
	c.GH = f.run
	if mv, err := c.Move(context.Background(), epicRef, "in progress"); err != nil || mv.Go == nil || mv.Go.Plan != nil || len(f.mutations) != 1 {
		t.Errorf("Move without a plan = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	// Any other move of the epic is no go.
	f = &goGH{t: t, found: true}
	c.GH = f.run
	if mv, err := c.Move(context.Background(), epicRef, "backlog"); err != nil || mv.Go != nil {
		t.Errorf("Move to Backlog = %+v, %v", mv, err)
	}
}
