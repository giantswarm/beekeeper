//go:build scenario

package fakegithub

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The queries beekeeper's board client sends (internal/board/client.go and
// cmd/unmergedhold.go), as it writes them.
const (
	coreIssueFields = `fragment I on Issue{number url title state createdAt updatedAt repository{nameWithOwner}
labels(first:20){nodes{name} pageInfo{hasNextPage endCursor}} assignees(first:10){nodes{login}} ...on Issue{blockedBy(first:100){totalCount nodes{closedAt number state repository{nameWithOwner}}}} subIssuesSummary{total completed}
parent{number repository{nameWithOwner}}}`
	coreBoardFields = `status:fieldValueByName(name:"Status"){...on ProjectV2ItemFieldSingleSelectValue{name updatedAt}}
kind:fieldValueByName(name:"Kind"){...on ProjectV2ItemFieldSingleSelectValue{name}}`
	coreFieldsQuery = `query($o:String!,$n:Int!){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){id
fields(first:50){nodes{...on ProjectV2SingleSelectField{id name options{id name}} ...on ProjectV2Field{id name dataType}}}}}}}`
	coreItemsQuery = `query($o:String!,$n:Int!,$q:String!,$c:String){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){
items(first:80,after:$c,query:$q){pageInfo{hasNextPage endCursor} nodes{` + coreBoardFields + `
content{...I}}}}}}}` + coreIssueFields
	corePullsQuery = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){pullRequests(states:OPEN,first:100,after:$after){nodes{number url state createdAt isDraft headRefName title body closingIssuesReferences(first:100){nodes{number repository{nameWithOwner}} pageInfo{hasNextPage}}} pageInfo{hasNextPage endCursor}}}}`
)

// The model answers the board's queries as beekeeper sends them through
// gh api graphql: the fields with the Status options, the open items with
// their Status and issue, and the open pull requests with what they
// close.
func TestCoreBoardQueries(t *testing.T) {
	s, _ := build(t, fixture)
	fields := decode(t, []byte(mustGH(t, s, "api", "graphql", "-f", "query="+coreFieldsQuery, "-F", "n=1", "-f", "o=example")))
	var options []string
	for _, f := range at(t, fields, "data", "repositoryOwner", "projectV2", "fields", "nodes").([]any) {
		if at(t, f, "name") == "Status" {
			for _, o := range at(t, f, "options").([]any) {
				options = append(options, at(t, o, "name").(string))
			}
		}
	}
	if !strings.Contains(strings.Join(options, ","), "Ready") {
		t.Fatalf("the Status field's options %v lack Ready: %v", options, fields)
	}

	items := decode(t, []byte(mustGH(t, s, "api", "graphql", "-f", "query="+coreItemsQuery, "-F", "n=1", "-f", "o=example", "-f", "q=is:open")))
	nodes := at(t, items, "data", "repositoryOwner", "projectV2", "items", "nodes").([]any)
	if len(nodes) != 2 {
		t.Fatalf("open items: %d, want the two open issues on the board: %v", len(nodes), items)
	}
	first := nodes[0]
	if at(t, first, "status", "name") != "Ready" || at(t, first, "kind") != nil ||
		at(t, first, "content", "number") != float64(1) || at(t, first, "content", "url") != "https://github.com/example/app/issues/1" ||
		at(t, first, "content", "repository", "nameWithOwner") != "example/app" || at(t, first, "content", "labels", "nodes", 0, "name") != "bug" ||
		at(t, first, "content", "subIssuesSummary", "total") != float64(1) || at(t, first, "content", "parent") != nil ||
		at(t, first, "content", "blockedBy", "totalCount") != float64(0) {
		t.Fatalf("the first item: %v", first)
	}
	if at(t, nodes[1], "content", "parent", "number") != float64(1) || at(t, nodes[1], "status", "name") != "Todo" {
		t.Fatalf("the second item: %v", nodes[1])
	}
	if at(t, items, "data", "repositoryOwner", "projectV2", "items", "pageInfo", "hasNextPage") != false {
		t.Fatalf("pageInfo: %v", items)
	}

	ready := decode(t, []byte(mustGH(t, s, "api", "graphql", "-f", "query="+coreItemsQuery, "-F", "n=1", "-f", "o=example", "-f", `q=is:open status:"Ready","Done"`)))
	if n := len(at(t, ready, "data", "repositoryOwner", "projectV2", "items", "nodes").([]any)); n != 1 {
		t.Fatalf("items with Status Ready or Done: %d, want 1: %v", n, ready)
	}

	pulls := decode(t, []byte(mustGH(t, s, "api", "graphql", "-f", "query="+corePullsQuery, "-f", "owner=example", "-f", "name=app")))
	open := at(t, pulls, "data", "repository", "pullRequests", "nodes").([]any)
	if len(open) != 1 || at(t, open[0], "number") != float64(4) || at(t, open[0], "isDraft") != false ||
		at(t, open[0], "closingIssuesReferences", "nodes", 0, "number") != float64(1) ||
		at(t, open[0], "closingIssuesReferences", "nodes", 0, "repository", "nameWithOwner") != "example/app" {
		t.Fatalf("open pull requests: %v", pulls)
	}
	if un := s.Unsupported(); len(un) != 0 {
		t.Fatalf("the model lacks something the board asks for: %+v", un)
	}
}

// A query costs what GitHub documents: the requests needed for every
// connection, each assumed full, over 100 and rounded. The board's page
// of 80 items with three nested connections costs 2 points, a page of 100
// costs 3, as the board client's comment says.
func TestGraphQLCostIsGitHubsFormula(t *testing.T) {
	for first, want := range map[string]int{"80": 2, "100": 3} {
		op, err := prepareGraphQL(strings.Replace(coreItemsQuery, "first:80", "first:"+first, 1), "", map[string]any{"o": "example", "n": 1, "q": "is:open"})
		if err != nil {
			t.Fatal(err)
		}
		if got := queryCost(op); got != want {
			t.Errorf("a page of %s items costs %d, want %d", first, got, want)
		}
	}
	s, _ := build(t, fixture+"costs: [{operation: repositoryOwner, cost: 7}]\n")
	mustGH(t, s, "api", "graphql", "-f", "query="+coreFieldsQuery, "-F", "n=1", "-f", "o=example")
	out := graphql(t, s, "loop", `{ viewer { login } rateLimit { limit remaining used cost } }`, nil)
	if spent(s, "test", bucketGraphQL) != 7 || spent(s, "loop", bucketGraphQL) != 1 {
		t.Fatalf("spent: %+v, want the declared 7 for repositoryOwner and 1 for the rate limit query", s.Spent())
	}
	if at(t, out, "data", "rateLimit", "remaining") != float64(5000-8) || at(t, out, "data", "rateLimit", "cost") != float64(1) ||
		at(t, out, "data", "viewer", "login") != DefaultViewer {
		t.Fatalf("rateLimit after 8 points: %v", out)
	}
}

// A conditional read whose ETag still matches is 304 Not Modified and
// costs nothing; a changed resource is read again and paid for.
func TestConditionalReadCostsNothing(t *testing.T) {
	s, _ := build(t, fixture)
	first := request(s, "loop", http.MethodGet, "/repos/example/app", nil, "")
	etag := first.Header.Get("ETag")
	if first.Status != http.StatusOK || etag == "" || first.Header.Get("X-RateLimit-Remaining") != "4999" || first.Header.Get("X-RateLimit-Resource") != "core" {
		t.Fatalf("first read: %d %v", first.Status, first.Header)
	}
	again := request(s, "loop", http.MethodGet, "/repos/example/app", map[string]string{"If-None-Match": etag}, "")
	if again.Status != http.StatusNotModified || len(again.Body) != 0 || again.Header.Get("X-RateLimit-Remaining") != "4999" {
		t.Fatalf("conditional read: %d %q %v", again.Status, again.Body, again.Header)
	}
	if got := spent(s, "loop", bucketCore); got != 1 {
		t.Fatalf("core spent %d, want 1: the 304 costs nothing", got)
	}
	calls := s.Calls()
	if len(calls) != 2 || calls[1].Status != http.StatusNotModified || !calls[1].Conditional || calls[1].Cost != 0 || calls[0].Cost != 1 {
		t.Fatalf("call log: %+v", calls)
	}
	if err := s.Model(func(m *Model) error {
		r, _ := m.Repo("example/app")
		r.Description = "changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed := request(s, "loop", http.MethodGet, "/repos/example/app", map[string]string{"If-None-Match": etag}, "")
	if changed.Status != http.StatusOK || changed.Header.Get("ETag") == etag || spent(s, "loop", bucketCore) != 2 {
		t.Fatalf("read of the changed repository: %d etag %q spent %d", changed.Status, changed.Header.Get("ETag"), spent(s, "loop", bucketCore))
	}
}

// The buckets hold their points per window on the scenario's clock: a
// spent bucket refuses with GitHub's words and headers, the hour's reset
// refills it, /rate_limit is free, and spend is kept per caller.
func TestBudgetsRefuseResetAndRecordSpend(t *testing.T) {
	s, c := build(t, fixture+`
