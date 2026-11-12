//go:build scenario

package fakegithub

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// gh api prints GitHub's body as it is, filters it with --jq, shows the
// answer's head with --include, and says an answer that is no success in
// gh's words with exit 1, a 304 included.
func TestGHAPI(t *testing.T) {
	s, _ := build(t, fixture)
	if out := mustGH(t, s, "api", "repos/example/app", "--jq", ".full_name"); out != "example/app\n" {
		t.Fatalf("--jq of a string: %q", out)
	}
	if out := mustGH(t, s, "api", "repos/example/app/issues/1", "--jq", ".labels[].name"); out != "bug\n" {
		t.Fatalf("--jq of a list: %q", out)
	}
	if out := mustGH(t, s, "api", "repos/example/app", "--jq", "{name, private}"); out != `{"name":"app","private":false}`+"\n" {
		t.Fatalf("--jq of an object: %q", out)
	}
	if out := mustGH(t, s, "api", "repos/example/app", "--jq", ".description // empty"); out != "" {
		t.Fatalf("--jq of nothing: %q", out)
	}
	plain := mustGH(t, s, "api", "/repos/example/app")
	if at(t, decode(t, []byte(plain)), "default_branch") != "main" || strings.HasSuffix(plain, "\n") {
		t.Fatalf("the plain body: %q", plain)
	}

	missing := gh(s, "test", "api", "repos/example/nope")
	if !missing.Handled || missing.Exit != 1 || missing.Stderr != "gh: Not Found (HTTP 404)\n" || at(t, decode(t, []byte(missing.Stdout)), "message") != "Not Found" {
		t.Fatalf("a 404: %+v", missing)
	}

	head := mustGH(t, s, "api", "-i", "repos/example/app")
	if !strings.HasPrefix(head, "HTTP/2.0 200 OK\n") || !strings.Contains(head, "\r\n\r\n{") || !strings.Contains(head, "X-Ratelimit-Remaining: ") {
		t.Fatalf("--include: %q", head)
	}
	etag := regexp.MustCompile(`(?m)^Etag: (\S+)\r$`).FindStringSubmatch(head)
	if etag == nil {
		t.Fatalf("--include shows no ETag: %q", head)
	}
	before := spent(s, "test", bucketCore)
	cached := gh(s, "test", "api", "--include", "repos/example/app", "-H", "If-None-Match: "+etag[1])
	if cached.Exit != 1 || cached.Stderr != "gh: HTTP 304\n" || !strings.HasPrefix(cached.Stdout, "HTTP/2.0 304 Not Modified\n") {
		t.Fatalf("a conditional read through gh: %+v", cached)
	}
	if spent(s, "test", bucketCore) != before {
		t.Fatalf("the 304 cost %d points", spent(s, "test", bucketCore)-before)
	}

	owner := s.GH(t.Context(), GHCall{Args: []string{"api", "repos/{owner}/{repo}", "--jq", ".name"}, Env: map[string]string{"GH_REPO": "example/app"}})
	if owner.Stdout != "app\n" {
		t.Fatalf("placeholders from GH_REPO: %+v", owner)
	}
}

// gh api --paginate follows the Link header and joins the pages of an
// array into one array; --slurp wraps the pages; --jq filters each page;
// a GraphQL query is followed by its endCursor.
func TestGHAPIPaginates(t *testing.T) {
	s, _ := build(t, fixture)
	all := decodeList(t, []byte(mustGH(t, s, "api", "--paginate", "repos/example/app/issues?state=all&per_page=1")))
	if len(all) != 4 {
		t.Fatalf("the four issues and pull requests, a page each, joined: %d", len(all))
	}
	pages := decodeList(t, []byte(mustGH(t, s, "api", "--paginate", "--slurp", "repos/example/app/issues?state=all&per_page=3")))
	if len(pages) != 2 || len(pages[0].([]any)) != 3 || len(pages[1].([]any)) != 1 {
		t.Fatalf("--slurp: %v", pages)
	}
	if out := mustGH(t, s, "api", "--paginate", "repos/example/app/issues?state=all&per_page=1", "--jq", ".[].number"); out != "4\n3\n2\n1\n" {
		t.Fatalf("--paginate with --jq: %q", out)
	}
	if one := decodeList(t, []byte(mustGH(t, s, "api", "repos/example/app/issues?state=all&per_page=1"))); len(one) != 1 {
		t.Fatalf("without --paginate: %d", len(one))
	}
	query := `query($endCursor:String){repository(owner:"example",name:"app"){issues(first:1,after:$endCursor){nodes{number} pageInfo{hasNextPage endCursor}}}}`
	dec := json.NewDecoder(strings.NewReader(mustGH(t, s, "api", "graphql", "--paginate", "-f", "query="+query)))
	var numbers []float64
	for dec.More() {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, at(t, doc, "data", "repository", "issues", "nodes", 0, "number").(float64))
	}
	if len(numbers) != 3 || numbers[0] != 1 || numbers[2] != 3 {
		t.Fatalf("GraphQL pages: %v", numbers)
	}
}

