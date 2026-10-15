package board

import (
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

// limits is GitHub's message for a field over its resource limits.
const limits = "Resource limits for this query exceeded."

func TestGraphQLReturnsAPartialAnswerWithItsData(t *testing.T) {
	answer := ""
	c := &Client{GH: func(context.Context, ...string) ([]byte, error) { return []byte(answer), nil }}
	var r struct {
		Repository struct {
			Issue *struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"repository"`
	}

	answer = `{"data":{"repository":{"issue":{"id":"X","projectItems":null}}},"errors":[{"type":"RESOURCE_LIMITS_EXCEEDED","path":["repository","issue","projectItems","nodes",0,"project"],"message":"` + limits + `"}]}`
	err := c.graphql(t.Context(), &r, "query", nil)
	var p *Partial
	if !errors.As(err, &p) || r.Repository.Issue == nil || r.Repository.Issue.ID != "X" {
		t.Fatalf("graphql = %v, data %+v", err, r)
	}
	if want := (GraphQLError{Path: []string{"repository", "issue", "projectItems", "nodes", "0", "project"}, Type: "RESOURCE_LIMITS_EXCEEDED", Message: limits}); len(p.Errors) != 1 || !slices.Equal(p.Errors[0].Path, want.Path) || p.Errors[0].Type != want.Type || p.Errors[0].Message != want.Message {
		t.Errorf("partial %+v", p.Errors)
	}
	if want := "GitHub answered partially: repository.issue.projectItems.nodes.0.project: " + limits; err.Error() != want {
		t.Errorf("message %q", err.Error())
	}

	answer = `{"data":{"repository":{"issue":{"id":"Y"}}}}`
	if err := c.graphql(t.Context(), &r, "query", nil); err != nil || r.Repository.Issue.ID != "Y" {
		t.Errorf("clean answer: %v, %+v", err, r)
	}

	// No data is an error of its own, never a Partial.
	answer = `{"data":null,"errors":[{"message":"Could not resolve to a ProjectV2"}]}`
	if err := c.graphql(t.Context(), &r, "query", nil); err == nil || errors.As(err, &p) || !strings.Contains(err.Error(), "Could not resolve to a ProjectV2") {
		t.Errorf("no data: %v", err)
	}
}

func TestAMoveNeverTakesAPartialAnswerForAnIssueOffTheBoard(t *testing.T) {
	f := &fakeGH{t: t, item: `{"data":{"repository":{"issue":{"id":"ISSUE","projectItems":{"nodes":[null]}}}},"errors":[{"type":"RESOURCE_LIMITS_EXCEEDED","path":["repository","issue","projectItems","nodes",0],"message":"` + limits + `"}]}`}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	if _, err := c.Move(t.Context(), issue5, "in progress"); err == nil || !strings.Contains(err.Error(), limits) || len(f.mutations) != 0 {
		t.Errorf("Move = %v, mutations %v", err, f.mutations)
	}
}

func TestExtrasBatchesBySubIssues(t *testing.T) {
	var parts []extrasPart
	for i, n := range []int{50, 50, 50, 20, 0, 200, 10} {
		parts = append(parts, extrasPart{field: fmt.Sprint(i), subs: n})
	}
	var got []string
	for _, b := range extrasBatches(parts) {
		var fs []string
		for _, p := range b {
			fs = append(fs, p.field)
		}
		got = append(got, strings.Join(fs, ","))
	}
	if want := []string{"0,1,2", "3,4", "5", "6"}; !slices.Equal(got, want) {
		t.Errorf("batches %q, want %q", got, want)
	}
}

// aliases are the aliased fields of an extras request.
var aliases = regexp.MustCompile(`\b([es]\d+):(?:repository|search)\(`)

func TestReadBatchesTheExtrasAndKeepsAPartialAnswer(t *testing.T) {
	epic := func(n int) string {
		return node(inProgress, `{"name":"Epic 🎯"}`, issue(n, `,"subIssuesSummary":{"total":50,"completed":40}`))
	}
	onP := func(status string) string {
		return fmt.Sprintf(`,"projectItems":{"nodes":[{"project":{"id":"P"},"status":{"name":%q}}]}`, status)
	}
	var asked [][]string
	f := &fakeGH{t: t, pages: []string{
		`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":false},"nodes":[` +
			epic(2) + "," + epic(3) + "," + epic(4) + "," + epic(5) + "," + node(upNext, "null", issue(9, "")) + `]}}}}}`,
	}, extrasBy: func(q string) string {
		var data, errs []string
		var names []string
		for _, m := range aliases.FindAllStringSubmatch(q, -1) {
			a := m[1]
			names = append(names, a)
			switch a {
			case "e3":
				// The last epic's sub-issues answer without their board
				// fields: 103 is off the board read, 9 one of its items.
				data = append(data, `"e3":{"issue":{"subIssues":{"nodes":[`+issue(103, `,"projectItems":null`)+","+issue(9, `,"projectItems":null`)+`]}}}`)
				for k := range 2 {
					errs = append(errs, fmt.Sprintf(`{"type":"RESOURCE_LIMITS_EXCEEDED","path":["e3","issue","subIssues","nodes",%d,"projectItems"],"message":%q}`, k, limits))
				}
			case "s2":
				data = append(data, `"s2":null`)
				errs = append(errs, fmt.Sprintf(`{"type":"RESOURCE_LIMITS_EXCEEDED","path":["s2"],"message":%q}`, limits))
			default:
				n := 100 + int(a[1]-'0')
				data = append(data, fmt.Sprintf(`%q:{"issue":{"subIssues":{"nodes":[%s]}}}`, a, issue(n, onP(inProgress))))
			}
		}
		asked = append(asked, names)
		return `{"data":{` + strings.Join(data, ",") + `},"errors":[` + strings.Join(errs, ",") + `]}`
	}}
	b := readBoard()
	snap, err := (&Client{GH: f.run, Board: b}).Read(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	// Each epic reads 50 sub-issues: three to a request, the search with
	// the last.
	if want := [][]string{{"e0", "e1", "e2"}, {"e3", "s2"}}; !slices.EqualFunc(asked, want, slices.Equal) {
		t.Errorf("requests %q, want %q", asked, want)
	}
	var got []string
	for _, c := range Rank(snap, b, now) {
		got = append(got, strings.TrimSpace(c.Ref+" "+c.Why()+" "+c.Skip))
	}
	want := []string{
		"o/r#100 epics, sub-issue of o/r#2 (In Progress ⛏️)",
		"o/r#101 epics, sub-issue of o/r#3 (In Progress ⛏️)",
		"o/r#102 epics, sub-issue of o/r#4 (In Progress ⛏️)",
		"o/r#103 epics, sub-issue of o/r#5 board fields unknown: projectItems: " + limits,
		"o/r#9 epics, sub-issue of o/r#5 (Up Next ➡️)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("read and ranked:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{`board.order "queue": ` + limits}; !slices.Equal(snap.Missing, want) {
		t.Errorf("snapshot missing %q, want %q", snap.Missing, want)
	}
}

func TestAnEpicWhoseSubIssuesAreUnknownIsOfferedSkipped(t *testing.T) {
	b := config.Board{Order: []config.BoardStep{{Name: "epics", Status: []string{inProgress}, SubIssues: true}}}
	snap := &Snapshot{Order: b.Order, Items: []Item{{Ref: epicRef, OnBoard: true, Status: inProgress, OpenSubIssues: 3, Updated: now,
		Missing: []string{"issue.subIssues: " + limits}}}}
	got := Rank(snap, b, now)
	if len(got) != 1 || got[0].Ref != epicRef || got[0].Skip != "sub-issues unknown: issue.subIssues: "+limits {
		t.Errorf("Rank = %+v", got)
	}
}

func TestAReadRetriesAnEmptyOrCutOffAnswer(t *testing.T) {
	answers := []string{"", `{"data":{"repository":{"issueOrPull`, `{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN"}}}}`}
	calls := 0
	gh := func(context.Context, ...string) ([]byte, error) {
		calls++
		return []byte(answers[min(calls, len(answers))-1]), nil
	}
	c := &Client{GH: github.Retrying(gh, time.Millisecond)}
	if open, err := c.Open(t.Context(), "example/tools#5"); err != nil || !open || calls != 3 {
		t.Errorf("Open = %v, %v after %d calls", open, err, calls)
	}

	answers, calls = []string{""}, 0
	if _, err := c.Open(t.Context(), "example/tools#5"); err == nil || !strings.HasPrefix(err.Error(), "gh api graphql (repository): an empty body after 3 tries") {
		t.Errorf("Open of a persistent empty answer = %v after %d calls", err, calls)
	}
}