budgets:
  core: {limit: 3, remaining: 2}
  graphql: {remaining: 1}
`)
	for i, caller := range []string{"loop", "worker"} {
		if resp := request(s, caller, http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK {
			t.Fatalf("read %d: %d", i, resp.Status)
		}
	}
	refused := request(s, "loop", http.MethodGet, "/repos/example/app", nil, "")
	if refused.Status != http.StatusForbidden || !strings.Contains(string(refused.Body), "API rate limit exceeded") ||
		refused.Header.Get("X-RateLimit-Remaining") != "0" || refused.Header.Get("X-RateLimit-Limit") != "3" {
		t.Fatalf("spent core bucket: %d %s %v", refused.Status, refused.Body, refused.Header)
	}
	reset, err := strconv.ParseInt(refused.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || time.Unix(reset, 0).Sub(c.Now()) != time.Hour {
		t.Fatalf("X-RateLimit-Reset %q, want an hour after the clock", refused.Header.Get("X-RateLimit-Reset"))
	}
	limits := decode(t, request(s, "loop", http.MethodGet, "/rate_limit", nil, "").Body)
	if at(t, limits, "resources", "core", "remaining") != float64(0) || at(t, limits, "resources", "search", "limit") != float64(30) ||
		at(t, limits, "resources", "code_search", "limit") != float64(10) || at(t, limits, "resources", "graphql", "remaining") != float64(1) {
		t.Fatalf("/rate_limit: %v", limits)
	}
	if spent(s, "loop", bucketCore) != 1 || spent(s, "worker", bucketCore) != 1 {
		t.Fatalf("spend per caller: %+v", s.Spent())
	}

	ok := graphql(t, s, "loop", `{ viewer { login } }`, nil)
	if at(t, ok, "data", "viewer", "login") != DefaultViewer {
		t.Fatalf("the last GraphQL point: %v", ok)
	}
	limited := graphql(t, s, "loop", `{ viewer { login } }`, nil)
	if at(t, limited, "errors", 0, "type") != "RATE_LIMITED" || limited["data"] != nil {
		t.Fatalf("spent GraphQL bucket: %v", limited)
	}

	c.Advance(time.Hour)
	if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK || resp.Header.Get("X-RateLimit-Remaining") != "2" {
		t.Fatalf("after the hourly reset: %d %v", resp.Status, resp.Header)
	}
	if again := graphql(t, s, "loop", `{ viewer { login } }`, nil); again["errors"] != nil {
		t.Fatalf("GraphQL after the reset: %v", again)
	}
	if state, _ := s.Bucket(bucketGraphQL); state.Remaining != 4999 || state.Limit != 5000 {
		t.Fatalf("the GraphQL bucket after its reset and one query: %+v", state)
	}
}

// The secondary rate limit refuses the request over its rate with 403 and
// Retry-After, whatever the buckets hold.
func TestSecondaryRateLimit(t *testing.T) {
	s, c := build(t, fixture+"budgets: {secondary: {max: 2, per: 1m, retry_after: 30s}}\n")
	for i := 0; i < 2; i++ {
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK {
			t.Fatalf("request %d: %d", i, resp.Status)
		}
	}
	resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, "")
	if resp.Status != http.StatusForbidden || resp.Header.Get("Retry-After") != "30" || !strings.Contains(string(resp.Body), "secondary rate limit") {
		t.Fatalf("the third request in a minute: %d %v %s", resp.Status, resp.Header, resp.Body)
	}
	if spent(s, "loop", bucketCore) != 2 {
		t.Fatalf("a refusal costs nothing: %+v", s.Spent())
	}
	c.Advance(time.Minute)
	if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK {
		t.Fatalf("after the window: %d", resp.Status)
	}
}

func arm(t *testing.T, s *Server, doc string) {
	t.Helper()
	g := parseGiven(t, "faults:\n  - "+strings.ReplaceAll(strings.TrimSpace(doc), "\n", "\n    ")+"\n")
	if err := s.Arm(g.Faults[0]); err != nil {
		t.Fatalf("arm: %v", err)
	}
}

// A fault answers the requests it matches: a status so many times or for
// a window of the clock, with Retry-After, as a secondary rate limit, or
// with no answer at all; it costs nothing and is named in the call log.
func TestFaults(t *testing.T) {
	t.Run("status for N requests", func(t *testing.T) {
		s, _ := build(t, fixture)
		arm(t, s, "name: flaky\nmatch: {path: repos/example/app}\nrespond: {status: 502, retry_after: 5s}\ntimes: 2")
		for i := 0; i < 2; i++ {
			resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, "")
			if resp.Status != http.StatusBadGateway || resp.Header.Get("Retry-After") != "5" || !strings.Contains(string(resp.Body), "Bad Gateway") {
				t.Fatalf("request %d: %d %v %s", i, resp.Status, resp.Header, resp.Body)
			}
		}
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK {
			t.Fatalf("after the fault's two requests: %d", resp.Status)
		}
		if other := request(s, "loop", http.MethodGet, "/repos/example/app/issues/1", nil, ""); other.Status != http.StatusOK {
			t.Fatalf("a path the fault does not match: %d", other.Status)
		}
		calls := s.Calls()
		if calls[0].Fault != "flaky" || calls[0].Cost != 0 || calls[2].Fault != "" || spent(s, "loop", bucketCore) != 2 {
			t.Fatalf("call log: %+v, spent %+v", calls, s.Spent())
		}
	})
	t.Run("status for a window of the clock", func(t *testing.T) {
		s, c := build(t, fixture)
		arm(t, s, "match: {method: GET}\nrespond: {status: 503}\nfor: 5m")
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusServiceUnavailable {
			t.Fatalf("inside the window: %d", resp.Status)
		}
		c.Advance(5 * time.Minute)
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK {
			t.Fatalf("after the window: %d", resp.Status)
		}
	})
	t.Run("matched on caller, conditional and operation", func(t *testing.T) {
		s, _ := build(t, fixture)
		arm(t, s, "name: for-worker\nmatch: {caller: worker, path: 'repos/.*'}\nrespond: {status: 404}")
		arm(t, s, "name: conditional\nmatch: {conditional: true}\nrespond: {status: 304}")
		arm(t, s, "name: board\nmatch: {path: graphql, operation: repositoryOwner}\nrespond: {secondary_rate_limit: true, retry_after: 1m}")
		if resp := request(s, "worker", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusNotFound {
			t.Fatalf("the worker's read: %d", resp.Status)
		}
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK {
			t.Fatalf("the loop's read: %d", resp.Status)
		}
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", map[string]string{"If-None-Match": `W/"anything"`}, ""); resp.Status != http.StatusNotModified {
			t.Fatalf("the loop's conditional read: %d", resp.Status)
		}
		if viewer := graphql(t, s, "loop", `{ viewer { login } }`, nil); viewer["errors"] != nil {
			t.Fatalf("an operation the fault does not match: %v", viewer)
		}
		res := gh(s, "loop", "api", "graphql", "-f", "query="+coreFieldsQuery, "-F", "n=1", "-f", "o=example")
		if res.Exit != 1 || !strings.Contains(res.Stderr, "secondary rate limit") || !strings.Contains(res.Stderr, "HTTP 403") {
			t.Fatalf("the board's query under a secondary rate limit: %+v", res)
		}
		last := s.Calls()[len(s.Calls())-1]
		if last.Fault != "board" || last.Operation != "repositoryOwner" || last.Via != viaGH || last.Status != http.StatusForbidden || spent(s, "loop", bucketGraphQL) != 1 {
			t.Fatalf("call log: %+v, spent %+v", last, s.Spent())
		}
	})
	t.Run("outage and delay", func(t *testing.T) {
		s, _ := build(t, fixture)
		var waited []time.Duration
		s.sleep = func(_ context.Context, d time.Duration) { waited = append(waited, d) }
		arm(t, s, "name: slow\nmatch: {path: repos/example/app}\nrespond: {delay: 8s}\ntimes: 1")
		arm(t, s, "name: down\nmatch: {path: repos/example/app/issues/1}\nrespond: {outage: true}")
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app", nil, ""); resp.Status != http.StatusOK || len(waited) != 1 || waited[0] != 8*time.Second {
			t.Fatalf("a slow answer is the real one, late: %d, waited %v", resp.Status, waited)
		}
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app/issues/1", nil, ""); !resp.Outage {
			t.Fatalf("an outage has no answer: %+v", resp)
		}
		res := gh(s, "loop", "api", "repos/example/app/issues/1")
		if res.Exit != 1 || !strings.Contains(res.Stderr, "error connecting to api.github.com") {
			t.Fatalf("gh during an outage: %+v", res)
		}
		if err := s.Clear("down"); err != nil {
			t.Fatal(err)
		}
		if resp := request(s, "loop", http.MethodGet, "/repos/example/app/issues/1", nil, ""); resp.Status != http.StatusOK {
			t.Fatalf("after the fault was cleared: %d", resp.Status)
		}
		if err := s.Clear("down"); err == nil {
			t.Fatal("clearing a fault that is not armed passed")
		}
		if err := s.Clear(clearAll); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("validation", func(t *testing.T) {
		for name, f := range map[string]Fault{
			"no answer":         {Match: FaultMatch{Path: "x"}},
			"two answers":       {Respond: FaultRespond{Status: 500, Outage: true}},
			"a bad pattern":     {Match: FaultMatch{Path: "("}, Respond: FaultRespond{Status: 500}},
			"a body alone":      {Respond: FaultRespond{Body: "{}", Outage: true}},
			"no HTTP status":    {Respond: FaultRespond{Status: 42}},
			"the reserved name": {Name: clearAll, Respond: FaultRespond{Status: 500}},
			"an unknown via":    {Match: FaultMatch{Via: "curl"}, Respond: FaultRespond{Status: 500}},
		} {
			if err := f.Validate(); err == nil {
				t.Errorf("a fault with %s is valid", name)
			}
		}
	})
}

// The server answers over HTTP on a loopback port. A path under
// /caller/<name> names who asks: the API base variable carries it, so
// direct callers are told apart, and links keep it. An outage drops the
// connection.
func TestHTTPNamesTheCaller(t *testing.T) {
	s, _ := build(t, fixture)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := s.Env("loop")[APIEnv]
	if base != s.URL()+"/caller/loop" || s.Env("loop")[APIURLEnv] != base || s.Env("loop")[CallerEnv] != "loop" {
		t.Fatalf("Env: %v", s.Env("loop"))
	}
	get := func(url string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(url) //nolint:gosec,noctx // the test's own loopback server
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	resp, body := get(base + "/repos/example/app")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"full_name":"example/app"`) || resp.Header.Get("ETag") == "" {
		t.Fatalf("GET over HTTP: %d %s", resp.StatusCode, body)
	}
	page, _ := get(base + "/repos/example/app/issues?state=all&per_page=1")
	if link := page.Header.Get("Link"); !strings.Contains(link, base+"/repos/example/app/issues?page=2&per_page=1&state=all>; rel=\"next\"") {
		t.Fatalf("Link %q does not lead back to the caller's base", link)
	}
	if enterprise, _ := get(s.URL() + "/api/v3/repos/example/app"); enterprise.StatusCode != http.StatusOK {
		t.Fatalf("an Enterprise path: %d", enterprise.StatusCode)
	}
	if spent(s, "loop", bucketCore) != 2 || spent(s, "", bucketCore) != 1 {
		t.Fatalf("spend per caller: %+v", s.Spent())
	}
	post, err := http.Post(base+"/graphql", "application/json", strings.NewReader(`{"query":"{ viewer { login } rateLimit { limit remaining used resetAt } }"}`)) //nolint:gosec,noctx // the test's own loopback server
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(post.Body)
	_ = post.Body.Close()
	if post.StatusCode != http.StatusOK || post.Header.Get("X-RateLimit-Resource") != bucketGraphQL || at(t, decode(t, raw), "data", "rateLimit", "remaining") != float64(4999) {
		t.Fatalf("GraphQL over HTTP: %d %v %s", post.StatusCode, post.Header, raw)
	}
	arm(t, s, "match: {path: repos/example/app}\nrespond: {outage: true}")
	if resp, err := http.Get(base + "/repos/example/app"); err == nil { //nolint:gosec,noctx // the test's own loopback server
		_ = resp.Body.Close()
		t.Fatalf("an outage answered %d", resp.StatusCode)
	}
}
