//go:build scenario

package fakegithub

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gh pr, as far as beekeeper and its workers call it.

var (
	pullURLRE  = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/pull/(\d+)`)
	issueURLRE = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/(?:issues|pull)/(\d+)`)
)

// prBase are the fields every pr command reads beside those it prints.
var prBase = []string{"id", "number", "title", "state", "url", "isDraft", "headRefName", "baseRefName"}

func union(a []string, b ...string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// findPull is gh's finder: the pull request of a number, a URL, a branch,
// or the branch checked out where gh was called.
func (g *ghRun) findPull(f flags, fields []string) (node, string, error) {
	selector := ""
	if len(f.pos) > 0 {
		selector = f.pos[0]
	}
	repo := ""
	number := 0
	if m := pullURLRE.FindStringSubmatch(selector); m != nil {
		repo = m[1]
		number, _ = strconv.Atoi(m[2])
	} else if n, err := strconv.Atoi(strings.TrimPrefix(selector, "#")); err == nil && selector != "" {
		number = n
	}
	if repo == "" {
		r, err := g.repo(f)
		if err != nil {
			return nil, "", err
		}
		repo = r
	}
	owner, name, _ := strings.Cut(repo, "/")
	selection := subjectGraphQL(fields)
	if number > 0 {
		var resp struct {
			Repository struct {
				PullRequest node `json:"pullRequest"`
			} `json:"repository"`
		}
		query := fmt.Sprintf(`query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) {
			repository(owner: $owner, name: $repo) {
				pullRequest(number: $pr_number) {%s}
			}
		}`, selection)
		if err := g.graphql(query, map[string]any{"owner": owner, "repo": name, "pr_number": number}, &resp); err != nil {
			return nil, repo, err
		}
		return resp.Repository.PullRequest, repo, nil
	}
	branch := selector
	if branch == "" {
		current, ok := g.currentBranch()
		if !ok {
			return nil, repo, errors.New("could not determine current branch: failed to run git: not on any branch")
		}
		branch = current
	}
	if _, b, ok := strings.Cut(branch, ":"); ok {
		branch = b
	}
	var resp struct {
		Repository struct {
			PullRequests struct {
				Nodes []node `json:"nodes"`
			} `json:"pullRequests"`
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
		} `json:"repository"`
	}
	query := fmt.Sprintf(`query PullRequestForBranch($owner: String!, $repo: String!, $headRefName: String!, $states: [PullRequestState!]) {
		repository(owner: $owner, name: $repo) {
			pullRequests(headRefName: $headRefName, states: $states, first: 30, orderBy: { field: CREATED_AT, direction: DESC }) {
				nodes {%s}
			}
			defaultBranchRef { name }
		}
	}`, subjectGraphQL(union(fields, "state", "headRefName")))
	vars := map[string]any{"owner": owner, "repo": name, "headRefName": branch, "states": []string{stateOpen, stateClosed, stateMerged}}
	if err := g.graphql(query, vars, &resp); err != nil {
		return nil, repo, err
	}
	for _, pr := range resp.Repository.PullRequests.Nodes {
		if text(pr, "headRefName") == branch && (text(pr, "state") == stateOpen || resp.Repository.DefaultBranchRef.Name != branch) {
			return pr, repo, nil
		}
	}
	return nil, repo, fmt.Errorf("no pull requests found for branch %q", branch)
}

func (g *ghRun) prView(args []string) error {
	f, err := parseFlags("pr view", args, []string{"comments|c"}, append([]string{"repo|R"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, pullJSONFields)
	if err != nil {
		return err
	}
	if f.has("json") {
		pr, _, err := g.findPull(f, asked)
		if err != nil {
			return err
		}
		return g.emit(f, exportSubject(pr, asked))
	}
	if f.on("comments") {
		return unhandled("the fake gh does not translate gh pr view --comments without --json")
	}
	pr, _, err := g.findPull(f, []string{"title", "state", "isDraft", "author", "labels", "assignees", "projectItems", "number", "url", "additions", "deletions", "autoMergeRequest", "body"})
	if err != nil {
		return err
	}
	auto := "disabled"
	if a := child(pr, "autoMergeRequest"); a != nil {
		auto = fmt.Sprintf("enabled\t%s\t%s", text(child(a, "enabledBy"), "login"), strings.ToLower(text(a, "mergeMethod")))
	}
	var projects []string
	for _, it := range nodesOf(pr, "projectItems") {
		projects = append(projects, text(child(it, "project"), "title"))
	}
	fmt.Fprintf(&g.out, "title:\t%s\nstate:\t%s\nauthor:\t%s\nlabels:\t%s\nassignees:\t%s\nreviewers:\t%s\nprojects:\t%s\nmilestone:\t%s\nnumber:\t%v\nurl:\t%s\nadditions:\t%v\ndeletions:\t%v\nauto-merge:\t%s\n--\n%s\n",
		text(pr, "title"), stateWithDraft(pr), text(child(pr, "author"), "login"), names(pr, "labels", "name"), names(pr, "assignees", "login"),
		"", strings.Join(projects, ", "), "", pr["number"], text(pr, "url"), pr["additions"], pr["deletions"], auto, text(pr, "body"))
	return nil
}

// listStates are the states gh's --state names.
var listStates = map[string][]string{
	"open": {stateOpen}, "closed": {stateClosed, stateMerged}, "merged": {stateMerged}, "all": {stateOpen, stateClosed, stateMerged},
}

func (g *ghRun) prList(args []string) error {
	f, err := parseFlags("pr list", args, []string{"draft|d"},
		append([]string{"repo|R", "state|s", "head|H", "base|B", "author|A", "assignee|a", "label|l", "search|S", "limit|L"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, pullJSONFields)
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	state := cmpOr(f.str("state"), "open")
	states, ok := listStates[state]
	if !ok {
		return fmt.Errorf("invalid argument %q for \"-s, --state\" flag: valid values are {open|closed|merged|all}", state)
	}
	limit := 30
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit <= 0 {
			return fmt.Errorf("invalid value for --limit: %v", f.str("limit"))
		}
	}
	fields := asked
	if !f.has("json") {
		fields = []string{"number", "title", "state", "url", "headRefName", "headRepositoryOwner", "isCrossRepository", "isDraft", "createdAt"}
	}
	owner, name, _ := strings.Cut(repo, "/")
	fragment := "fragment pr on PullRequest{" + subjectGraphQL(fields) + "}"
	var prs []node
	useSearch := f.has("draft") || f.has("author") || f.has("assignee") || f.has("search") || f.has("label")
	cursor := ""
	for len(prs) < limit {
		page := min(limit-len(prs), 100)
		var nodes []node
		var hasNext bool
		var end string
		if useSearch {
			var resp struct {
				Search struct {
					Nodes    []node `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"search"`
			}
			query := fragment + `
			query PullRequestSearch($q: String!, $limit: Int!, $endCursor: String) {
				search(query: $q, type: ISSUE, first: $limit, after: $endCursor) {
					issueCount
					nodes { ...pr }
					pageInfo { hasNextPage endCursor }
				}
			}`
			vars := map[string]any{"q": prSearchQuery(f, repo, state), "limit": page}
			if cursor != "" {
				vars["endCursor"] = cursor
			}
			if err := g.graphql(query, vars, &resp); err != nil {
				return err
			}
			nodes, hasNext, end = resp.Search.Nodes, resp.Search.PageInfo.HasNextPage, resp.Search.PageInfo.EndCursor
		} else {
			var resp struct {
				Repository struct {
					PullRequests struct {
						Nodes    []node `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"pullRequests"`
				} `json:"repository"`
			}
			query := fragment + `
			query PullRequestList($owner: String!, $repo: String!, $limit: Int!, $endCursor: String, $baseBranch: String, $headBranch: String, $state: [PullRequestState!] = OPEN) {
				repository(owner: $owner, name: $repo) {
					pullRequests(states: $state, baseRefName: $baseBranch, headRefName: $headBranch, first: $limit, after: $endCursor, orderBy: {field: CREATED_AT, direction: DESC}) {
						totalCount
						nodes { ...pr }
						pageInfo { hasNextPage endCursor }
					}
				}
			}`
			vars := map[string]any{"owner": owner, "repo": name, "limit": page, "state": states}
			if b := f.str("base"); b != "" {
				vars["baseBranch"] = b
			}
			if h := f.str("head"); h != "" {
				vars["headBranch"] = h
			}
			if cursor != "" {
				vars["endCursor"] = cursor
			}
			if err := g.graphql(query, vars, &resp); err != nil {
				return err
			}
			p := resp.Repository.PullRequests
			nodes, hasNext, end = p.Nodes, p.PageInfo.HasNextPage, p.PageInfo.EndCursor
		}
		prs = append(prs, nodes...)
		if !hasNext || end == "" {
			break
		}
		cursor = end
	}
	if len(prs) > limit {
		prs = prs[:limit]
	}
	if f.has("json") {
		out := []any{}
		for _, pr := range prs {
			out = append(out, exportSubject(pr, asked))
		}
		return g.emit(f, out)
	}
	// Without a terminal gh lists nothing for no result, and exits 0.
	for _, pr := range prs {
		fmt.Fprintf(&g.out, "%v\t%s\t%s\t%s\t%s\n", pr["number"], strings.Join(strings.Fields(text(pr, "title")), " "), text(pr, "headRefName"), stateWithDraft(pr), text(pr, "createdAt"))
	}
	return nil
}

func ghQuote(s string) string {
	if strings.ContainsAny(s, " \"\t\r\n") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// prSearchQuery is the search gh pr list makes of its filters: its
// keywords, then the qualifiers by name.
func prSearchQuery(f flags, repo, state string) string {
	type qualifier struct{ key, value string }
	var qs []qualifier
	add := func(key, value string) {
		if value != "" {
			qs = append(qs, qualifier{key, key + ":" + ghQuote(value)})
		}
	}
	add("assignee", f.str("assignee"))
	add("author", f.str("author"))
	add("base", f.str("base"))
	if f.has("draft") {
		add("draft", strconv.FormatBool(f.on("draft")))
	}
	add("head", f.str("head"))
	labels := f.all("label")
	sort.Strings(labels)
	for _, l := range labels {
		add("label", l)
	}
	add("repo", repo)
	switch state {
	case "open", "closed":
		add("state", state)
	case "merged":
		add("is", "merged")
	}
	add("type", "pr")
	sort.SliceStable(qs, func(i, j int) bool { return qs[i].key < qs[j].key })
	parts := []string{}
	if s := f.str("search"); s != "" {
		parts = append(parts, s)
	}
	for _, q := range qs {
		parts = append(parts, q.value)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// checkJSONFields are the --json fields of gh pr checks.
var checkJSONFields = []string{"bucket", "completedAt", "description", "event", "link", "name", "startedAt", "state", "workflow"}

func (g *ghRun) prChecks(args []string) error {
	f, err := parseFlags("pr checks", args, []string{"required"}, append([]string{"repo|R"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, checkJSONFields)
	if err != nil {
		return err
	}
	if f.on("required") {
		return unhandled("the fake gh does not translate gh pr checks --required")
	}
	pr, _, err := g.findPull(f, []string{"number", "headRefName", "statusCheckRollup"})
	if err != nil {
		return err
	}
	rollup, _ := exportRollup(pr).([]any)
	if len(rollup) == 0 {
		return fmt.Errorf("no checks reported on the '%s' branch", text(pr, "headRefName"))
	}
	var contexts []node
	for _, c := range rollup {
		contexts = append(contexts, c.(node))
	}
	// gh keeps the latest of each check, the newest first.
	sort.SliceStable(contexts, func(i, j int) bool { return text(contexts[i], "startedAt") > text(contexts[j], "startedAt") })
	seen := map[string]bool{}
	var checks []node
	failed, pending := 0, 0
	for _, c := range contexts {
		name := cmpOr(text(c, "name"), text(c, "context"))
		k := "context/" + name
		if text(c, "context") == "" {
			k = fmt.Sprintf("%s/%s/%s", name, text(c, "workflowName"), "")
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		state := text(c, "state")
		if state == "" {
			if text(c, "status") == checkCompleted {
				state = text(c, "conclusion")
			} else {
				state = text(c, "status")
			}
		}
		bucket := "pending"
		switch state {
		case "SUCCESS":
			bucket = "pass"
		case "SKIPPED", "NEUTRAL":
			bucket = "skipping"
		case "ERROR", "FAILURE", "TIMED_OUT", "ACTION_REQUIRED":
			bucket = "fail"
			failed++
		case "CANCELLED":
			bucket = "cancel"
		default:
			pending++
		}
		checks = append(checks, node{
			"name": name, "state": state, "startedAt": timeOr(c, "startedAt"), "completedAt": timeOr(c, "completedAt"),
			"link": cmpOr(text(c, "detailsUrl"), text(c, "targetUrl")), "bucket": bucket, "event": "", "workflow": text(c, "workflowName"), "description": "",
		})
	}
	if f.has("json") {
		out := []any{}
		for _, c := range checks {
			entry := node{}
			for _, field := range asked {
				entry[field] = c[field]
			}
			out = append(out, entry)
		}
		return g.emit(f, out)
	}
	for _, c := range checks {
		elapsed := ""
		if start, err := time.Parse(time.RFC3339, text(c, "startedAt")); err == nil && text(c, "completedAt") != zeroTime && !start.IsZero() {
			if end, err := time.Parse(time.RFC3339, text(c, "completedAt")); err == nil && end.After(start) {
				elapsed = end.Sub(start).String()
			}
		}
		name := text(c, "name")
		if w := text(c, "workflow"); w != "" {
			name = w + "/" + name
		}
		fmt.Fprintf(&g.out, "%s\t%s\t%s\t%s\t%s\n", name, text(c, "bucket"), elapsed, text(c, "link"), text(c, "description"))
	}
	switch {
	case failed > 0:
		return errSilent
	case pending > 0:
		return &errExit{code: 8}
	}
	return nil
}

// body is a command's --body or --body-file.
func (g *ghRun) body(f flags) (string, bool, error) {
	if file := f.str("body-file"); file != "" {
		b, err := g.readFile(file)
		return string(b), true, err
	}
	return f.str("body"), f.has("body"), nil
}

// labelIDs are the node ids of a repository's labels of those names.
func (g *ghRun) labelIDs(repo string, labelNames []string) ([]string, error) {
	if len(labelNames) == 0 {
		return nil, nil
	}
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository struct {
			Labels struct {
				Nodes []node `json:"nodes"`
			} `json:"labels"`
		} `json:"repository"`
	}
	query := `query RepositoryLabelList($owner: String!, $repo: String!) {
		repository(owner: $owner, name: $repo) { labels(first: 100) { nodes { id name } pageInfo { hasNextPage endCursor } } }
	}`
	if err := g.graphql(query, map[string]any{"owner": owner, "repo": name}, &resp); err != nil {
		return nil, err
	}
	var ids []string
	for _, want := range labelNames {
		found := false
		for _, l := range resp.Repository.Labels.Nodes {
			if strings.EqualFold(text(l, "name"), want) {
				ids, found = append(ids, text(l, "id")), true
			}
		}
		if !found {
			return nil, fmt.Errorf("could not add label: '%s' not found", want)
		}
	}
	return ids, nil
}

func (g *ghRun) prCreate(args []string) error {
	f, err := parseFlags("pr create", args, []string{"draft|d", "no-maintainer-edit"},
		[]string{"repo|R", "base|B", "head|H", "title|t", "body|b", "body-file|F", "label|l"})
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
		return errors.New("must provide `--title` and `--body` (or `--fill` or `fill-first` or `--fillverbose`) when not running interactively")
	}
	head := f.str("head")
	if head == "" {
		current, ok := g.currentBranch()
		if !ok {
			return errors.New("could not determine the current branch: failed to run git: not on any branch")
		}
		head = current
	}
	if _, b, ok := strings.Cut(head, ":"); ok {
		head = b
	}
	owner, name, _ := strings.Cut(repo, "/")
	var meta struct {
		Repository struct {
			ID               string `json:"id"`
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			PullRequests struct {
				Nodes []node `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	query := `query PullRequestForBranch($owner: String!, $repo: String!, $headRefName: String!) {
		repository(owner: $owner, name: $repo) {
			id
			defaultBranchRef { name }
			pullRequests(headRefName: $headRefName, states: OPEN, first: 30) { nodes { number url baseRefName headRefName } }
		}
	}`
	if err := g.graphql(query, map[string]any{"owner": owner, "repo": name, "headRefName": head}, &meta); err != nil {
		return err
	}
	base := cmpOr(f.str("base"), meta.Repository.DefaultBranchRef.Name)
	for _, pr := range meta.Repository.PullRequests.Nodes {
		if text(pr, "baseRefName") == base {
			return fmt.Errorf("a pull request for branch %q into branch %q already exists:\n%s", head, base, text(pr, "url"))
		}
	}
	var created struct {
		CreatePullRequest struct {
			PullRequest node `json:"pullRequest"`
		} `json:"createPullRequest"`
	}
	mutation := `mutation PullRequestCreate($input: CreatePullRequestInput!) {
		createPullRequest(input: $input) { pullRequest { id url } }
	}`
	input := map[string]any{"repositoryId": meta.Repository.ID, "baseRefName": base, "headRefName": head, "title": f.str("title"), "body": body, "draft": f.on("draft")}
	if err := g.graphql(mutation, map[string]any{"input": input}, &created); err != nil {
		return fmt.Errorf("pull request create failed: %w", err)
	}
	pr := created.CreatePullRequest.PullRequest
	if labels := f.list("label"); len(labels) > 0 {
		ids, err := g.labelIDs(repo, labels)
		if err != nil {
			return err
		}
		update := `mutation PullRequestCreateMetadata($input: UpdatePullRequestInput!) { updatePullRequest(input: $input) { clientMutationId } }`
		if err := g.graphql(update, map[string]any{"input": map[string]any{"pullRequestId": text(pr, "id"), "labelIds": ids}}, nil); err != nil {
			return err
		}
	}
	fmt.Fprintln(&g.out, text(pr, "url"))
	return nil
}

func (g *ghRun) prMerge(args []string) error {
	f, err := parseFlags("pr merge", args, []string{"merge|m", "squash|s", "rebase|r", "admin", "auto", "disable-auto", "delete-branch|d"},
		[]string{"repo|R", "match-head-commit", "subject|t", "body|b", "body-file|F"})
	if err != nil {
		return err
	}
	methods := 0
	method := ""
	for flag, m := range map[string]string{"merge": "MERGE", "squash": "SQUASH", "rebase": "REBASE"} {
		if f.on(flag) {
			methods++
			method = m
		}
	}
	if methods > 1 {
		return errors.New("only one of --merge, --rebase, or --squash can be enabled")
	}
	pr, repo, err := g.findPull(f, union(prBase, "mergeable", "mergeStateStatus", "isInMergeQueue", "isMergeQueueEnabled"))
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%v", repo, pr["number"])
	id := text(pr, "id")
	if f.on("disable-auto") {
		q := `mutation PullRequestAutoMergeDisable($prID: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $prID}) { clientMutationId } }`
		return g.graphql(q, map[string]any{"prID": id}, nil)
	}
	merged := text(pr, "state") == stateMerged
	status := text(pr, "mergeStateStatus")
	admin := f.on("admin")
	immediately := status == "CLEAN" || status == "HAS_HOOKS" || status == "UNSTABLE"
	auto := f.on("auto") && !immediately
	queued := pr["isMergeQueueEnabled"] == true
	if queued && f.on("delete-branch") {
		return errors.New("X Cannot use `-d` or `--delete-branch` when merge queue enabled")
	}
	reason := ""
	switch status {
	case "BLOCKED":
		if !admin {
			reason = "the base branch policy prohibits the merge"
		}
	case "BEHIND":
		if !admin {
			reason = "the head branch is not up to date with the base branch"
		}
	case "DIRTY":
		reason = "the merge commit cannot be cleanly created"
	}
	if !queued && reason != "" && !auto && !merged {
		fmt.Fprintf(&g.err, "X Pull request %s is not mergeable: %s.\n", ref, reason)
		fmt.Fprintln(&g.err, "To have the pull request merged after all the requirements have been met, add the `--auto` flag.")
		if !admin && (status == "BLOCKED" || status == "BEHIND") {
			fmt.Fprintln(&g.err, "To use administrator privileges to immediately merge the pull request, add the `--admin` flag.")
		}
		return errSilent
	}
	if !merged {
		toQueue := queued && !admin
		if !toQueue && method == "" {
			return errors.New("--merge, --rebase, or --squash required when not running interactively")
		}
		body, hasBody, err := g.body(f)
		if err != nil {
			return err
		}
		input := map[string]any{"pullRequestId": id}
		if !toQueue {
			input["mergeMethod"] = method
		}
		if s := f.str("subject"); s != "" {
			input["commitHeadline"] = s
		}
		if hasBody {
			input["commitBody"] = body
		}
		if sha := f.str("match-head-commit"); sha != "" {
			input["expectedHeadOid"] = sha
		}
		if auto || toQueue {
			q := `mutation PullRequestAutoMerge($input: EnablePullRequestAutoMergeInput!) { enablePullRequestAutoMerge(input: $input) { clientMutationId } }`
			return g.graphql(q, map[string]any{"input": input}, nil)
		}
		q := `mutation PullRequestMerge($input: MergePullRequestInput!) { mergePullRequest(input: $input) { clientMutationId } }`
		if err := g.graphql(q, map[string]any{"input": input}, nil); err != nil {
			return err
		}
	} else if f.on("delete-branch") {
		fmt.Fprintf(&g.err, "! Pull request %s was already merged\n", ref)
	}
	if f.on("delete-branch") {
		return g.deleteBranch(repo, text(pr, "headRefName"))
	}
	return nil
}

// deleteBranch deletes a pull request's head branch, as gh does through
// REST; a branch already gone is no error.
func (g *ghRun) deleteBranch(repo, branch string) error {
	resp := g.do(http.MethodDelete, "/repos/"+repo+"/git/refs/heads/"+url.PathEscape(branch), nil, nil, nil)
	if resp.Outage {
		return connectError()
	}
	if resp.Status >= 300 && resp.Status != http.StatusUnprocessableEntity && resp.Status != http.StatusNotFound {
		return fmt.Errorf("failed to delete remote branch %s: %w", branch, httpError(resp, http.MethodDelete, "/repos/"+repo+"/git/refs/heads/"+branch))
	}
	return nil
}

func (g *ghRun) comment(id, body string) (string, error) {
	var resp struct {
		AddComment struct {
			CommentEdge struct {
				Node node `json:"node"`
			} `json:"commentEdge"`
		} `json:"addComment"`
	}
	q := `mutation CommentCreate($input: AddCommentInput!) { addComment(input: $input) { commentEdge { node { url } } } }`
	err := g.graphql(q, map[string]any{"input": map[string]any{"subjectId": id, "body": body}}, &resp)
	return text(resp.AddComment.CommentEdge.Node, "url"), err
}

func (g *ghRun) prClose(args []string) error {
	f, err := parseFlags("pr close", args, []string{"delete-branch|d"}, []string{"repo|R", "comment|c"})
	if err != nil {
		return err
	}
	pr, repo, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%v (%s)", repo, pr["number"], text(pr, "title"))
	switch text(pr, "state") {
	case stateMerged:
		fmt.Fprintf(&g.err, "X Pull request %s can't be closed because it was already merged\n", ref)
		return errSilent
	case stateClosed:
		fmt.Fprintf(&g.err, "! Pull request %s is already closed\n", ref)
		return nil
	}
	if c := f.str("comment"); c != "" {
		if _, err := g.comment(text(pr, "id"), c); err != nil {
			return err
		}
	}
	q := `mutation PullRequestClose($input: ClosePullRequestInput!) { closePullRequest(input: $input) { pullRequest { id } } }`
	if err := g.graphql(q, map[string]any{"input": map[string]any{"pullRequestId": text(pr, "id")}}, nil); err != nil {
		return fmt.Errorf("API call failed: %w", err)
	}
	fmt.Fprintf(&g.err, "✓ Closed pull request %s\n", ref)
	if f.on("delete-branch") {
		if err := g.deleteBranch(repo, text(pr, "headRefName")); err != nil {
			return err
		}
		fmt.Fprintf(&g.err, "✓ Deleted branch %s\n", text(pr, "headRefName"))
	}
	return nil
}

func (g *ghRun) prReopen(args []string) error {
	f, err := parseFlags("pr reopen", args, nil, []string{"repo|R", "comment|c"})
	if err != nil {
		return err
	}
	pr, repo, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%v (%s)", repo, pr["number"], text(pr, "title"))
	switch text(pr, "state") {
	case stateMerged:
		fmt.Fprintf(&g.err, "X Pull request %s can't be reopened because it was already merged\n", ref)
		return errSilent
	case stateOpen:
		fmt.Fprintf(&g.err, "! Pull request %s is already open\n", ref)
		return nil
	}
	if c := f.str("comment"); c != "" {
		if _, err := g.comment(text(pr, "id"), c); err != nil {
			return err
		}
	}
	q := `mutation PullRequestReopen($input: ReopenPullRequestInput!) { reopenPullRequest(input: $input) { pullRequest { id } } }`
	if err := g.graphql(q, map[string]any{"input": map[string]any{"pullRequestId": text(pr, "id")}}, nil); err != nil {
		return fmt.Errorf("API call failed: %w", err)
	}
	fmt.Fprintf(&g.err, "✓ Reopened pull request %s\n", ref)
	return nil
}

func (g *ghRun) prReady(args []string) error {
	f, err := parseFlags("pr ready", args, []string{"undo"}, []string{"repo|R"})
	if err != nil {
		return err
	}
	pr, repo, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%v", repo, pr["number"])
	if text(pr, "state") != stateOpen {
		fmt.Fprintf(&g.err, "X Pull request %s is closed. Only draft pull requests can be marked as \"ready for review\"\n", ref)
		return errSilent
	}
	draft := pr["isDraft"] == true
	input := map[string]any{"input": map[string]any{"pullRequestId": text(pr, "id")}}
	if f.on("undo") {
		if draft {
			fmt.Fprintf(&g.err, "! Pull request %s is already \"in draft\"\n", ref)
			return nil
		}
		q := `mutation ConvertPullRequestToDraft($input: ConvertPullRequestToDraftInput!) { convertPullRequestToDraft(input: $input) { pullRequest { id } } }`
		if err := g.graphql(q, input, nil); err != nil {
			return fmt.Errorf("API call failed: %w", err)
		}
		fmt.Fprintf(&g.err, "✓ Pull request %s is converted to \"draft\"\n", ref)
		return nil
	}
	if !draft {
		fmt.Fprintf(&g.err, "! Pull request %s is already \"ready for review\"\n", ref)
		return nil
	}
	q := `mutation PullRequestReadyForReview($input: MarkPullRequestReadyForReviewInput!) { markPullRequestReadyForReview(input: $input) { pullRequest { id } } }`
	if err := g.graphql(q, input, nil); err != nil {
		return fmt.Errorf("API call failed: %w", err)
	}
	fmt.Fprintf(&g.err, "✓ Pull request %s is marked as \"ready for review\"\n", ref)
	return nil
}

func (g *ghRun) prComment(args []string) error {
	f, err := parseFlags("pr comment", args, nil, []string{"repo|R", "body|b", "body-file|F"})
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
	pr, _, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	link, err := g.comment(text(pr, "id"), body)
	if err != nil {
		return err
	}
	fmt.Fprintln(&g.out, link)
	return nil
}

func (g *ghRun) prReview(args []string) error {
	f, err := parseFlags("pr review", args, []string{"approve|a", "request-changes|r", "comment|c"}, []string{"repo|R", "body|b", "body-file|F"})
	if err != nil {
		return err
	}
	event, picked := "", 0
	for flag, e := range map[string]string{"approve": "APPROVE", "request-changes": "REQUEST_CHANGES", "comment": "COMMENT"} {
		if f.on(flag) {
			event = e
			picked++
		}
	}
	switch {
	case picked > 1:
		return errors.New("need exactly one of --approve, --request-changes, or --comment")
	case picked == 0:
		return errors.New("--approve, --request-changes, or --comment required when not running interactively")
	}
	body, _, err := g.body(f)
	if err != nil {
		return err
	}
	if body == "" && event != "APPROVE" {
		return errors.New("body cannot be blank for " + map[string]string{"REQUEST_CHANGES": "request-changes", "COMMENT": "comment"}[event] + " review")
	}
	pr, repo, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	q := `mutation PullRequestReviewAdd($input: AddPullRequestReviewInput!) { addPullRequestReview(input: $input) { clientMutationId } }`
	input := map[string]any{"pullRequestId": text(pr, "id"), "event": event, "body": body}
	if err := g.graphql(q, map[string]any{"input": input}, nil); err != nil {
		return fmt.Errorf("failed to create review: %w", err)
	}
	ref := fmt.Sprintf("%s#%v", repo, pr["number"])
	switch event {
	case "COMMENT":
		fmt.Fprintf(&g.err, "- Reviewed pull request %s\n", ref)
	case "APPROVE":
		fmt.Fprintf(&g.err, "✓ Approved pull request %s\n", ref)
	default:
		fmt.Fprintf(&g.err, "+ Requested changes to pull request %s\n", ref)
	}
	return nil
}

func (g *ghRun) prEdit(args []string) error {
	f, err := parseFlags("pr edit", args, nil, []string{"repo|R", "title|t", "body|b", "body-file|F", "base|B", "add-label", "remove-label"})
	if err != nil {
		return err
	}
	pr, repo, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	if err := g.edit(f, repo, text(pr, "id"), true); err != nil {
		return err
	}
	fmt.Fprintln(&g.out, text(pr, "url"))
	return nil
}

// edit applies gh pr edit's and gh issue edit's flags to a subject.
func (g *ghRun) edit(f flags, repo, id string, pull bool) error {
	body, hasBody, err := g.body(f)
	if err != nil {
		return err
	}
	input := map[string]any{}
	if f.has("title") {
		input["title"] = f.str("title")
	}
	if hasBody {
		input["body"] = body
	}
	if pull && f.has("base") {
		input["baseRefName"] = f.str("base")
	}
	if len(input) > 0 {
		q := `mutation IssueUpdate($input: UpdateIssueInput!) { updateIssue(input: $input) { __typename } }`
		input["id"] = id
		if pull {
			q = `mutation PullRequestUpdate($input: UpdatePullRequestInput!) { updatePullRequest(input: $input) { __typename } }`
			delete(input, "id")
			input["pullRequestId"] = id
		}
		if err := g.graphql(q, map[string]any{"input": input}, nil); err != nil {
			return err
		}
	}
	for flag, mutation := range map[string]string{"add-label": "addLabelsToLabelable", "remove-label": "removeLabelsFromLabelable"} {
		labels := f.list(flag)
		if len(labels) == 0 {
			continue
		}
		ids, err := g.labelIDs(repo, labels)
		if err != nil {
			return err
		}
		q := fmt.Sprintf(`mutation LabelUpdate($input: %sInput!) { %s(input: $input) { __typename } }`, strings.ToUpper(mutation[:1])+mutation[1:], mutation)
		if err := g.graphql(q, map[string]any{"input": map[string]any{"labelableId": id, "labelIds": ids}}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (g *ghRun) prDiff(args []string) error {
	f, err := parseFlags("pr diff", args, []string{"patch", "name-only"}, []string{"repo|R", "color"})
	if err != nil {
		return err
	}
	if f.on("patch") {
		return unhandled("the fake gh does not translate gh pr diff --patch")
	}
	pr, repo, err := g.findPull(f, prBase)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/pulls/%v", repo, pr["number"])
	header := http.Header{}
	header.Set("Accept", "application/vnd.github.diff; charset=utf-8")
	resp := g.do(http.MethodGet, path, nil, header, nil)
	if resp.Outage {
		return connectError()
	}
	if resp.Status != http.StatusOK {
		return fmt.Errorf("could not find pull request diff: %w", httpError(resp, http.MethodGet, path))
	}
	if !f.on("name-only") {
		g.out.Write(resp.Body)
		return nil
	}
	for _, line := range strings.Split(string(resp.Body), "\n") {
		if rest, ok := strings.CutPrefix(line, "+++ b/"); ok {
			fmt.Fprintln(&g.out, rest)
		}
	}
	return nil
}

func (g *ghRun) prUpdateBranch(args []string) error {
	f, err := parseFlags("pr update-branch", args, []string{"rebase"}, []string{"repo|R"})
	if err != nil {
		return err
	}
	pr, _, err := g.findPull(f, union(prBase, "headRefOid", "mergeable"))
	if err != nil {
		return err
	}
	if text(pr, "mergeable") == "CONFLICTING" {
		fmt.Fprintln(&g.err, "X Cannot update PR branch due to conflicts")
		return errSilent
	}
	method := "MERGE"
	if f.on("rebase") {
		method = "REBASE"
	}
	q := `mutation PullRequestUpdateBranch($input: UpdatePullRequestBranchInput!) { updatePullRequestBranch(input: $input) { pullRequest { id } } }`
	input := map[string]any{"pullRequestId": text(pr, "id"), "expectedHeadOid": text(pr, "headRefOid"), "updateMethod": method}
	if err := g.graphql(q, map[string]any{"input": input}, nil); err != nil {
		if strings.Contains(err.Error(), "no new commits on the base branch") {
			fmt.Fprintln(&g.out, "✓ PR branch already up-to-date")
			return nil
		}
		if strings.Contains(err.Error(), "merge conflict between base and head") {
			fmt.Fprintln(&g.err, "X Cannot update PR branch due to conflicts")
			return errSilent
		}
		return err
	}
	fmt.Fprintln(&g.out, "✓ PR branch updated")
	return nil
}
