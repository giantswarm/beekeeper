//go:build scenario

package fakegithub

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// gh issue.

// issueBase are the fields every issue command reads.
var issueBase = []string{"id", "number", "title", "state", "url"}

// findIssue is the issue of a number or a URL.
func (g *ghRun) findIssue(f flags, fields []string) (node, string, error) {
	if len(f.pos) == 0 {
		return nil, "", errors.New("accepts 1 arg(s), received 0")
	}
	selector, repo, number := f.pos[0], "", 0
	if m := issueURLRE.FindStringSubmatch(selector); m != nil {
		repo = m[1]
		number, _ = strconv.Atoi(m[2])
	} else if n, err := strconv.Atoi(strings.TrimPrefix(selector, "#")); err == nil {
		number = n
	} else {
		return nil, "", fmt.Errorf("invalid issue format: %q", selector)
	}
	if repo == "" {
		r, err := g.repo(f)
		if err != nil {
			return nil, "", err
		}
		repo = r
	}
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository struct {
			Issue node `json:"issue"`
		} `json:"repository"`
	}
	// gh reads an issue by its number as an issue or a pull request, each
	// with the fields it has.
	var pullFields []string
	for _, f := range fields {
		if !slices.Contains(issueOnlyJSONFields, f) {
			pullFields = append(pullFields, f)
		}
	}
	query := fmt.Sprintf(`query IssueByNumber($owner: String!, $repo: String!, $number: Int!) {
		repository(owner: $owner, name: $repo) {
			hasIssuesEnabled
			issue: issueOrPullRequest(number: $number) {
				__typename
				...on Issue{%s}
				...on PullRequest{%s}
			}
		}
	}`, subjectGraphQL(fields), subjectGraphQL(pullFields))
	if err := g.graphql(query, map[string]any{"owner": owner, "repo": name, "number": number}, &resp); err != nil {
		return nil, repo, err
	}
	return resp.Repository.Issue, repo, nil
}