// gh api sends fields as a JSON body or, for GraphQL, as variables, and
// says a GraphQL error in gh's words.
func TestGHAPIWritesAndGraphQL(t *testing.T) {
	s, _ := build(t, fixture)
	created := decode(t, []byte(mustGH(t, s, "api", "-X", "POST", "repos/example/app/issues/1/comments", "-f", "body=hello")))
	if at(t, created, "body") != "hello" || at(t, created, "user", "login") != DefaultViewer {
		t.Fatalf("a posted comment: %v", created)
	}
	if out := mustGH(t, s, "api", "repos/example/app/issues/1/comments", "--jq", ".[0].body"); out != "hello\n" {
		t.Fatalf("the comment read back: %q", out)
	}
	if out := mustGH(t, s, "api", "graphql", "-f", "query={viewer{login}}"); out != `{"data":{"viewer":{"login":"desk"}}}` {
		t.Fatalf("a GraphQL answer: %q", out)
	}
	typed := mustGH(t, s, "api", "graphql", "-f", `query=query($n:Int!){repository(owner:"example",name:"app"){issue(number:$n){title}}}`, "-F", "n=2", "--jq", ".data.repository.issue.title")
	if typed != "Second\n" {
		t.Fatalf("a typed variable: %q", typed)
	}
	notFound := gh(s, "test", "api", "graphql", "-f", `query={repository(owner:"example",name:"app"){issue(number:99){title}}}`)
	if notFound.Exit != 1 || notFound.Stderr != "gh: Could not resolve to an Issue with the number of 99.\n" ||
		at(t, decode(t, []byte(notFound.Stdout)), "errors", 0, "type") != "NOT_FOUND" || at(t, decode(t, []byte(notFound.Stdout)), "data", "repository", "issue") != nil {
		t.Fatalf("a GraphQL NOT_FOUND: %+v", notFound)
	}
	mutation := `mutation($s:ID!,$b:String!){addComment(input:{subjectId:$s,body:$b}){commentEdge{node{url}}}}`
	id := strings.TrimSpace(mustGH(t, s, "api", "graphql", "-f", `query={repository(owner:"example",name:"app"){issue(number:1){id}}}`, "--jq", ".data.repository.issue.id"))
	link := mustGH(t, s, "api", "graphql", "-f", "query="+mutation, "-f", "s="+id, "-f", "b=second", "--jq", ".data.addComment.commentEdge.node.url")
	if !regexp.MustCompile(`^https://github\.com/example/app/issues/1#issuecomment-\d+\n$`).MatchString(link) {
		t.Fatalf("a mutation's answer: %q", link)
	}
}

