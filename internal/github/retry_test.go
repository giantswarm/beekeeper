package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// answers is a GH that gives its answers in turn, the last one for good,
// and counts the calls.
type answers struct {
	outs  []string
	errs  []error
	calls int
}

func (a *answers) run(context.Context, ...string) ([]byte, error) {
	i := min(a.calls, len(a.outs)-1)
	a.calls++
	return []byte(a.outs[i]), a.errs[i]
}

const query = `query($o:String!){repositoryOwner(login:$o){id}}`

var graphqlRead = []string{ghAPI, graphqlEndpoint, "-f", "query=" + query, "-f", "o=example"}

func TestRetryingRetriesATransientRead(t *testing.T) {
	ok := `{"data":{"repositoryOwner":{"id":"X"}}}`
	cut := errors.New("gh api graphql -f: exit status 1: unexpected end of JSON input")
	for name, a := range map[string]*answers{
		"empty body":        {outs: []string{"", ok}, errs: []error{nil, nil}},
		"truncated body":    {outs: []string{`{"data":{"repositoryOw`, ok}, errs: []error{nil, nil}},
		"gh's decode error": {outs: []string{"", ok}, errs: []error{cut, nil}},
		"HTTP 502":          {outs: []string{"", ok}, errs: []error{errors.New("gh api graphql -f: exit status 1: HTTP 502: Bad Gateway (https://api.github.com/graphql)"), nil}},
		"504 of the client": {outs: []string{"", ok}, errs: []error{errors.New("gh pr view 1: exit status 1: non-200 OK status code: 504 Gateway Timeout body: \"\""), nil}},
		"empty, then cut":   {outs: []string{"", `{"data":`, ok}, errs: []error{nil, cut, nil}},
	} {
		out, err := Retrying(a.run, time.Millisecond)(t.Context(), graphqlRead...)
		if err != nil || string(out) != ok || a.calls < 2 {
			t.Errorf("%s: %q, %v after %d calls", name, out, err, a.calls)
		}
	}
}

func TestRetryingNamesTheCallOfAPersistentFailure(t *testing.T) {
	a := &answers{outs: []string{""}, errs: []error{nil}}
	_, err := Retrying(a.run, time.Millisecond)(t.Context(), graphqlRead...)
	if err == nil || !errors.Is(err, errNoBody) || a.calls != readTries ||
		err.Error() != "gh api graphql (repositoryOwner): an empty body after 3 tries: no complete JSON body" {
		t.Errorf("%v after %d calls", err, a.calls)
	}
	a = &answers{outs: []string{""}, errs: []error{errors.New("gh issue view 7: exit status 1: HTTP 503: Service Unavailable")}}
	_, err = Retrying(a.run, time.Millisecond)(t.Context(), "issue", "view", "7", "--json", "title")
	if err == nil || a.calls != readTries || !strings.HasPrefix(err.Error(), "gh issue view 7: HTTP 503 after 3 tries: ") {
		t.Errorf("%v after %d calls", err, a.calls)
	}
}

