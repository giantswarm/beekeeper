package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The beekeeper person calls the members refusals name.
const (
	personTeemow = "beekeeper person teemow --org giantswarm"
	personInOrg  = "beekeeper person <login> --org giantswarm"
	personAnyOrg = "beekeeper person <login> --org <org>"
)

func TestMembersRefusal(t *testing.T) {
	const worker = "sess-worker"
	h := Hook{Started: func(s string) bool { return s == worker }}
	for name, tc := range map[string]struct {
		cmd     string
		refused bool
		person  string
	}{
		"the org's member check":          {"gh api orgs/giantswarm/members/teemow", true, personTeemow},
		"by path, with a method":          {"/usr/bin/gh api -X GET /orgs/giantswarm/members/teemow", true, personTeemow},
		"the full URL, with the headers":  {"gh api -i https://api.github.com/orgs/giantswarm/members/teemow", true, personTeemow},
		"the members list, paginated":     {"gh api --paginate 'orgs/giantswarm/members?per_page=100' | jq -r '.[].login'", true, personInOrg},
		"a membership":                    {"gh api orgs/giantswarm/memberships/teemow", true, personTeemow},
		"the org's teams":                 {"gh api orgs/giantswarm/teams", true, personInOrg},
		"a team's members":                {"gh api orgs/giantswarm/teams/team-bumblebee/members --jq '.[].login'", true, personInOrg},
		"a team membership":               {"gh api orgs/giantswarm/teams/team-bumblebee/memberships/teemow", true, personInOrg},
		"a team by id":                    {"gh api teams/1234/members", true, personAnyOrg},
		"a login's orgs":                  {"gh api users/teemow/orgs", true, "beekeeper person teemow --org <org>"},
		"after a cd, with a header":       {"cd /tmp && gh api -H 'Accept: application/vnd.github+json' orgs/giantswarm/members/teemow", true, personTeemow},
		"with variables":                  {`gh api "orgs/$ORG/members/$LOGIN"`, true, "beekeeper person $LOGIN --org $ORG"},
		"under a timeout":                 {"timeout 30 gh api orgs/giantswarm/members/teemow", true, personTeemow},
		"graphql membersWithRole":         {`gh api graphql -f query='{ organization(login: "giantswarm") { membersWithRole(first: 100) { nodes { login } } } }'`, true, personAnyOrg},
		"graphql a team's members, lines": {"gh api graphql -f query='\nquery {\n  organization(login: \"giantswarm\") {\n    team(slug: \"team-bumblebee\") { members(first: 100) { nodes { login } } }\n  }\n}'", true, personAnyOrg},
		"graphql the org's teams":         {`gh api graphql -f query='{ organization(login: "giantswarm") { teams(first: 50) { nodes { slug } } } }'`, true, personAnyOrg},
		"graphql a user's organizations":  {`gh api graphql -f query='{ user(login: "teemow") { organizations(first: 10) { nodes { login } } } }'`, true, personAnyOrg},
		"graphql from a heredoc":          {"gh api graphql -f query=@- <<'EOF'\n{ organization(login: \"giantswarm\") { membersWithRole(first: 1) { totalCount } } }\nEOF", true, personAnyOrg},
		"graphql the board":               {`gh api graphql -f query='{ organization(login: "giantswarm") { projectV2(number: 273) { items(first: 20, query: "repo:giantswarm/beekeeper") { nodes { id } } } } }'`, false, ""},
		"graphql a repository":            {`gh api graphql -f query='{ repository(owner: "giantswarm", name: "beekeeper") { issue(number: 566) { title } } }'`, false, ""},
		"beekeeper person":                {"beekeeper person teemow", false, ""},
		"beekeeper person, another org":   {"beekeeper person teemow --org giantswarm --refresh", false, ""},
		"an issue read":                   {"gh api repos/giantswarm/beekeeper/issues/566", false, ""},
		"a repository's collaborators":    {"gh api repos/giantswarm/beekeeper/collaborators", false, ""},
		"the caller's own memberships":    {"gh api user/memberships/orgs/giantswarm", false, ""},
		"an org read":                     {"gh api orgs/giantswarm", false, ""},
		"an org's repositories":           {"gh api orgs/giantswarm/repos --paginate", false, ""},
		"a comment naming the path":       {`gh api repos/giantswarm/beekeeper/issues/566/comments -f body="the hook refuses orgs/giantswarm/members/<login>"`, false, ""},
		"a grep for it":                   {"grep -rn 'orgs/giantswarm/members' internal/guard/", false, ""},
		"gh issue view":                   {"gh issue view 566 --repo giantswarm/beekeeper", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := h.membersRefusal(tc.cmd, worker, t.TempDir())
			if (r != "") != tc.refused {
				t.Fatalf("membersRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
			if !tc.refused {
				return
			}
			if !strings.Contains(r, "`"+tc.person+"`") {
				t.Errorf("the refusal does not name %q: %q", tc.person, r)
			}
			if p := h.membersRefusal(tc.cmd, "person", t.TempDir()); p != "" {
				t.Errorf("the person's own session is refused: %q", p)
			}
			if p := (Hook{}).membersRefusal(tc.cmd, worker, t.TempDir()); p != "" {
				t.Errorf("without a start check the hook refuses: %q", p)
			}
		})
	}
}

func TestMembersRefusalReadsTheQueryFile(t *testing.T) {
	const worker = "sess-worker"
	h := Hook{Started: func(s string) bool { return s == worker }}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "q.graphql"), []byte(`{ organization(login: "giantswarm") { membersWithRole(first: 100) { nodes { login } } } }`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "q.json"), []byte(`{"query": "{ organization(login: \"giantswarm\") { team(slug: \"x\") { members(first: 1) { totalCount } } } }"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		cmd     string
		refused bool
	}{
		"a raw field from a file":    {"gh api graphql -F query=@q.graphql", true},
		"the input file":             {"gh api graphql --input q.json", true},
		"the input file, = form":     {"gh api graphql --input=" + filepath.Join(dir, "q.json"), true},
		"a file that is not there":   {"gh api graphql -f query=@missing.graphql", false},
		"a file of another query":    {"gh api repos/giantswarm/beekeeper/issues/566/comments --input q.json", false},
		"the person's own file read": {"cat q.graphql", false},
	} {
		t.Run(name, func(t *testing.T) {
			if r := h.membersRefusal(tc.cmd, worker, dir); (r != "") != tc.refused {
				t.Fatalf("membersRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
		})
	}
}

func TestGHAPIEndpoint(t *testing.T) {
	const members = "orgs/giantswarm/members"
	for cmd, want := range map[string]string{
		"gh api orgs/giantswarm":                                                   "orgs/giantswarm",
		"gh api -X GET orgs/giantswarm/members/teemow":                             "orgs/giantswarm/members/teemow",
		"gh api --method=GET --paginate orgs/giantswarm/members":                   members,
		"gh api -XGET /orgs/giantswarm/teams":                                      "/orgs/giantswarm/teams",
		"gh api -f body=orgs/giantswarm/members repos/a/b/issues/1/comments":       "repos/a/b/issues/1/comments",
		"gh api -F body=@orgs/giantswarm/members repos/a/b/issues/1/comments":      "repos/a/b/issues/1/comments",
		"gh api --jq '.[].login' --hostname github.com orgs/giantswarm/members -i": members,
		"/usr/bin/gh api graphql -f query=x":                                       "graphql",
		"GH_PAGER= gh api orgs/giantswarm/members":                                 members,
		"beekeeper run -- gh api orgs/giantswarm/members":                          members,
		"gh api":                              "",
		"gh api --paginate":                   "",
		"gh issue view 566":                   "",
		"beekeeper person teemow":             "",
		"echo gh api orgs/giantswarm/members": "",
		`gh api -H "Accept: application/vnd.github+json" orgs/giantswarm/memberships/x`: "orgs/giantswarm/memberships/x",
	} {
		if got := ghAPIEndpoint(shellWords(cmd)); got != want {
			t.Errorf("ghAPIEndpoint(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestDecideRefusesMembersRead(t *testing.T) {
	const worker = "sess-worker"
	read := false
	h := Hook{Self: hookSelf, Shell: hookShell, Started: func(s string) bool { read = true; return s == worker }}
	decide := func(session, cmd string) *decision {
		ev := toolEvent(bashTool, map[string]any{commandKey: cmd})
		ev["cwd"] = t.TempDir()
		ev["session_id"] = session
		return decideEvent(t, h, ev)
	}
	if d := decide(worker, "beekeeper person teemow"); (d != nil && d.PermissionDecision == decisionDeny) || read {
		t.Errorf("beekeeper person refused (%+v) or the start read for it (%v)", d, read)
	}
	if d := decide(worker, "gh api repos/giantswarm/beekeeper/issues/566"); (d != nil && d.PermissionDecision == decisionDeny) || read {
		t.Errorf("an issue read refused (%+v) or the start read for it (%v)", d, read)
	}
	if d := decide(worker, "gh api orgs/giantswarm/members/teemow"); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "`beekeeper person teemow --org giantswarm`") {
		t.Errorf("a worker's member check not refused naming beekeeper person: %+v", d)
	}
	if d := decide("person", "gh api orgs/giantswarm/members/teemow"); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("the person's own member check refused: %+v", d)
	}
}