func (g *ghRun) issueView(args []string) error {
	f, err := parseFlags("issue view", args, []string{"comments|c"}, append([]string{"repo|R"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, issueJSONFields)
	if err != nil {
		return err
	}
	if f.has("json") {
		issue, _, err := g.findIssue(f, asked)
		if err != nil {
			return err
		}
		return g.emit(f, exportSubject(issue, asked))
	}
	if f.on("comments") {
		return unhandled("the fake gh does not translate gh issue view --comments without --json")
	}
	issue, _, err := g.findIssue(f, []string{"title", "state", "author", "labels", "comments", "assignees", "projectItems", "parent", "subIssues", "subIssuesSummary", "blockedBy", "blocking", "number", "body"})
	if err != nil {
		return err
	}
	comments, _ := child(issue, "comments")["totalCount"].(float64)
	var projects []string
	for _, it := range nodesOf(issue, "projectItems") {
		projects = append(projects, text(child(it, "project"), "title"))
	}
	parent := ""
	if p := child(issue, "parent"); p != nil {
		parent = linkedRefs([]node{p})
	}
	completed := ""
	if s := child(issue, "subIssuesSummary"); s != nil {
		if total, _ := s["total"].(float64); total > 0 {
			completed = fmt.Sprintf("%v/%v", s["completed"], s["total"])
		}
	}
	fmt.Fprintf(&g.out, "title:\t%s\nstate:\t%s\nauthor:\t%s\nlabels:\t%s\ncomments:\t%d\nassignees:\t%s\nprojects:\t%s\nmilestone:\t%s\nissue-type:\t%s\nparent:\t%s\nsub-issues:\t%s\nsub-issues-completed:\t%s\nblocked-by:\t%s\nblocking:\t%s\nnumber:\t%v\n--\n%s\n",
		text(issue, "title"), text(issue, "state"), text(child(issue, "author"), "login"), names(issue, "labels", "name"), int(comments),
		names(issue, "assignees", "login"), strings.Join(projects, ", "), "", "", parent, linkedRefs(nodesOf(issue, "subIssues")), completed,
		linkedRefs(nodesOf(issue, "blockedBy")), linkedRefs(nodesOf(issue, "blocking")), issue["number"], text(issue, "body"))
	return nil
}

func (g *ghRun) issueList(args []string) error {
	f, err := parseFlags("issue list", args, nil,
		append([]string{"repo|R", "state|s", "author|A", "assignee|a", "label|l", "search|S", "limit|L"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, issueJSONFields)
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	state := cmpOr(f.str("state"), "open")
	states, ok := map[string][]string{"open": {stateOpen}, "closed": {stateClosed}, "all": {stateOpen, stateClosed}}[state]
	if !ok {
		return fmt.Errorf("invalid argument %q for \"-s, --state\" flag: valid values are {open|closed|all}", state)
	}
	limit := 30
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit <= 0 {
			return fmt.Errorf("invalid limit: %v", f.str("limit"))
		}
	}
	fields := asked
	if !f.has("json") {
		fields = []string{"number", "title", "state", "url", "updatedAt", "labels"}
	}
	owner, name, _ := strings.Cut(repo, "/")
	fragment := "fragment issue on Issue{" + subjectGraphQL(fields) + "}"
	var issues []node
	cursor := ""
	for len(issues) < limit {
		page := min(limit-len(issues), 100)
		var nodes []node
		var hasNext bool
		var end string
		if f.has("search") || f.has("label") {
			var resp struct {
				Search struct {
					Nodes    []node `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"search"`
			}
			q := []string{}
			if s := f.str("search"); s != "" {
				q = append(q, s)
			}
			if a := f.str("assignee"); a != "" {
				q = append(q, "assignee:"+ghQuote(a))
			}
			if a := f.str("author"); a != "" {
				q = append(q, "author:"+ghQuote(a))
			}
			for _, l := range f.all("label") {
				q = append(q, "label:"+ghQuote(l))
			}
			q = append(q, "repo:"+repo)
			if state != "all" {
				q = append(q, "state:"+state)
			}
			q = append(q, "type:issue")
			query := fragment + `
			query IssueSearch($type: SearchType!, $limit: Int, $after: String, $query: String!) {
				search(type: $type, first: $limit, after: $after, query: $query) {
					issueCount
					nodes { ...issue }
					pageInfo { hasNextPage endCursor }
				}
			}`
			vars := map[string]any{"type": "ISSUE", "limit": page, "query": strings.Join(q, " ")}
			if cursor != "" {
				vars["after"] = cursor
			}
			if err := g.graphql(query, vars, &resp); err != nil {
				return err
			}
			nodes, hasNext, end = resp.Search.Nodes, resp.Search.PageInfo.HasNextPage, resp.Search.PageInfo.EndCursor
		} else {
			var resp struct {
				Repository struct {
					Issues struct {
						Nodes    []node `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"issues"`
				} `json:"repository"`
			}
			query := fragment + `
			query IssueList($owner: String!, $repo: String!, $limit: Int, $endCursor: String, $states: [IssueState!] = OPEN, $assignee: String, $author: String) {
				repository(owner: $owner, name: $repo) {
					hasIssuesEnabled
					issues(first: $limit, after: $endCursor, orderBy: {field: CREATED_AT, direction: DESC}, states: $states, filterBy: {assignee: $assignee, createdBy: $author}) {
						totalCount
						nodes { ...issue }
						pageInfo { hasNextPage endCursor }
					}
				}
			}`
			vars := map[string]any{"owner": owner, "repo": name, "limit": page, "states": states}
			if a := f.str("assignee"); a != "" {
				vars["assignee"] = a
			}
			if a := f.str("author"); a != "" {
				vars["author"] = a
			}
			if cursor != "" {
				vars["endCursor"] = cursor
			}
			if err := g.graphql(query, vars, &resp); err != nil {
				return err
			}
			i := resp.Repository.Issues
			nodes, hasNext, end = i.Nodes, i.PageInfo.HasNextPage, i.PageInfo.EndCursor
		}
		issues = append(issues, nodes...)
		if !hasNext || end == "" {
			break
		}
		cursor = end
	}
	if len(issues) > limit {
		issues = issues[:limit]
	}
	if f.has("json") {
		out := []any{}
		for _, i := range issues {
			out = append(out, exportSubject(i, asked))
		}
		return g.emit(f, out)
	}
	for _, i := range issues {
		fmt.Fprintf(&g.out, "%v\t%s\t%s\t%s\t%s\n", i["number"], text(i, "state"), strings.Join(strings.Fields(text(i, "title")), " "), names(i, "labels", "name"), text(i, "updatedAt"))
	}
	return nil
}

func (g *ghRun) issueCreate(args []string) error {
	f, err := parseFlags("issue create", args, nil, []string{"repo|R", "title|t", "body|b", "body-file|F", "label|l"})
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	body, hasBody, err := g.body(f)
	if err != nil {
		return err
	}
	if !f.has("title") || !hasBody {
		return errors.New("must provide `--title` and `--body` when not running interactively")
	}
	owner, name, _ := strings.Cut(repo, "/")
	var meta struct {
		Repository struct {
			ID string `json:"id"`
		} `json:"repository"`
	}
	if err := g.graphql(`query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id hasIssuesEnabled } }`,
		map[string]any{"owner": owner, "name": name}, &meta); err != nil {
		return err
	}
	input := map[string]any{"repositoryId": meta.Repository.ID, "title": f.str("title"), "body": body}
	if labels := f.list("label"); len(labels) > 0 {
		ids, err := g.labelIDs(repo, labels)
		if err != nil {
			return err
		}
		input["labelIds"] = ids
	}
	var created struct {
		CreateIssue struct {
			Issue node `json:"issue"`
		} `json:"createIssue"`
	}
	q := `mutation IssueCreate($input: CreateIssueInput!) { createIssue(input: $input) { issue { id url } } }`
	if err := g.graphql(q, map[string]any{"input": input}, &created); err != nil {
		return err
	}
	fmt.Fprintln(&g.out, text(created.CreateIssue.Issue, "url"))
	return nil
}

// closeReasons are gh issue close's reasons and GraphQL's.
var closeReasons = map[string]string{"completed": "COMPLETED", "not planned": "NOT_PLANNED", "duplicate": "DUPLICATE"}

func (g *ghRun) issueClose(args []string) error {
	f, err := parseFlags("issue close", args, nil, []string{"repo|R", "comment|c", "reason|r"})
	if err != nil {
		return err
	}
	reason := ""
	if r := f.str("reason"); r != "" {
		mapped, ok := closeReasons[r]
		if !ok {
			return fmt.Errorf("invalid argument %q for \"-r, --reason\" flag: valid values are {completed|not planned|duplicate}", r)
		}
		reason = mapped
	}
	issue, repo, err := g.findIssue(f, issueBase)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%v (%s)", repo, issue["number"], text(issue, "title"))
	if text(issue, "state") == stateClosed {
		fmt.Fprintf(&g.err, "! Issue %s is already closed\n", ref)
		return nil
	}
	if c := f.str("comment"); c != "" {
		if _, err := g.comment(text(issue, "id"), c); err != nil {
			return err
		}
	}
	input := map[string]any{"issueId": text(issue, "id")}
	if reason != "" {
		input["stateReason"] = reason
	}
	q := `mutation IssueClose($input: CloseIssueInput!) { closeIssue(input: $input) { issue { id } } }`
	if err := g.graphql(q, map[string]any{"input": input}, nil); err != nil {
		return err
	}
	fmt.Fprintf(&g.err, "✓ Closed issue %s\n", ref)
	return nil
}

func (g *ghRun) issueReopen(args []string) error {
	f, err := parseFlags("issue reopen", args, nil, []string{"repo|R", "comment|c"})
	if err != nil {
		return err
	}
	issue, repo, err := g.findIssue(f, issueBase)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%v (%s)", repo, issue["number"], text(issue, "title"))
	if text(issue, "state") == stateOpen {
		fmt.Fprintf(&g.err, "! Issue %s is already open\n", ref)
		return nil
	}
	if c := f.str("comment"); c != "" {
		if _, err := g.comment(text(issue, "id"), c); err != nil {
			return err
		}
	}
	q := `mutation IssueReopen($input: ReopenIssueInput!) { reopenIssue(input: $input) { issue { id } } }`
	if err := g.graphql(q, map[string]any{"input": map[string]any{"issueId": text(issue, "id")}}, nil); err != nil {
		return err
	}
	fmt.Fprintf(&g.err, "✓ Reopened issue %s\n", ref)
	return nil
}

func (g *ghRun) issueComment(args []string) error {
	f, err := parseFlags("issue comment", args, nil, []string{"repo|R", "body|b", "body-file|F"})
	if err != nil {
		return err
	}
	body, hasBody, err := g.body(f)
	if err != nil {
		return err
	}
	if !hasBody {
		return errors.New("flags required when not running interactively")
	}
	issue, _, err := g.findIssue(f, issueBase)
	if err != nil {
		return err
	}
	link, err := g.comment(text(issue, "id"), body)
	if err != nil {
		return err
	}
	fmt.Fprintln(&g.out, link)
	return nil
}

func (g *ghRun) issueEdit(args []string) error {
	f, err := parseFlags("issue edit", args, nil, []string{"repo|R", "title|t", "body|b", "body-file|F", "add-label", "remove-label"})
	if err != nil {
		return err
	}
	issue, repo, err := g.findIssue(f, issueBase)
	if err != nil {
		return err
	}
	if err := g.edit(f, repo, text(issue, "id"), false); err != nil {
		return err
	}
	fmt.Fprintln(&g.out, text(issue, "url"))
	return nil
}
