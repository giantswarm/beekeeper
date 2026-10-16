package github

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestRefStatesBoundsEachTryByItsBatch(t *testing.T) {
	tries := map[string]time.Duration{}
	run := func(ctx context.Context, _ ...string) ([]byte, error) {
		tries[Repo(ctx)], _ = ctx.Value(tryKey{}).(time.Duration)
		return []byte(`{"data":{}}`), nil
	}
	refs := []PR{{Repo: "example/a", N: 1}, {Repo: "example/b", N: 2}, {Repo: "other/c", N: 3}}
	if _, err := RefStates(t.Context(), run, refs); err != nil {
		t.Fatal(err)
	}
	if want := map[string]time.Duration{"example/a": 10200 * time.Millisecond, "other/c": 10100 * time.Millisecond}; !maps.Equal(tries, want) {
		t.Errorf("tries %v, want %v", tries, want)
	}
}

func TestRefStatesResolvesAPersonalAccountRepository(t *testing.T) {
	var acting []string
	run := ownerLink(&acting, func(query string) string {
		if strings.Contains(query, `owner:"teemow"`) {
			return `{"data":{"r0":{"issueOrPullRequest":{"state":"OPEN"}}}}`
		}
		return `{"data":{"r0":{"issueOrPullRequest":{"state":"MERGED"}},"r1":{"issueOrPullRequest":{"state":"OPEN"}}}}`
	})
	refs := []PR{{Repo: orgRepo, N: 1}, {Repo: personalRepo, N: 7}, {Repo: "giantswarm/devctl", N: 2}}
	states, err := RefStates(context.Background(), run, refs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[PR]RefState{refs[0]: {State: Merged}, refs[1]: {State: Open}, refs[2]: {State: Open}}
	if len(states) != len(want) {
		t.Errorf("RefStates = %v, want %v", states, want)
	}
	for r, s := range want {
		if states[r] != s {
			t.Errorf("%v: %+v, want %+v", r, states[r], s)
		}
	}
	if !slices.Equal(acting, []string{orgRepo, personalRepo}) {
		t.Errorf("requests acted on %q, want one per owner", acting)
	}
}
