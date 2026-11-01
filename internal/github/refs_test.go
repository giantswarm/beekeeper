package github

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRefStates(t *testing.T) {
	var query string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		query = args[len(args)-1]
		return []byte(`{"data":{"r0":{"issueOrPullRequest":{"__typename":"Issue","state":"CLOSED","timelineItems":{"nodes":[{"closer":null}]}}},"r1":{"issueOrPullRequest":{"__typename":"PullRequest","state":"MERGED"}},"r2":{"issueOrPullRequest":null},` +
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
		0: {Kind: RefIssue, State: Closed},
		1: {Kind: RefPullRequest, State: Merged},
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
	if !strings.Contains(query, `r0:repository(owner:"example",name:"notes"){issueOrPullRequest(number:1){__typename ...on Issue{state timelineItems(last:1,itemTypes:[CLOSED_EVENT])`) {
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

func TestRefStatesReadsALargeParkedSetInBatches(t *testing.T) {
	var asked []int
	tries := map[time.Duration]int{}
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		query := args[len(args)-1]
		n := strings.Count(query, ":repository(")
		asked = append(asked, n)
		d, _ := ctx.Value(tryKey{}).(time.Duration)
		tries[d]++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a batch's read has no deadline of its own")
		}
		// The second batch is killed: it fails alone.
		if strings.Contains(query, "number:21)") {
			return nil, errors.New("gh api graphql: signal: killed")
		}
		var b strings.Builder
		b.WriteString(`{"data":{`)
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"r` + strconv.Itoa(i) + `":{"issueOrPullRequest":{"state":"OPEN"}}`)
		}
		b.WriteString("}}")
		return []byte(b.String()), nil
	}
	var refs []PR
	for n := 1; n <= 45; n++ {
		refs = append(refs, PR{Repo: "example/notes", N: n})
	}
	states, err := RefStates(t.Context(), run, refs)
	if !slices.Equal(asked, []int{20, 20, 5}) {
		t.Errorf("requests asked %v references, want batches of at most 20", asked)
	}
	if want := map[time.Duration]int{12 * time.Second: 2, 10500 * time.Millisecond: 1}; !maps.Equal(tries, want) {
		t.Errorf("tries %v, want each bounded by its own batch %v", tries, want)
	}
	if err == nil || !strings.Contains(err.Error(), "refs batch 2/3 of example (20: example/notes#21 … example/notes#40): gh api graphql: signal: killed") {
		t.Errorf("err = %v, want the failed batch named", err)
	}
	if strings.Contains(err.Error(), "batch 1/3") || strings.Contains(err.Error(), "batch 3/3") {
		t.Errorf("err = %v names a batch that answered", err)
	}
	if len(states) != 25 || states[refs[0]].State != Open || states[refs[44]].State != Open {
		t.Errorf("states of the answered batches = %v", states)
	}
	if _, ok := states[refs[20]]; ok {
		t.Error("a reference of the failed batch has a state")
	}
}

// A pull request's newest review or comment by anyone but its author is its
// Reply; a pending review (no submittedAt) and the author's own are left out.
func TestRefStatesReadsAPullRequestsNewestReply(t *testing.T) {
	var query string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		query = args[len(args)-1]
		return []byte(`{"data":{"r0":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"worker-app"},` +
			`"reviews":{"nodes":[{"author":{"login":"reviewer"},"state":"CHANGES_REQUESTED","submittedAt":"2026-11-01T10:00:00Z"},{"author":{"login":"reviewer"},"state":"PENDING","submittedAt":null}]},` +
			`"comments":{"nodes":[{"author":{"login":"worker-app"},"createdAt":"2026-11-01T12:00:00Z"},{"author":null,"createdAt":"2026-11-01T11:00:00Z"}]}}},` +
			`"r1":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"worker-app"},"reviews":{"nodes":[]},"comments":{"nodes":[{"author":{"login":"worker-app"},"createdAt":"2026-11-01T12:00:00Z"}]}}}}}`), nil
	}
	const repo = "example/notes"
	refs := []PR{{Repo: repo, N: 1}, {Repo: repo, N: 2}}
	states, err := RefStates(t.Context(), run, refs)
	if err != nil {
		t.Fatal(err)
	}
	want := RefState{State: Open, Reply: Reply{By: "ghost", At: time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC), Kind: "comment"}}
	if got := states[refs[0]]; got != want {
		t.Errorf("r0: %+v, want %+v", got, want)
	}
	if got := states[refs[1]]; got != (RefState{State: Open}) {
		t.Errorf("r1, only its author's comment: %+v", got)
	}
	if !strings.Contains(query, "...on PullRequest{state author{login} reviews(last:10){nodes{author{login} state submittedAt}} comments(last:10){nodes{author{login} createdAt}}}") {
		t.Errorf("query %s", query)
	}
}
