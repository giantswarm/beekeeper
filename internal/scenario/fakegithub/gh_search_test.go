//go:build scenario

package fakegithub

import (
	"strings"
	"testing"
)

// gh search prs as the supervisor loop's sweep runs it: the desk's open
// pull requests in its organizations, each with its repository and number.
func TestGHSearchPRs(t *testing.T) {
	s, _ := build(t, fixture)
	sweep := []string{"search", "prs", "--state", "open", "--json", "repository,number", "--limit", "1000", "--author", "desk", "--owner", "example"}
	if out := mustGH(t, s, sweep...); out != `[{"number":4,"repository":{"name":"app","nameWithOwner":"example/app"}}]`+"\n" {
		t.Fatalf("the sweep's listing: %q", out)
	}
	for name, args := range map[string][]string{
		"another author":       {"search", "prs", "--state", "open", "--json", "number", "--author", "nobody"},
		"another organization": {"search", "prs", "--state", "open", "--json", "number", "--author", "desk", "--owner", "elsewhere"},
		"another repository":   {"search", "prs", "--json", "number", "--author", "desk", "--repo", "example/other"},
		"closed only":          {"search", "prs", "--state", "closed", "--json", "number", "--author", "desk"},
	} {
		if out := mustGH(t, s, args...); out != "[]\n" {
			t.Errorf("%s: %q, want none", name, out)
		}
	}
	// The other fields it exports, and --jq over them.
	pr := decode(t, []byte(mustGH(t, s, "search", "prs", "--json", "number,title,state,url,isDraft,isPullRequest", "--repo", "example/app", "--jq", ".[0]")))
	for k, want := range map[string]any{"number": float64(4), "title": "Fix the first", "state": "open", "url": "https://github.com/example/app/pull/4", "isDraft": false, "isPullRequest": true} {
		if pr[k] != want {
			t.Errorf("%s: %v, want %v", k, pr[k], want)
		}
	}

	// It asks REST's search for pull requests, as gh writes the query.
	last := s.Calls()[len(s.Calls())-1]
	if last.Path != "search/issues" || last.Bucket != bucketSearch {
		t.Errorf("the last call: %s in bucket %s, want search/issues in %s", last.Path, last.Bucket, bucketSearch)
	}
	if q := searchPRsQuery(flags{vals: map[string][]string{"author": {"desk"}, "owner": {"example", "other"}, "repo": {"example/app"}, "state": {"open"}}, pos: []string{"fix"}}); q != "fix author:desk repo:example/app state:open type:pr user:example user:other" {
		t.Errorf("the query: %q", q)
	}

	// What the translator does not serve is no guess.
	if res := gh(s, "test", "search", "prs", "--author", "desk"); res.Handled {
		t.Errorf("search prs without --json was answered: %+v", res)
	}
	if res := gh(s, "test", "search", "prs", "--json", "number", "--draft"); res.Handled && res.Exit == 0 {
		t.Errorf("a flag the translator does not know was answered: %+v", res)
	}
	if res := gh(s, "test", "search", "prs", "--json", "number,commentsCount"); res.Handled || !strings.Contains(res.Reason, `"commentsCount"`) {
		t.Errorf("a field gh knows and the translator does not serve was answered: %+v", res)
	}
	if res := gh(s, "test", "search", "prs", "--json", "headRefName"); res.Exit != 1 || !strings.Contains(res.Stderr, `Unknown JSON field: "headRefName"`) || !strings.Contains(res.Stderr, "commentsCount") {
		t.Errorf("a field gh does not know: exit %d, %q", res.Exit, res.Stderr)
	}
	if res := gh(s, "test", "search", "prs", "--json", "number", "--state", "merged"); res.Exit != 1 || !strings.Contains(res.Stderr, "valid values are {open|closed}") {
		t.Errorf("a state search does not have: exit %d, %q", res.Exit, res.Stderr)
	}
	if res := gh(s, "test", "search", "prs", "--json", "number", "--limit", "0"); res.Exit != 1 || !strings.Contains(res.Stderr, "between 1 and 1000") {
		t.Errorf("a limit of none: exit %d, %q", res.Exit, res.Stderr)
	}
}
