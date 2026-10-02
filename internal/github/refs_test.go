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
		return []byte(`{"data":{"r0":{"issueOrPullRequest":{"state":"CLOSED"}},"r1":{"issueOrPullRequest":{"state":"MERGED"}},"r2":{"issueOrPullRequest":null}}}`), errors.New("exit status 1")
	}
	const repo = "example/notes"
	refs := []PR{{Repo: repo, N: 1}, {Repo: repo, N: 2}, {Repo: repo, N: 404}}
	states, err := RefStates(context.Background(), run, refs)
	if err != nil || states[refs[0]] != Closed || states[refs[1]] != Merged || len(states) != 2 {
		t.Errorf("RefStates = %v, %v", states, err)
	}
	if !strings.Contains(query, `r0:repository(owner:"example",name:"notes"){issueOrPullRequest(number:1)`) {
		t.Errorf("query %s", query)
	}
	if _, err := RefStates(context.Background(), func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }, refs); err == nil {
		t.Error("RefStates without an answer: no error")
	}
	if states, err := RefStates(context.Background(), nil, nil); states != nil || err != nil {
		t.Error("RefStates of nothing asks GitHub")
	}
}