// What the model does not have is never answered with a guess: an unknown
// REST route or GraphQL field is recorded as unsupported, and a command
// or flag the translator does not serve is not handled at all.
func TestTheModelsGapsFailLoudly(t *testing.T) {
	s, _ := build(t, fixture)
	route := gh(s, "test", "api", "repos/example/app/dependabot/alerts")
	if route.Exit != 1 || route.Stderr != "gh: Not Found (HTTP 404)\n" {
		t.Fatalf("an unknown route: %+v", route)
	}
	field := gh(s, "test", "api", "graphql", "-f", "query={viewer{bio}}")
	if field.Exit != 1 || !strings.Contains(field.Stderr, "Field 'bio' doesn't exist on type 'User'") {
		t.Fatalf("an unknown field: %+v", field)
	}
	un := s.Unsupported()
	if len(un) != 2 || !strings.Contains(un[0].Unsupported, "GET /repos/example/app/dependabot/alerts") || !strings.Contains(un[1].Unsupported, "Field 'bio'") {
		t.Fatalf("unsupported calls: %+v", un)
	}
	for _, args := range [][]string{
		{"pr", "frob", "4"},
		{"api", "--verbose", "repos/example/app"},
		{"api", "repos/example/app", "--template", "{{.name}}"},
		{"pr", "view", "4", "--repo", "example/app", "--web"},
		{"codespace", "list"},
		{"run", "list", "--repo", "example/app"},
	} {
		if res := gh(s, "test", args...); res.Handled || res.Reason == "" {
			t.Errorf("gh %s is answered: %+v", strings.Join(args, " "), res)
		}
	}
	if token := mustGH(t, s, "auth", "token", "--hostname", "github.com"); token != Token+"\n" {
		t.Fatalf("gh auth token: %q", token)
	}
}

// gh pr view and gh pr list export the fields of --json as gh does.
func TestGHPullJSON(t *testing.T) {
	s, _ := build(t, fixture)
	fields := "number,state,title,headRefName,isDraft,author,closingIssuesReferences,statusCheckRollup,reviews,mergeCommit,reviewDecision,mergedAt,labels,mergeable,mergeStateStatus"
	pr := decode(t, []byte(mustGH(t, s, "pr", "view", "4", "--repo", "example/app", "--json", fields)))
	want := map[string]any{"number": float64(4), "state": "OPEN", "title": "Fix the first", "headRefName": "fix-1", "isDraft": false,
		"mergeCommit": nil, "reviewDecision": "APPROVED", "mergedAt": nil, "mergeable": "MERGEABLE", "mergeStateStatus": "UNSTABLE"}
	for k, v := range want {
		if pr[k] != v {
			t.Errorf("%s: %v, want %v", k, pr[k], v)
		}
	}
	if len(pr) != len(strings.Split(fields, ",")) {
		t.Errorf("exactly the asked fields: %v", pr)
	}
	if at(t, pr, "author", "login") != "desk" || at(t, pr, "author", "is_bot") != false || at(t, pr, "author", "name") != "" {
		t.Errorf("author: %v", pr["author"])
	}
	if at(t, pr, "closingIssuesReferences", 0, "number") != float64(1) || at(t, pr, "closingIssuesReferences", 0, "repository", "owner", "login") != "example" {
		t.Errorf("closingIssuesReferences: %v", pr["closingIssuesReferences"])
	}
	check := at(t, pr, "statusCheckRollup", 0)
	if at(t, check, "__typename") != "CheckRun" || at(t, check, "name") != "build" || at(t, check, "status") != "IN_PROGRESS" ||
		at(t, check, "conclusion") != "" || at(t, check, "workflowName") != "CI" || at(t, check, "completedAt") != zeroTime {
		t.Errorf("statusCheckRollup: %v", check)
	}
	if at(t, pr, "reviews", 0, "state") != "APPROVED" || at(t, pr, "reviews", 0, "author", "login") != "reviewer" {
		t.Errorf("reviews: %v", pr["reviews"])
	}
	if labels, ok := pr["labels"].([]any); !ok || len(labels) != 0 {
		t.Errorf("labels: %v", pr["labels"])
	}

	if out := mustGH(t, s, "pr", "list", "--repo", "example/app", "--json", "number,title"); out != `[{"number":4,"title":"Fix the first"}]`+"\n" {
		t.Fatalf("pr list: %q", out)
	}
	if out := mustGH(t, s, "pr", "list", "--repo", "example/app", "--state", "merged", "--json", "number"); out != "[]\n" {
		t.Fatalf("pr list of no pull request: %q", out)
	}
	if out := mustGH(t, s, "pr", "list", "--repo", "example/app", "--search", "draft:false 1", "--json", "number", "--jq", ".[].number"); out != "4\n" {
		t.Fatalf("pr list --search: %q", out)
	}
	if out := mustGH(t, s, "pr", "list", "--repo", "example/app", "--head", "fix-1", "--state", "all", "--json", "url,state", "-q", ".[0].url"); out != "https://github.com/example/app/pull/4\n" {
		t.Fatalf("pr list --head: %q", out)
	}
	if out := mustGH(t, s, "pr", "checks", "4", "--repo", "example/app", "--json", "name,bucket"); out != `[{"bucket":"pending","name":"build"}]`+"\n" {
		t.Fatalf("pr checks --json: %q", out)
	}
	if res := gh(s, "test", "pr", "checks", "4", "--repo", "example/app"); res.Exit != 8 || !strings.HasPrefix(res.Stdout, "CI/build\tpending\t") {
		t.Fatalf("pr checks of a pending check exits 8: %+v", res)
	}

	missing := gh(s, "test", "pr", "view", "99", "--repo", "example/app", "--json", "state")
	if missing.Exit != 1 || missing.Stderr != "GraphQL: Could not resolve to a PullRequest with the number of 99. (repository.pullRequest)\n" {
		t.Fatalf("pr view of no pull request: %+v", missing)
	}
	unknown := gh(s, "test", "pr", "view", "4", "--repo", "example/app", "--json", "nope")
	if unknown.Exit != 1 || !strings.HasPrefix(unknown.Stderr, `Unknown JSON field: "nope"`) {
		t.Fatalf("an unknown --json field: %+v", unknown)
	}
	repo := mustGH(t, s, "repo", "view", "example/app", "--json", "defaultBranchRef,nameWithOwner")
	if repo != `{"defaultBranchRef":{"name":"main"},"nameWithOwner":"example/app"}`+"\n" {
		t.Fatalf("repo view: %q", repo)
	}
}