func TestRetryingStopsWithItsContext(t *testing.T) {
	a := &answers{outs: []string{""}, errs: []error{nil}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Retrying(a.run, time.Hour)(ctx, graphqlRead...); err == nil || a.calls != 1 || !strings.Contains(err.Error(), "retry cancelled") {
		t.Errorf("%v after %d calls", err, a.calls)
	}
}

func TestRetryingAsksAWriteOrAFinalAnswerOnce(t *testing.T) {
	bad := errors.New("exit status 1: HTTP 502: Bad Gateway")
	for name, c := range map[string]struct {
		args []string
		out  string
		err  error
	}{
		"a mutation":                {[]string{ghAPI, graphqlEndpoint, "-f", "query=mutation{addComment(input:{}){clientMutationId}}"}, "", bad},
		"a REST call with a body":   {[]string{ghAPI, "repos/o/r/issues", "-f", "title=x"}, "", bad},
		"a PATCH":                   {[]string{ghAPI, "-X", "PATCH", "repos/o/r/pulls/1"}, "", bad},
		"a merge":                   {[]string{"pr", "merge", "1"}, "", bad},
		"GitHub's errors with data": {graphqlRead, `{"data":{"repositoryOwner":null},"errors":[{"message":"Could not resolve"}]}`, errors.New("exit status 1: GraphQL: Could not resolve")},
		"a 404":                     {[]string{ghAPI, "repos/o/r"}, `{"message":"Not Found"}`, errors.New("exit status 1: HTTP 404: Not Found")},
		"a filtered empty answer":   {[]string{ghAPI, "user", "--jq", ".login"}, "", nil},
		"a raw file":                {[]string{ghAPI, "-H", "Accept: application/vnd.github.raw", "repos/o/r/contents/f"}, "", nil},
		"a list's plain output":     {[]string{"pr", "list"}, "", nil},
	} {
		a := &answers{outs: []string{c.out}, errs: []error{c.err}}
		out, err := Retrying(a.run, time.Millisecond)(t.Context(), c.args...)
		if a.calls != 1 || string(out) != c.out || !errors.Is(err, c.err) && err != c.err {
			t.Errorf("%s: %q, %v after %d calls", name, out, err, a.calls)
		}
	}
}

// killedGH is a GH whose first hang tries hang until their context ends and
// die as exec.CommandContext's SIGKILL leaves gh; the next answer ok.
type killedGH struct {
	hang, calls int
}

func (k *killedGH) run(ctx context.Context, _ ...string) ([]byte, error) {
	k.calls++
	if k.calls > k.hang {
		return []byte(`{"data":{"repositoryOwner":{"id":"X"}}}`), nil
	}
	<-ctx.Done()
	return nil, errors.New("gh api graphql -f: signal: killed: ")
}

func TestRetryingRetriesAKilledTry(t *testing.T) {
	k := &killedGH{hang: 1}
	out, err := Retrying(k.run, time.Millisecond)(TryTimeout(t.Context(), 10*time.Millisecond), graphqlRead...)
	if err != nil || k.calls != 2 || !strings.Contains(string(out), `"X"`) {
		t.Errorf("a try killed at its timeout: %q, %v after %d calls", out, err, k.calls)
	}

	a := &answers{outs: []string{"", `{"data":{}}`}, errs: []error{errors.New("gh api graphql -f: signal: killed: "), nil}}
	if out, err := Retrying(a.run, time.Millisecond)(t.Context(), graphqlRead...); err != nil || a.calls != 2 || string(out) != `{"data":{}}` {
		t.Errorf("a gh the machine killed: %q, %v after %d calls", out, err, a.calls)
	}

	k = &killedGH{hang: readTries}
	_, err = Retrying(k.run, time.Millisecond)(TryTimeout(t.Context(), 10*time.Millisecond), graphqlRead...)
	if k.calls != readTries || err == nil || err.Error() != "gh api graphql (repositoryOwner): no answer within 10ms after 3 tries: gh api graphql -f: signal: killed: " {
		t.Errorf("every try killed: %v after %d calls", err, k.calls)
	}
}

func TestRetryingDoesNotOutliveItsContext(t *testing.T) {
	k := &killedGH{hang: readTries}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := Retrying(k.run, time.Hour)(TryTimeout(ctx, time.Hour), graphqlRead...)
	if k.calls != 1 || err == nil || !strings.Contains(err.Error(), "a killed gh, retry cancelled") {
		t.Errorf("the read's own deadline: %v after %d calls", err, k.calls)
	}
}

func TestIsRead(t *testing.T) {
	for args, want := range map[string]bool{
		"api graphql -f query={viewer{login}}":   true,
		"api graphql -f query=mutation{x}":       false,
		"api graphql -F query=@q.graphql":        false,
		"api repos/o/r/pulls/1":                  true,
		"api --method=GET search/issues -f q=x":  true,
		"api repos/o/r/issues -F title=x":        false,
		"api --input body.json repos/o/r/issues": false,
		"issue view 7 --json title":              true,
		"pr checks 1":                            true,
		"search prs --author x":                  true,
		"project item-list 1 --owner o":          true,
		"pr merge 1 --squash":                    false,
		"issue comment 7 --body x":               false,
		"":                                       false,
	} {
		if got := isRead(strings.Fields(args)); got != want {
			t.Errorf("isRead(%q) = %v", args, got)
		}
	}
}
