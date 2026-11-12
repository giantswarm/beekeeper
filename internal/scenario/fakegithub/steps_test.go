//go:build scenario

package fakegithub

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A github step changes the model, and a github expectation reads it: a
// check's result on a pull request's head, a review, a merge, a close, a
// comment and a bucket.
func TestStepsAndExpectations(t *testing.T) {
	s, _ := build(t, fixture)
	pull := func(e PullExpect) error {
		e.Repo, e.Number = "example/app", 4
		return s.Check(&Expect{Pull: &e})
	}
	if err := pull(PullExpect{State: "open", Checks: "pending", ReviewDecision: "approved", Draft: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := pull(PullExpect{Checks: "green"}); err == nil || !strings.Contains(err.Error(), "example/app#4: checks pending, want green") {
		t.Fatalf("a wrong expectation: %v", err)
	}
	for _, st := range []*Step{
		{Check: &CheckStep{Repo: "example/app", Pull: 4, Name: "build", State: "green"}},
		{Check: &CheckStep{Repo: "example/app", Head: "fix-1", Name: "lint", State: "red"}},
		{Review: &ReviewStep{Repo: "example/app", Pull: 4, State: "changes_requested", Author: "second", Body: "no"}},
		{Comment: &CommentStep{Repo: "example/app", Issue: 1, Author: "second", Body: "seen"}},
	} {
		if err := st.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := s.Run(st); err != nil {
			t.Fatal(err)
		}
	}
	if err := pull(PullExpect{Checks: "red", ReviewDecision: "changes_requested"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 1, State: "open", Labels: []string{"bug"}, Board: "ready",
		Comments: &Range{Equals: ptr(1)}, OpenPulls: &Range{Min: ptr(1), Max: ptr(1)}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 3, Board: "none"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(&Step{Merge: &MergeStep{Repo: "example/app", Pull: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := pull(PullExpect{State: "merged", MergedInto: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 1, State: "closed", OpenPulls: &Range{Equals: ptr(0)}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(&Step{Close: &CloseStep{Repo: "example/app", Issue: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 2, State: "closed"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(&Step{Close: &CloseStep{Repo: "example/app", Issue: 99}}); err == nil {
		t.Fatal("closing an issue the scenario does not have passed")
	}

	mustGH(t, s, "api", "repos/example/app")
	gh(s, "loop", "api", "graphql", "-f", "query={viewer{login}}")
	for _, e := range []CallsExpect{
		{Path: "repos/example/app", Method: "GET", Caller: "test", Status: ptr(200), Via: viaGH, Min: ptr(1), Max: ptr(1)},
		{Operation: "viewer", Caller: "loop", Min: ptr(1)},
		{Caller: "nobody-else", Max: ptr(0)},
		{Path: "repos/.*", Conditional: ptr(true), Max: ptr(0)},
	} {
		if err := s.Check(&Expect{Calls: &e}); err != nil {
			t.Errorf("%+v: %v", e, err)
		}
	}
	err := s.Check(&Expect{Calls: &CallsExpect{Path: "repos/example/app", Status: ptr(304), Min: ptr(1)}})
	if err == nil || !strings.Contains(err.Error(), "GET repos/example/app by test via gh: 200") {
		t.Fatalf("a failed calls expectation names what GitHub saw: %v", err)
	}
	if err := s.Run(&Step{Budget: map[string]GivenBudget{bucketGraphQL: {Remaining: ptr(10)}}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []BudgetExpect{
		{Bucket: bucketGraphQL, Caller: "loop", Spent: &Range{Equals: ptr(1)}},
		{Bucket: bucketGraphQL, Remaining: &Range{Equals: ptr(10)}},
		{Bucket: bucketCore, Spent: &Range{Min: ptr(1)}, Remaining: &Range{Max: ptr(4999)}},
		{Bucket: bucketSearch, Caller: "loop", Spent: &Range{Equals: ptr(0)}},
	} {
		if err := s.Check(&Expect{Budget: &e}); err != nil {
			t.Errorf("%+v: %v", e, err)
		}
	}
	if err := s.Check(&Expect{Budget: &BudgetExpect{Bucket: bucketGraphQL, Caller: "loop", Spent: &Range{Max: ptr(0)}}}); err == nil || !strings.Contains(err.Error(), "spent by loop: 1, at most 0") {
		t.Fatalf("a failed budget expectation: %v", err)
	}
}

// Steps, expectations and given.github refuse what they cannot mean:
// exactly one action or kind, known states, known buckets, and no unknown
// key, in a range too.
func TestValidation(t *testing.T) {
	for name, st := range map[string]*Step{
		"no action":            {},
		"two actions":          {Merge: &MergeStep{Repo: "o/r", Pull: 1}, Close: &CloseStep{Repo: "o/r", Pull: 1}},
		"a check of no head":   {Check: &CheckStep{Repo: "o/r", Name: "x", State: "green"}},
		"an unknown state":     {Check: &CheckStep{Repo: "o/r", Pull: 1, Name: "x", State: "blue"}},
		"a bad repository":     {Merge: &MergeStep{Repo: "nope", Pull: 1}},
		"a close of both":      {Close: &CloseStep{Repo: "o/r", Pull: 1, Issue: 2}},
		"an unknown bucket":    {Budget: map[string]GivenBudget{"gql": {}}},
		"a comment of nothing": {Comment: &CommentStep{Repo: "o/r", Issue: 1}},
	} {
		if err := st.Validate(); err == nil {
			t.Errorf("a step with %s is valid", name)
		}
	}
	for name, e := range map[string]*Expect{
		"no kind":               {},
		"calls without a bound": {Calls: &CallsExpect{Path: "x"}},
		"an unknown bucket":     {Budget: &BudgetExpect{Bucket: "gql", Spent: &Range{}}},
		"a budget of nothing":   {Budget: &BudgetExpect{Bucket: bucketCore}},
		"a pull of nothing":     {Pull: &PullExpect{Repo: "o/r", Number: 1}},
		"an unknown state":      {Issue: &IssueExpect{Repo: "o/r", Number: 1, State: "merged"}},
	} {
		if err := e.Validate(); err == nil {
			t.Errorf("an expectation with %s is valid", name)
		}
	}
	if err := (&FaultStep{}).Validate(); err == nil {
		t.Error("a fault step that neither arms nor clears is valid")
	}
	for name, doc := range map[string]string{
		"an unknown key":              "repositories: [{name: example/app, isues: []}]",
		"an unknown key in a budget":  "budgets: {core: {remaning: 1}}",
		"a bad issue reference":       "repositories: [{name: example/app, issues: [{number: 1, parent: nope}]}]",
		"a negative duration":         "budgets: {core: {reset_in: -1s}}",
		"an unknown key in a fault":   "faults: [{match: {paht: x}, respond: {status: 500}}]",
		"an unknown key in a project": "projects: [{owner: example, number: 1, feilds: []}]",
	} {
		dec := yaml.NewDecoder(strings.NewReader(doc))
		dec.KnownFields(true)
		var g Given
		if err := dec.Decode(&g); err == nil {
			t.Errorf("given.github with %s decodes", name)
		}
	}
	for name, doc := range map[string]string{
		"a parent the scenario lacks":   "repositories: [{name: example/app, issues: [{number: 1, parent: 9}]}]",
		"a second issue of a number":    "repositories: [{name: example/app, issues: [{number: 1}, {number: 1}]}]",
		"a pull request without a head": "repositories: [{name: example/app, pulls: [{number: 1}]}]",
		"an unknown issue state":        "repositories: [{name: example/app, issues: [{number: 1, state: merged}]}]",
		"an unknown check state":        "repositories: [{name: example/app, pulls: [{number: 1, head: x, checks: [{name: c, state: blue}]}]}]",
		"a status the board lacks":      "projects: [{owner: example, number: 1, fields: [{name: Status, options: [Ready]}]}]\nrepositories: [{name: example/app, issues: [{number: 1, board: Nope}]}]",
		"a bucket over its limit":       "budgets: {core: {limit: 10, remaining: 11}}",
		"a fault without an answer":     "faults: [{match: {path: x}}]",
		"a bad repository name":         "repositories: [{name: app}]",
		"a cost without an operation":   "costs: [{cost: 3}]",
	} {
		if err := parseGiven(t, doc).Validate(); err == nil {
			t.Errorf("given.github with %s is valid", name)
		}
	}
	var r struct {
		Exact  Range `yaml:"exact"`
		Bounds Range `yaml:"bounds"`
	}
	if err := yaml.Unmarshal([]byte("exact: 3\nbounds: {min: 1, max: 2}\n"), &r); err != nil || *r.Exact.Equals != 3 || *r.Bounds.Min != 1 || *r.Bounds.Max != 2 {
		t.Fatalf("ranges: %+v, %v", r, err)
	}
	for _, doc := range []string{"exact: {mni: 1}", "exact: {}", "exact: [1]"} {
		if err := yaml.Unmarshal([]byte(doc), &r); err == nil {
			t.Errorf("the range %q decodes", doc)
		}
	}
}

// A board the scenario does not declare is the desk's, with the usual
// statuses and every one its issues name; a declared one is as declared.
func TestBoards(t *testing.T) {
	s, _ := build(t, `
repositories:
  - name: example/app
    issues:
      - {number: 1, board: Inbox, fields: {Kind: Bug}}
`)
	out := graphql(t, s, "test", `{organization(login:"example"){projectV2(number:1){title fields(first:20){nodes{...on ProjectV2SingleSelectField{name options{name}}}}
items(first:10){nodes{type isArchived kind:fieldValueByName(name:"Kind"){...on ProjectV2ItemFieldSingleSelectValue{name optionId}} content{...on Issue{number}}}}}}}`, nil)
	project := at(t, out, "data", "organization", "projectV2")
	var statuses []string
	for _, f := range at(t, project, "fields", "nodes").([]any) {
		if m, _ := f.(map[string]any); m["name"] == "Status" {
			for _, o := range m["options"].([]any) {
				statuses = append(statuses, at(t, o, "name").(string))
			}
		}
	}
	if strings.Join(statuses, ",") != "Todo,Ready,In Progress,Blocked,Done,Inbox" {
		t.Fatalf("the desk's board's statuses: %v", statuses)
	}
	if at(t, project, "items", "nodes", 0, "kind", "name") != "Bug" || at(t, project, "items", "nodes", 0, "type") != "ISSUE" || at(t, project, "items", "nodes", 0, "content", "number") != float64(1) {
		t.Fatalf("the board's item: %v", project)
	}

	// The board's mutations, as the board client sends them.
	ids := graphql(t, s, "test", `{repository(owner:"example",name:"app"){issue(number:1){id projectItems(first:5){nodes{id project{id}}}}}
organization(login:"example"){projectV2(number:1){field(name:"Status"){...on ProjectV2SingleSelectField{id options{id name}}}}}}`, nil)
	item := at(t, ids, "data", "repository", "issue", "projectItems", "nodes", 0)
	field := at(t, ids, "data", "organization", "projectV2", "field")
	var done string
	for _, o := range at(t, field, "options").([]any) {
		if at(t, o, "name") == "Done" {
			done = at(t, o, "id").(string)
		}
	}
	moved := graphql(t, s, "test", `mutation($p:ID!,$i:ID!,$f:ID!,$v:String!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{singleSelectOptionId:$v}}){projectV2Item{id}}}`,
		map[string]any{"p": at(t, item, "project", "id"), "i": at(t, item, "id"), "f": at(t, field, "id"), "v": done})
	if moved["errors"] != nil {
		t.Fatalf("moving the item: %v", moved)
	}
	if err := s.Check(&Expect{Issue: &IssueExpect{Repo: "example/app", Number: 1, Board: "Done"}}); err != nil {
		t.Fatal(err)
	}
	archived := graphql(t, s, "test", `mutation($p:ID!,$i:ID!){archiveProjectV2Item(input:{projectId:$p,itemId:$i}){item{id isArchived}}}`,
		map[string]any{"p": at(t, item, "project", "id"), "i": at(t, item, "id")})
	if at(t, archived, "data", "archiveProjectV2Item", "item", "isArchived") != true {
		t.Fatalf("archiving the item: %v", archived)
	}
	open := graphql(t, s, "test", `{organization(login:"example"){projectV2(number:1){items(first:10,query:"is:open"){nodes{id}}}}}`, nil)
	if n := len(at(t, open, "data", "organization", "projectV2", "items", "nodes").([]any)); n != 0 {
		t.Fatalf("an archived item is no open item: %d", n)
	}
	if un := s.Unsupported(); len(un) != 0 {
		t.Fatalf("unsupported: %+v", un)
	}
}

// The issue search reads GitHub's qualifiers, free text, a dash and the
// advanced syntax's OR and parentheses, and says a qualifier it lacks.
func TestSearch(t *testing.T) {
	s, _ := build(t, fixture+`  - name: other/lib
    issues: [{number: 7, title: Elsewhere, labels: [bug]}]
`)
	for query, want := range map[string]string{
		"repo:example/app is:issue is:open sort:created-asc":          "example/app#1 example/app#2",
		"repo:example/app is:issue":                                   "example/app#3 example/app#2 example/app#1",
		"is:pr is:open draft:true":                                    "",
		"label:bug is:open":                                           "other/lib#7 example/app#1",
		"repo:example/app repo:other/lib is:issue is:open -label:bug": "example/app#2",
		"org:other": "other/lib#7",
		`(repo:example/app OR repo:other/lib) is:pr ("Closes #1" OR "none")`: "example/app#4",
		"is:pr 1":                               "example/app#4",
		"is:pr 2":                               "",
		"is:issue second":                       "example/app#2",
		"is:closed":                             "example/app#3",
		"author:@me is:pr head:fix-1 base:main": "example/app#4",
		"linked:pr is:issue":                    "example/app#1",
		"no:label is:issue is:open":             "example/app#2",
	} {
		var got []string
		err := s.Model(func(m *Model) error {
			hits, err := m.Search(query, true)
			for _, h := range hits {
				switch v := h.(type) {
				case *Issue:
					got = append(got, v.Ref())
				case *Pull:
					got = append(got, v.Ref())
				}
			}
			return err
		})
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("search %q: %v (%v), want %q", query, got, err, want)
		}
	}
	rest := decode(t, []byte(mustGH(t, s, "api", "-X", "GET", "search/issues", "-f", "q=repo:example/app is:issue is:open", "-F", "per_page=1")))
	if rest["total_count"] != float64(2) || len(rest["items"].([]any)) != 1 || spent(s, "test", bucketSearch) != 1 {
		t.Fatalf("REST search: %v, spent %+v", rest, s.Spent())
	}
	res := gh(s, "test", "api", "graphql", "-f", `query={search(query:"reactions:>5",type:ISSUE,first:5){issueCount}}`)
	if res.Exit != 1 || len(s.Unsupported()) != 1 || !strings.Contains(s.Unsupported()[0].Unsupported, "reactions") {
		t.Fatalf("a qualifier the search lacks: %+v, unsupported %+v", res, s.Unsupported())
	}
}

// The parser reads what gh and beekeeper write and says where a document
// breaks.
func TestGraphQLParse(t *testing.T) {
	doc, err := parseGraphQL(`
		# gh's list query
		fragment pr on PullRequest{number,title}
		query PullRequestList($owner: String!, $limit: Int!, $state: [PullRequestState!] = OPEN, $draft: Boolean = false) {
			repository(owner: $owner, name: "app") @include(if: true) {
				pullRequests(states: $state, first: $limit, orderBy: {field: CREATED_AT, direction: DESC}, labels: ["a", "b"]) {
					nodes { ...pr, n: number ... on PullRequest { body(format: """
						block
					""") } }
				}
			}
		}`)
	if err != nil {
		t.Fatal(err)
	}
	op := doc.Ops[0]
	if len(doc.Ops) != 1 || op.Name != "PullRequestList" || len(op.Vars) != 4 || op.Vars[2].Type != "[PullRequestState!]" ||
		op.Vars[2].Default == nil || op.Vars[2].Default.Str != "OPEN" || doc.Fragments["pr"].On != "PullRequest" {
		t.Fatalf("the document: %+v", op)
	}
	repo := op.Sels[0]
	pulls := repo.Field.Sels[0].Field
	if repo.Dirs[0].Name != "include" || len(pulls.Args) != 4 || pulls.Args[2].Value.Fields[1].Value.Str != "DESC" || len(pulls.Args[3].Value.List) != 2 {
		t.Fatalf("the arguments: %+v", pulls.Args)
	}
	nodes := pulls.Sels[0].Field.Sels
	if nodes[0].Spread != "pr" || nodes[1].Field.Alias != "n" || nodes[2].Inline.On != "PullRequest" || nodes[2].Inline.Sels[0].Field.Args[0].Value.Str != "block" {
		t.Fatalf("the selections: %+v", nodes)
	}
	for src, want := range map[string]string{
		"{ viewer { login }":        "Unexpected end of document",
		"query { viewer { } }":      "an empty selection",
		"{ a(b: ) }":                `unexpected ")"`,
		"subscription { x }":        "subscriptions are not served",
		"":                          "A query attribute must be specified",
		`{ a(b: "unterminated) }`:   "unterminated string",
		"query($a: Int = $b) { a }": "a variable where a constant is needed",
		"fragment f on A{a} fragment f on A{a} {a}": "a second fragment",
	} {
		if _, err := parseGraphQL(src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parse %q: %v, want an error with %q", src, err, want)
		}
	}
	s, _ := build(t, fixture)
	skipped := graphql(t, s, "test", `query($no:Boolean!){viewer{login @skip(if:$no) id @include(if:$no)}}`, map[string]any{"no": true})
	if _, has := at(t, skipped, "data", "viewer").(map[string]any)["login"]; has || at(t, skipped, "data", "viewer", "id") == nil {
		t.Fatalf("@skip and @include: %v", skipped)
	}
	paging := graphql(t, s, "test", `{repository(owner:"example",name:"app"){a:issues{totalCount} b:issues{nodes{number}} c:issues(first:500){nodes{number}} d:issues(last:2){nodes{number} pageInfo{hasPreviousPage startCursor}}}}`, nil)
	if at(t, paging, "data", "repository", "a", "totalCount") != float64(3) || at(t, paging, "data", "repository", "b", "nodes") != nil ||
		at(t, paging, "errors", 0, "type") != "MISSING_PAGINATION_BOUNDARIES" || at(t, paging, "errors", 1, "type") != "EXCESSIVE_PAGINATION" ||
		at(t, paging, "data", "repository", "d", "nodes", 0, "number") != float64(2) || at(t, paging, "data", "repository", "d", "pageInfo", "hasPreviousPage") != true {
		t.Fatalf("pagination: %v", paging)
	}
}