// The pull request commands change the model as GitHub would and say so
// in gh's words: create, review, merge (with the head it expects), close,
// comment and edit.
func TestGHPullCommands(t *testing.T) {
	s, _ := build(t, fixture)
	if err := s.Model(func(m *Model) error {
		r, _ := m.Repo("example/app")
		r.Branches["feature"] = fakeSHA("feature")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	create := []string{"pr", "create", "--repo", "example/app", "--head", "feature", "--title", "A feature", "--body", "Fixes #2", "--label", "bug"}
	if out := mustGH(t, s, create...); out != "https://github.com/example/app/pull/5\n" {
		t.Fatalf("pr create: %q", out)
	}
	if again := gh(s, "test", create...); again.Exit != 1 || !strings.Contains(again.Stderr, `a pull request for branch "feature" into branch "main" already exists`) {
		t.Fatalf("a second pr create of the branch: %+v", again)
	}
	if out := mustGH(t, s, "pr", "view", "feature", "--repo", "example/app", "--json", "number,labels", "--jq", "[.number, .labels[0].name]"); out != `[5,"bug"]`+"\n" {
		t.Fatalf("pr view by branch: %q", out)
	}

	own := gh(s, "test", "pr", "review", "4", "--repo", "example/app", "--approve")
	if own.Exit != 1 || !strings.Contains(own.Stderr, "Can not approve your own pull request") {
		t.Fatalf("approving one's own pull request: %+v", own)
	}
	if out := mustGH(t, s, "pr", "comment", "4", "--repo", "example/app", "--body", "ping"); !regexp.MustCompile(`^https://github\.com/example/app/pull/4#issuecomment-\d+\n$`).MatchString(out) {
		t.Fatalf("pr comment: %q", out)
	}
	if out := mustGH(t, s, "pr", "edit", "4", "--repo", "example/app", "--title", "Fix it", "--add-label", "bug"); out != "https://github.com/example/app/pull/4\n" {
		t.Fatalf("pr edit: %q", out)
	}

	stale := gh(s, "test", "pr", "merge", "4", "--repo", "example/app", "--squash", "--match-head-commit", "0000000000000000000000000000000000000000")
	if stale.Exit != 1 || stale.Stderr != "GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)\n" {
		t.Fatalf("a merge of a head that moved: %+v", stale)
	}
	if res := gh(s, "test", "pr", "merge", "4", "--repo", "example/app"); res.Exit != 1 || !strings.Contains(res.Stderr, "--merge, --rebase, or --squash required") {
		t.Fatalf("a merge without a method: %+v", res)
	}
	if res := gh(s, "test", "pr", "merge", "4", "--repo", "example/app", "--squash"); res.Exit != 0 || res.Stdout != "" || res.Stderr != "" {
		t.Fatalf("pr merge: %+v", res)
	}
	if err := s.Check(&Expect{Pull: &PullExpect{Repo: "example/app", Number: 4, State: "merged", Labels: []string{"bug"}, Comments: &Range{Equals: ptr(1)}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 1, State: "closed", Pulls: &Range{Equals: ptr(1)}, OpenPulls: &Range{Equals: ptr(0)}}}); err != nil {
		t.Fatalf("the merge closes the issue its body names: %v", err)
	}
	if out := mustGH(t, s, "pr", "view", "4", "--repo", "example/app", "--json", "state,title,mergedAt", "--jq", "[.state, .title, (.mergedAt != null)]"); out != `["MERGED","Fix it",true]`+"\n" {
		t.Fatalf("the merged pull request: %q", out)
	}

	closed := gh(s, "test", "pr", "close", "5", "--repo", "example/app", "--comment", "not now")
	if closed.Exit != 0 || closed.Stderr != "✓ Closed pull request example/app#5 (A feature)\n" {
		t.Fatalf("pr close: %+v", closed)
	}
	if again := gh(s, "test", "pr", "close", "5", "--repo", "example/app"); again.Exit != 0 || again.Stderr != "! Pull request example/app#5 (A feature) is already closed\n" {
		t.Fatalf("pr close of a closed pull request: %+v", again)
	}
	if merged := gh(s, "test", "pr", "close", "4", "--repo", "example/app"); merged.Exit != 1 || !strings.Contains(merged.Stderr, "can't be closed because it was already merged") {
		t.Fatalf("pr close of a merged pull request: %+v", merged)
	}
}

// A protected base holds a merge as gh says it, and --admin lifts it.
func TestGHPullMergeAgainstProtection(t *testing.T) {
	s, _ := build(t, `
repositories:
  - name: example/app
    protection: [{branch: main, required_reviews: 1, required_checks: [build]}]
    pulls:
      - {number: 1, title: Guarded, head: guarded, author: worker}
`)
	blocked := gh(s, "test", "pr", "merge", "1", "--repo", "example/app", "--squash")
	if blocked.Exit != 1 || !strings.Contains(blocked.Stderr, "X Pull request example/app#1 is not mergeable: the base branch policy prohibits the merge.") ||
		!strings.Contains(blocked.Stderr, "add the `--admin` flag") {
		t.Fatalf("a merge the base's protection holds: %+v", blocked)
	}
	if out := mustGH(t, s, "pr", "view", "1", "--repo", "example/app", "--json", "reviewDecision,mergeStateStatus"); out != `{"mergeStateStatus":"BLOCKED","reviewDecision":"REVIEW_REQUIRED"}`+"\n" {
		t.Fatalf("the guarded pull request: %q", out)
	}
	for _, st := range []*Step{
		{Review: &ReviewStep{Repo: "example/app", Pull: 1, State: "approved"}},
		{Check: &CheckStep{Repo: "example/app", Pull: 1, Name: "build", State: "red"}},
	} {
		if err := s.Run(st); err != nil {
			t.Fatal(err)
		}
	}
	if out := mustGH(t, s, "pr", "view", "1", "--repo", "example/app", "--json", "reviewDecision,mergeStateStatus"); out != `{"mergeStateStatus":"BLOCKED","reviewDecision":"APPROVED"}`+"\n" {
		t.Fatalf("approved, its required check red: %q", out)
	}
	if err := s.Run(&Step{Check: &CheckStep{Repo: "example/app", Pull: 1, Name: "build", State: "green"}}); err != nil {
		t.Fatal(err)
	}
	if out := mustGH(t, s, "pr", "view", "1", "--repo", "example/app", "--json", "mergeStateStatus", "--jq", ".mergeStateStatus"); out != "CLEAN\n" {
		t.Fatalf("approved and green: %q", out)
	}
	s2, _ := build(t, `
repositories:
  - name: example/app
    protection: [{branch: main, required_reviews: 1}]
    pulls:
      - {number: 1, title: Guarded, head: guarded, author: worker}
`)
	if res := gh(s2, "test", "pr", "merge", "1", "--repo", "example/app", "--squash", "--admin"); res.Exit != 0 {
		t.Fatalf("an administrator's merge: %+v", res)
	}
	if err := s2.Check(&Expect{Pull: &PullExpect{Repo: "example/app", Number: 1, State: "merged"}}); err != nil {
		t.Fatal(err)
	}
}

// gh issue reads and changes issues as gh does.
func TestGHIssue(t *testing.T) {
	s, _ := build(t, fixture)
	issue := decode(t, []byte(mustGH(t, s, "issue", "view", "2", "--repo", "example/app", "--json", "number,state,title,labels,parent,subIssuesSummary,closedAt,url")))
	if issue["number"] != float64(2) || issue["state"] != "OPEN" || issue["closedAt"] != nil || at(t, issue, "parent", "number") != float64(1) ||
		at(t, issue, "subIssuesSummary", "total") != float64(0) || issue["url"] != "https://github.com/example/app/issues/2" {
		t.Fatalf("issue view: %v", issue)
	}
	numbers := mustGH(t, s, "issue", "list", "--repo", "example/app", "--json", "number", "--jq", "[.[].number] | sort")
	if numbers != "[1,2]\n" {
		t.Fatalf("issue list of the open issues: %q", numbers)
	}
	if out := mustGH(t, s, "issue", "list", "--repo", "example/app", "--state", "closed", "--json", "number,stateReason"); out != `[{"number":3,"stateReason":"COMPLETED"}]`+"\n" {
		t.Fatalf("issue list --state closed: %q", out)
	}
	if out := mustGH(t, s, "issue", "list", "--repo", "example/app", "--label", "bug", "--json", "number", "--jq", ".[].number"); out != "1\n" {
		t.Fatalf("issue list --label: %q", out)
	}
	created := mustGH(t, s, "issue", "create", "--repo", "example/app", "--title", "New", "--body", "text", "--label", "bug")
	if created != "https://github.com/example/app/issues/5\n" {
		t.Fatalf("issue create: %q", created)
	}
	closed := gh(s, "test", "issue", "close", "https://github.com/example/app/issues/5", "--reason", "not planned", "--comment", "no")
	if closed.Exit != 0 || closed.Stderr != "✓ Closed issue example/app#5 (New)\n" {
		t.Fatalf("issue close: %+v", closed)
	}
	if out := mustGH(t, s, "issue", "view", "5", "--repo", "example/app", "--json", "state,stateReason,comments", "--jq", "[.state, .stateReason, .comments[0].body]"); out != `["CLOSED","NOT_PLANNED","no"]`+"\n" {
		t.Fatalf("the closed issue: %q", out)
	}
	if again := gh(s, "test", "issue", "close", "5", "--repo", "example/app"); again.Exit != 0 || again.Stderr != "! Issue example/app#5 (New) is already closed\n" {
		t.Fatalf("issue close of a closed issue: %+v", again)
	}
	if out := mustGH(t, s, "issue", "edit", "1", "--repo", "example/app", "--remove-label", "bug", "--title", "Renamed"); out != "https://github.com/example/app/issues/1\n" {
		t.Fatalf("issue edit: %q", out)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 1, Labels: []string{}, Board: "Ready"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if res := gh(s, "test", "label", "create", "triage", "--repo", "example/app", "--color", "D93F0B", "--description", "Human triage required", "--force"); res.Exit != 0 || res.Stdout != "" {
			t.Fatalf("label create --force, run %d: %+v", i, res)
		}
	}
	if out := mustGH(t, s, "label", "list", "--repo", "example/app", "--json", "name,color", "--jq", `.[] | select(.name == "triage") | .color`); out != "D93F0B\n" {
		t.Fatalf("the created label: %q", out)
	}
	if dup := gh(s, "test", "label", "create", "triage", "--repo", "example/app"); dup.Exit != 1 || !strings.Contains(dup.Stderr, "already exists; use `--force`") {
		t.Fatalf("label create of an existing label: %+v", dup)
	}
}

// Releases and workflow runs are served through REST as gh reads them.
func TestGHReleasesAndRuns(t *testing.T) {
	s, _ := build(t, fixture+`    releases:
      - {tag: v1.0.0, name: One, assets: [{name: notes.txt, content: "first\n"}]}
      - {tag: v1.1.0-rc.1, prerelease: true}
`)
	list := decodeList(t, []byte(mustGH(t, s, "release", "list", "--repo", "example/app", "--json", "tagName,isLatest,isPrerelease")))
	if len(list) != 2 || at(t, list[0], "tagName") != "v1.1.0-rc.1" || at(t, list[0], "isPrerelease") != true || at(t, list[1], "isLatest") != true {
		t.Fatalf("release list: %v", list)
	}
	view := decode(t, []byte(mustGH(t, s, "release", "view", "--repo", "example/app", "--json", "tagName,name,isDraft,assets")))
	if view["tagName"] != "v1.0.0" || view["name"] != "One" || at(t, view, "assets", 0, "name") != "notes.txt" || at(t, view, "assets", 0, "size") != float64(6) {
		t.Fatalf("release view of the latest: %v", view)
	}
	if out := mustGH(t, s, "api", "repos/example/app/releases/tags/v1.1.0-rc.1", "--jq", ".prerelease"); out != "true\n" {
		t.Fatalf("a release by tag: %q", out)
	}
	if missing := gh(s, "test", "release", "view", "v9", "--repo", "example/app", "--json", "name"); missing.Exit != 1 || missing.Stderr != "release not found\n" {
		t.Fatalf("release view of no release: %+v", missing)
	}
	dir := t.TempDir()
	res := s.GH(t.Context(), GHCall{Args: []string{"release", "download", "v1.0.0", "--repo", "example/app", "--pattern", "*.txt"}, Dir: dir})
	if res.Exit != 0 {
		t.Fatalf("release download: %+v", res)
	}
	if created := mustGH(t, s, "release", "create", "v2.0.0", "--repo", "example/app", "--title", "Two", "--notes", "big"); created != "https://github.com/example/app/releases/tag/v2.0.0\n" {
		t.Fatalf("release create: %q", created)
	}
	if out := mustGH(t, s, "api", "repos/example/app/releases/latest", "--jq", ".tag_name"); out != "v2.0.0\n" {
		t.Fatalf("the latest release: %q", out)
	}

	if err := s.Run(&Step{Check: &CheckStep{Repo: "example/app", Pull: 4, Name: "build", State: "red"}}); err != nil {
		t.Fatal(err)
	}
	runs := decodeList(t, []byte(mustGH(t, s, "run", "list", "--repo", "example/app", "--json", "databaseId,status,conclusion,workflowName,headSha,attempt")))
	if len(runs) != 1 || at(t, runs[0], "status") != "completed" || at(t, runs[0], "conclusion") != "failure" || at(t, runs[0], "workflowName") != "CI" || at(t, runs[0], "attempt") != float64(1) {
		t.Fatalf("run list: %v", runs)
	}
	id := strings.TrimSpace(mustGH(t, s, "run", "list", "--repo", "example/app", "--json", "databaseId", "--jq", ".[0].databaseId"))
	if out := mustGH(t, s, "run", "rerun", id, "--repo", "example/app", "--failed"); out != "✓ Requested rerun (failed jobs) of run "+id+"\n" {
		t.Fatalf("run rerun: %q", out)
	}
	view2 := decode(t, []byte(mustGH(t, s, "run", "view", id, "--repo", "example/app", "--json", "status,conclusion,attempt,jobs")))
	if view2["status"] != "queued" || view2["conclusion"] != "" || view2["attempt"] != float64(2) || at(t, view2, "jobs", 0, "name") != "build" {
		t.Fatalf("the rerun run: %v", view2)
	}
	api := decode(t, []byte(mustGH(t, s, "api", "--paginate", "repos/example/app/actions/runs?per_page=100")))
	if at(t, api, "workflow_runs", 0, "run_attempt") != float64(2) || at(t, api, "workflow_runs", 0, "conclusion") != nil || at(t, api, "total_count") != float64(1) {
		t.Fatalf("the runs through REST: %v", api)
	}
	if !bytes.Contains([]byte(mustGH(t, s, "api", "repos/example/app/commits/fix-1/check-runs", "--jq", ".check_runs[] | {name, status, conclusion}")), []byte(`"status":"queued"`)) {
		t.Fatal("the check run of the rerun is not queued")
	}
}
