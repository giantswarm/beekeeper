package guard

import (
	"strings"
	"testing"
)

// hookSelf and hookShell are the beekeeper binary and the shell the Decide
// tests' hook rewrites to.
const (
	hookSelf  = "/bin/beekeeper"
	hookShell = "/bin/sh"
)

func TestSearchRefusal(t *testing.T) {
	const worker = "sess-worker"
	h := Hook{Started: func(s string) bool { return s == worker }}
	for name, tc := range map[string]struct {
		cmd     string
		refused bool
	}{
		"across the org":                {`gh search issues --owner giantswarm "private search"`, true},
		"pull requests across the org":  {"gh search prs --owner giantswarm --state open dedupe", true},
		"with no scope":                 {"gh search issues 'is:open label:bug'", true},
		"org: in the query":             {`gh search issues "org:giantswarm token"`, true},
		"two repositories":              {"gh search issues --repo giantswarm/beekeeper --repo giantswarm/giantswarm token", true},
		"two repo: qualifiers":          {`gh search issues "repo:giantswarm/a repo:giantswarm/b token"`, true},
		"a flag and a qualifier":        {"gh search issues -R giantswarm/a repo:giantswarm/b token", true},
		"an excluded repository only":   {`gh search issues --owner giantswarm -- "token -repo:giantswarm/a"`, true},
		"by path, under a timeout":      {"timeout 30 /usr/bin/gh search issues --owner giantswarm token --json number", true},
		"in a pipeline after a cd":      {"cd /tmp && gh search issues --owner giantswarm token --json url | jq -r '.[].url'", true},
		"the REST search, org-wide":     {`gh api -X GET search/issues -f q='org:giantswarm is:issue token'`, true},
		"the REST search, in the path":  {`gh api 'search/issues?q=org%3Agiantswarm+is%3Aissue+token'`, true},
		"the REST search, full URL":     {`gh api "https://api.github.com/search/issues?q=is:issue+token"`, true},
		"one repository":                {"gh search issues --repo giantswarm/beekeeper token", false},
		"one repository, short, = form": {"gh search prs -R giantswarm/beekeeper --state open && gh search issues --repo=giantswarm/beekeeper x", false},
		"one repo: qualifier":           {`gh search issues "repo:giantswarm/beekeeper token" --limit 5`, false},
		"a variable repository":         {`for r in $(cat repos); do gh search issues --repo "$r" token; done`, false},
		"the REST search, one repo":     {`gh api -X GET search/issues -f q='repo:giantswarm/beekeeper is:issue token'`, false},
		"the REST search, encoded repo": {`gh api 'search/issues?q=repo%3Agiantswarm%2Fbeekeeper+is%3Aissue'`, false},
		"the per-repository listing":    {`gh issue list --repo giantswarm/beekeeper --state all --search "private search"`, false},
		"a pull request listing":        {`gh pr list --repo giantswarm/beekeeper --search "token"`, false},
		"repositories search":           {"gh search repos --owner giantswarm beekeeper", false},
		"code search":                   {"gh search code --owner giantswarm searchRefusal", false},
		"another REST search":           {"gh api 'search/repositories?q=org:giantswarm'", false},
		"a grep for the command":        {"grep -rn 'gh search issues --owner giantswarm' internal/guard/", false},
		"an echo naming it":             {"echo gh search issues --owner giantswarm x", false},
		"a comment naming it":           {`gh issue comment 757 --repo giantswarm/beekeeper --body "gh search issues --owner giantswarm misses"`, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := h.searchRefusal(tc.cmd, worker)
			if (r != "") != tc.refused {
				t.Fatalf("searchRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
			if !tc.refused {
				return
			}
			if !strings.Contains(r, "`gh issue list --repo <owner/repo> --state all --search") {
				t.Errorf("the refusal does not name the per-repository listing: %q", r)
			}
			if p := h.searchRefusal(tc.cmd, "person"); p != "" {
				t.Errorf("the person's own session is refused: %q", p)
			}
			if p := (Hook{}).searchRefusal(tc.cmd, worker); p != "" {
				t.Errorf("without a start check the hook refuses: %q", p)
			}
		})
	}
}

func TestDecideRefusesCrossRepositorySearch(t *testing.T) {
	const worker = "sess-worker"
	read := false
	h := Hook{Self: hookSelf, Shell: hookShell, Started: func(s string) bool { read = true; return s == worker }}
	decide := func(session, cmd string) *decision {
		ev := toolEvent(bashTool, map[string]any{commandKey: cmd})
		ev["cwd"] = t.TempDir()
		ev["session_id"] = session
		return decideEvent(t, h, ev)
	}
	if d := decide(worker, "gh search issues --repo giantswarm/beekeeper token"); (d != nil && d.PermissionDecision == decisionDeny) || read {
		t.Errorf("a one-repository search refused (%+v) or the start read for it (%v)", d, read)
	}
	if d := decide(worker, "gh search issues --owner giantswarm token"); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "gh issue list --repo") {
		t.Errorf("a worker's org-wide search not refused naming the listing: %+v", d)
	}
	if d := decide("person", "gh search issues --owner giantswarm token"); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("the person's own org-wide search refused: %+v", d)
	}
}
