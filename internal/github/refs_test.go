package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRefStates(t *testing.T) {
	var query string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		query = args[len(args)-1]
		return []byte(`{"data":{"r0":{"issueOrPullRequest":{"state":"CLOSED","timelineItems":{"nodes":[{"closer":null}]}}},"r1":{"issueOrPullRequest":{"state":"MERGED"}},"r2":{"issueOrPullRequest":null},` +
			// Closed by a pull request's closing keyword at its squash merge: the closer is the merge commit.
			`"r3":{"issueOrPullRequest":{"state":"CLOSED","timelineItems":{"nodes":[{"closer":{"__typename":"Commit","abbreviatedOid":"f880a16","associatedPullRequests":{"nodes":[{"number":9,"repository":{"nameWithOwner":"example/notes"}}]}}}]}}},` +
			// Closed by a pull request directly, by a bare commit, and reopened after a keyword close.
			`"r4":{"issueOrPullRequest":{"state":"CLOSED","timelineItems":{"nodes":[{"closer":{"__typename":"PullRequest","number":10,"repository":{"nameWithOwner":"example/other"}}}]}}},` +
			`"r5":{"issueOrPullRequest":{"state":"CLOSED","timelineItems":{"nodes":[{"closer":{"__typename":"Commit","abbreviatedOid":"abc1234","associatedPullRequests":{"nodes":[]}}}]}}},` +
			`"r6":{"issueOrPullRequest":{"state":"OPEN","timelineItems":{"nodes":[{"closer":{"__typename":"PullRequest","number":10,"repository":{"nameWithOwner":"example/other"}}}]}}}}}`), errors.New("exit status 1")
	}
	const repo = "example/notes"
	refs := []PR{{Repo: repo, N: 1}, {Repo: repo, N: 2}, {Repo: repo, N: 404}, {Repo: repo, N: 3}, {Repo: repo, N: 4}, {Repo: repo, N: 5}, {Repo: repo, N: 6}}
	states, err := RefStates(context.Background(), run, refs)
	if err != nil || len(states) != 6 {
		t.Fatalf("RefStates = %v, %v", states, err)
	}
	for i, want := range map[int]RefState{
		0: {State: Closed},
		1: {State: Merged},
		3: {State: Closed, Closer: "example/notes#9"},
		4: {State: Closed, Closer: "example/other#10"},
		5: {State: Closed, Closer: "commit abc1234"},
		6: {State: Open},
	} {
		if got := states[refs[i]]; got != want {
			t.Errorf("r%d: %+v, want %+v", i, got, want)
		}
	}
	if _, ok := states[refs[2]]; ok {
		t.Errorf("an unanswered reference has a state: %v", states)
	}
	if !strings.Contains(query, `r0:repository(owner:"example",name:"notes"){issueOrPullRequest(number:1){...on Issue{state timelineItems(last:1,itemTypes:[CLOSED_EVENT])`) {
		t.Errorf("query %s", query)
	}
	if _, err := RefStates(context.Background(), func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }, refs); err == nil {
		t.Error("RefStates without an answer: no error")
	}
	if states, err := RefStates(context.Background(), nil, nil); states != nil || err != nil {
		t.Error("RefStates of nothing asks GitHub")
	}
}
