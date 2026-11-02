package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestThreadsReadsStatesAndParents(t *testing.T) {
	var query string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		query = args[len(args)-1]
		return []byte(`{"data":{` +
			`"r0":{"issueOrPullRequest":{"state":"OPEN","parent":{"number":1,"state":"CLOSED","repository":{"nameWithOwner":"example/app"}}}},` +
			`"r1":{"issueOrPullRequest":{"state":"MERGED"}},` +
			`"r2":{"issueOrPullRequest":null}}}`), errors.New("one reference not found")
	}
	const repo = "example/app"
	refs := []PR{{Repo: repo, N: 7}, {Repo: repo, N: 8}, {Repo: repo, N: 9}}
	got, err := Threads(context.Background(), run, refs)
	if err == nil {
		t.Error("a failed request is no error")
	}
	if !strings.Contains(query, "parent{number state repository{nameWithOwner}}") || !strings.Contains(query, "issueOrPullRequest(number:9)") {
		t.Errorf("query %s", query)
	}
	if th := got[refs[0]]; th != (Thread{State: Open, Parent: "example/app#1", ParentState: Closed}) {
		t.Errorf("sub-issue %+v", th)
	}
	if th := got[refs[1]]; th != (Thread{State: Merged}) {
		t.Errorf("pull request %+v", th)
	}
	if _, ok := got[refs[2]]; ok {
		t.Error("an unanswered reference is in the map")
	}
}
