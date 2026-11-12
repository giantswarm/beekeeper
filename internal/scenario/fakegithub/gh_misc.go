//go:build scenario

package fakegithub

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// gh repo, gh label, gh run and gh release.

// repoJSONFields are the gh repo view fields the model serves, each a
// field of GraphQL's Repository as gh selects it.
var repoJSONFields = map[string]string{
	"id": "id", "name": "name", "nameWithOwner": "nameWithOwner", "owner": "owner{id,login}", "description": "description", "url": "url",
	"sshUrl": "sshUrl", "isPrivate": "isPrivate", "visibility": "visibility", "isArchived": "isArchived", "isFork": "isFork",
	"isEmpty": "isEmpty", "isTemplate": "isTemplate", "isInOrganization": "isInOrganization", "createdAt": "createdAt",
	"pushedAt": "pushedAt", "updatedAt": "updatedAt", "defaultBranchRef": "defaultBranchRef{name}", "parent": "parent{id,name,owner{id,login}}",
	"viewerPermission": "viewerPermission", "viewerCanAdminister": "viewerCanAdminister", "viewerDefaultMergeMethod": "viewerDefaultMergeMethod",
	"hasIssuesEnabled": "hasIssuesEnabled", "hasProjectsEnabled": "hasProjectsEnabled", "hasWikiEnabled": "hasWikiEnabled",
	"mergeCommitAllowed": "mergeCommitAllowed", "squashMergeAllowed": "squashMergeAllowed", "rebaseMergeAllowed": "rebaseMergeAllowed",
	"deleteBranchOnMerge": "deleteBranchOnMerge", "latestRelease": "latestRelease{publishedAt,tagName,name,url}",
}

func (g *ghRun) repoView(args []string) error {
	f, err := parseFlags("repo view", args, nil, append([]string{"branch|b"}, jsonFlags...))
	if err != nil {
		return err
	}
	if !f.has("json") {
		return unhandled("the fake gh translates gh repo view with --json only")
	}
	var selection []string
	for _, field := range f.list("json") {
		sel, ok := repoJSONFields[field]
		if !ok {
			return unhandled("the fake gh does not serve the gh repo view field %q", field)
		}
		selection = append(selection, sel)
	}
	var repo string
	if len(f.pos) > 0 {
		if repo, err = repoArg(f.pos[0]); err != nil {
			return err
		}
	} else if repo, err = g.repo(f); err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository node `json:"repository"`
	}
	query := fmt.Sprintf(`query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {%s} }`, strings.Join(selection, ","))
	if err := g.graphql(query, map[string]any{"owner": owner, "name": name}, &resp); err != nil {
		return err
	}
	out := node{}
	for _, field := range f.list("json") {
		out[field] = resp.Repository[field]
	}
	return g.emit(f, out)
}

func (g *ghRun) labelCreate(args []string) error {
	f, err := parseFlags("label create", args, []string{"force|f"}, []string{"repo|R", "color|c", "description|d"})
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return errors.New("cannot create label: name argument required")
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	name := f.pos[0]
	label := map[string]any{"name": name, "color": strings.TrimPrefix(cmpOr(f.str("color"), "ededed"), "#"), "description": f.str("description")}
	body, _ := marshalNoEscape(label)
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=utf-8")
	path := "/repos/" + repo + "/labels"
	resp := g.do(http.MethodPost, path, nil, header, body)
	switch {
	case resp.Outage:
		return connectError()
	case resp.Status == http.StatusUnprocessableEntity && strings.Contains(string(resp.Body), "already_exists"):
		if !f.on("force") {
			return fmt.Errorf("label with name %q already exists; use `--force` to update its color and description", name)
		}
		label["new_name"] = name
		return g.rest(http.MethodPatch, path+"/"+url.PathEscape(name), nil, label, nil)
	case resp.Status >= 300:
		return httpError(resp, http.MethodPost, path)
	}
	return nil
}

func (g *ghRun) labelList(args []string) error {
	f, err := parseFlags("label list", args, nil, append([]string{"repo|R", "limit|L", "search|S"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, []string{"color", "description", "id", "isDefault", "name", "url"})
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository struct {
			Labels struct {
				Nodes []node `json:"nodes"`
			} `json:"labels"`
		} `json:"repository"`
	}
	query := `query LabelList($owner: String!, $repo: String!, $limit: Int!, $query: String) {
		repository(owner: $owner, name: $repo) { labels(first: $limit, query: $query) { totalCount, nodes { id name color description }, pageInfo { hasNextPage endCursor } } }
	}`
	limit := 30
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit <= 0 {
			return fmt.Errorf("invalid limit: %v", f.str("limit"))
		}
	}
	vars := map[string]any{"owner": owner, "repo": name, "limit": min(limit, 100)}
	if s := f.str("search"); s != "" {
		vars["query"] = s
	}
	if err := g.graphql(query, vars, &resp); err != nil {
		return err
	}
	if f.has("json") {
		out := []any{}
		for _, l := range resp.Repository.Labels.Nodes {
			entry := node{}
			for _, field := range asked {
				switch field {
				case "isDefault":
					entry[field] = false
				case "url":
					entry[field] = fmt.Sprintf("%s/%s/labels/%s", webHost, repo, url.PathEscape(text(l, "name")))
				default:
					entry[field] = text(l, field)
				}
			}
			out = append(out, entry)
		}
		return g.emit(f, out)
	}
	for _, l := range resp.Repository.Labels.Nodes {
		fmt.Fprintf(&g.out, "%s\t%s\t#%s\n", text(l, "name"), text(l, "description"), text(l, "color"))
	}
	return nil
}

// runJSONFields are gh run's --json fields and the REST fields they read.
var runJSONFields = map[string]string{
	"name": "name", "displayTitle": "display_title", "headBranch": "head_branch", "headSha": "head_sha", "createdAt": "created_at",
	"updatedAt": "updated_at", "startedAt": "run_started_at", "attempt": "run_attempt", "status": "status", "conclusion": "conclusion",
	"event": "event", "number": "run_number", "databaseId": "id", "workflowDatabaseId": "workflow_id", "workflowName": "name", "url": "html_url",
}

func runFieldNames(single bool) []string {
	out := make([]string, 0, len(runJSONFields)+1)
	for f := range runJSONFields {
		out = append(out, f)
	}
	if single {
		out = append(out, "jobs")
	}
	slices.Sort(out)
	return out
}

func exportRun(run node, fields []string, jobs []any) node {
	out := node{}
	for _, f := range fields {
		switch f {
		case "jobs":
			out[f] = jobs
		case "conclusion", "headBranch":
			out[f] = text(run, runJSONFields[f])
		default:
			out[f] = run[runJSONFields[f]]
		}
	}
	return out
}

func (g *ghRun) runList(args []string) error {
	f, err := parseFlags("run list", args, nil,
		append([]string{"repo|R", "branch|b", "commit|c", "event|e", "status|s", "limit|L"}, jsonFlags...))
	if err != nil {
		return err
	}
	if !f.has("json") {
		return unhandled("the fake gh translates gh run list with --json only")
	}
	asked, err := g.fields(f, runFieldNames(false))
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	limit := 20
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit <= 0 {
			return fmt.Errorf("invalid limit: %v", f.str("limit"))
		}
	}
	query := url.Values{}
	query.Set("per_page", strconv.Itoa(min(limit, 100)))
	for flag, param := range map[string]string{"branch": "branch", "commit": "head_sha", "event": "event", "status": "status"} {
		if v := f.str(flag); v != "" {
			query.Set(param, v)
		}
	}
	var resp struct {
		Runs []node `json:"workflow_runs"`
	}
	if err := g.rest(http.MethodGet, "/repos/"+repo+"/actions/runs", query, nil, &resp); err != nil {
		return err
	}
	out := []any{}
	for i, run := range resp.Runs {
		if i >= limit {
			break
		}
		out = append(out, exportRun(run, asked, nil))
	}
	return g.emit(f, out)
}

func (g *ghRun) runView(args []string) error {
	f, err := parseFlags("run view", args, []string{"exit-status"}, append([]string{"repo|R"}, jsonFlags...))
	if err != nil {
		return err
	}
	if !f.has("json") {
		return unhandled("the fake gh translates gh run view with --json only")
	}
	if len(f.pos) != 1 {
		return errors.New("run or job ID required when not running interactively")
	}
	asked, err := g.fields(f, runFieldNames(true))
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	var run node
	if err := g.rest(http.MethodGet, "/repos/"+repo+"/actions/runs/"+f.pos[0], nil, nil, &run); err != nil {
		return fmt.Errorf("failed to get run: %w", err)
	}
	var jobs []any
	if slices.Contains(asked, "jobs") {
		var resp struct {
			Jobs []node `json:"jobs"`
		}
		if err := g.rest(http.MethodGet, "/repos/"+repo+"/actions/runs/"+f.pos[0]+"/jobs", url.Values{"per_page": {"100"}}, nil, &resp); err != nil {
			return fmt.Errorf("failed to get jobs: %w", err)
		}
		jobs = []any{}
		for _, j := range resp.Jobs {
			jobs = append(jobs, node{
				"completedAt": timeOr(j, "completed_at"), "conclusion": text(j, "conclusion"), "databaseId": j["id"], "name": text(j, "name"),
				"startedAt": timeOr(j, "started_at"), "status": text(j, "status"), "steps": []any{}, "url": text(j, "html_url"),
			})
		}
	}
	if err := g.emit(f, exportRun(run, asked, jobs)); err != nil {
		return err
	}
	if f.on("exit-status") && text(run, "conclusion") != "success" && text(run, "status") == "completed" {
		return errSilent
	}
	return nil
}

func (g *ghRun) runRerun(args []string) error {
	f, err := parseFlags("run rerun", args, []string{"failed"}, []string{"repo|R"})
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return errors.New("`<run-id>` or `--job` required when not running interactively")
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	path, only := "/repos/"+repo+"/actions/runs/"+f.pos[0]+"/rerun", ""
	if f.on("failed") {
		path, only = path+"-failed-jobs", "(failed jobs) "
	}
	if err := g.rest(http.MethodPost, path, nil, nil, nil); err != nil {
		return fmt.Errorf("run %s cannot be rerun; %w", f.pos[0], err)
	}
	fmt.Fprintf(&g.out, "✓ Requested rerun %sof run %s\n", only, f.pos[0])
	return nil
}

func (g *ghRun) releaseList(args []string) error {
	f, err := parseFlags("release list", args, []string{"exclude-drafts", "exclude-pre-releases"}, append([]string{"repo|R", "limit|L", "order|O"}, jsonFlags...))
	if err != nil {
		return err
	}
	asked, err := g.fields(f, []string{"createdAt", "isDraft", "isImmutable", "isLatest", "isPrerelease", "name", "publishedAt", "tagName"})
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	limit := 30
	if f.has("limit") {
		if limit, err = strconv.Atoi(f.str("limit")); err != nil || limit <= 0 {
			return fmt.Errorf("invalid limit: %v", f.str("limit"))
		}
	}
	owner, name, _ := strings.Cut(repo, "/")
	var resp struct {
		Repository struct {
			Releases struct {
				Nodes []node `json:"nodes"`
			} `json:"releases"`
		} `json:"repository"`
	}
	query := `query RepositoryReleaseList($owner: String!, $name: String!, $perPage: Int!, $endCursor: String, $direction: OrderDirection!) {
		repository(owner: $owner, name: $name) {
			releases(first: $perPage, orderBy: {field: CREATED_AT, direction: $direction}, after: $endCursor) {
				nodes { name tagName isDraft isLatest isPrerelease createdAt publishedAt }
				pageInfo { hasNextPage endCursor }
			}
		}
	}`
	vars := map[string]any{"owner": owner, "name": name, "perPage": min(limit, 100), "direction": strings.ToUpper(cmpOr(f.str("order"), "desc"))}
	if err := g.graphql(query, vars, &resp); err != nil {
		return err
	}
	var releases []node
	for _, r := range resp.Repository.Releases.Nodes {
		if f.on("exclude-drafts") && r["isDraft"] == true || f.on("exclude-pre-releases") && r["isPrerelease"] == true {
			continue
		}
		releases = append(releases, r)
	}
	if f.has("json") {
		out := []any{}
		for _, r := range releases {
			entry := node{}
			for _, field := range asked {
				if field == "isImmutable" {
					entry[field] = false
					continue
				}
				entry[field] = r[field]
			}
			out = append(out, entry)
		}
		return g.emit(f, out)
	}
	for _, r := range releases {
		kind := ""
		switch {
		case r["isLatest"] == true:
			kind = "Latest"
		case r["isDraft"] == true:
			kind = "Draft"
		case r["isPrerelease"] == true:
			kind = "Pre-release"
		}
		published := text(r, "publishedAt")
		if published == "" {
			published = text(r, "createdAt")
		}
		fmt.Fprintf(&g.out, "%s\t%s\t%s\t%s\n", cmpOr(text(r, "name"), text(r, "tagName")), kind, text(r, "tagName"), published)
	}
	return nil
}

// fetchRelease reads the release of a tag, or the latest.
func (g *ghRun) fetchRelease(repo, tag string) (node, error) {
	path := "/repos/" + repo + "/releases/latest"
	if tag != "" {
		path = "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	resp := g.do(http.MethodGet, path, nil, nil, nil)
	switch {
	case resp.Outage:
		return nil, connectError()
	case resp.Status == http.StatusNotFound:
		return nil, errors.New("release not found")
	case resp.Status >= 300:
		return nil, httpError(resp, http.MethodGet, path)
	}
	var rel node
	if err := unmarshal(resp.Body, &rel); err != nil {
		return nil, err
	}
	return rel, nil
}

// releaseJSONFields are gh release view's --json fields.
var releaseJSONFields = []string{"apiUrl", "assets", "author", "body", "createdAt", "databaseId", "id", "isDraft", "isImmutable", "isPrerelease",
	"name", "publishedAt", "tagName", "tarballUrl", "targetCommitish", "uploadUrl", "url", "zipballUrl"}

func (g *ghRun) releaseView(args []string) error {
	f, err := parseFlags("release view", args, nil, append([]string{"repo|R"}, jsonFlags...))
	if err != nil {
		return err
	}
	if !f.has("json") {
		return unhandled("the fake gh translates gh release view with --json only")
	}
	asked, err := g.fields(f, releaseJSONFields)
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	tag := ""
	if len(f.pos) > 0 {
		tag = f.pos[0]
	}
	rel, err := g.fetchRelease(repo, tag)
	if err != nil {
		return err
	}
	rest := map[string]string{"apiUrl": "url", "body": "body", "createdAt": "created_at", "databaseId": "id", "id": "node_id", "isDraft": "draft",
		"isPrerelease": "prerelease", "name": "name", "publishedAt": "published_at", "tagName": "tag_name", "tarballUrl": "tarball_url",
		"targetCommitish": "target_commitish", "uploadUrl": "upload_url", "url": "html_url", "zipballUrl": "zipball_url"}
	out := node{}
	for _, field := range asked {
		switch field {
		case "isImmutable":
			out[field] = false
		case "author":
			a := child(rel, "author")
			out[field] = node{"id": text(a, "node_id"), "login": text(a, "login")}
		case "assets":
			assets := []any{}
			list, _ := rel["assets"].([]any)
			for _, e := range list {
				a, _ := e.(map[string]any)
				assets = append(assets, node{
					"apiUrl": text(a, "url"), "contentType": text(a, "content_type"), "createdAt": timeOr(a, "created_at"), "digest": nil,
					"downloadCount": a["download_count"], "id": text(a, "node_id"), "label": text(a, "label"), "name": text(a, "name"),
					"size": a["size"], "state": text(a, "state"), "updatedAt": timeOr(a, "updated_at"), "url": text(a, "browser_download_url"),
				})
			}
			out[field] = assets
		default:
			out[field] = rel[rest[field]]
		}
	}
	return g.emit(f, out)
}

// uploadAssets uploads files to a release; an entry may name its label
// after a #.
func (g *ghRun) uploadAssets(repo string, rel node, files []string, clobber bool) error {
	id := fmt.Sprintf("%v", rel["id"])
	for _, file := range files {
		file, _, _ = strings.Cut(file, "#")
		data, err := g.readFile(file)
		if err != nil {
			return err
		}
		name := filepath.Base(file)
		list, _ := rel["assets"].([]any)
		for _, e := range list {
			a, _ := e.(map[string]any)
			if text(a, "name") != name {
				continue
			}
			if !clobber {
				return fmt.Errorf("asset under the same name already exists: [%s]", name)
			}
			if err := g.rest(http.MethodDelete, fmt.Sprintf("/repos/%s/releases/assets/%v", repo, a["id"]), nil, nil, nil); err != nil {
				return err
			}
		}
		header := http.Header{}
		header.Set("Content-Type", "application/octet-stream")
		path := "/repos/" + repo + "/releases/" + id + "/assets"
		resp := g.do(http.MethodPost, path, url.Values{"name": {name}}, header, data)
		if resp.Outage {
			return connectError()
		}
		if resp.Status >= 300 {
			return httpError(resp, http.MethodPost, path)
		}
	}
	return nil
}

func (g *ghRun) releaseCreate(args []string) error {
	f, err := parseFlags("release create", args, []string{"draft|d", "prerelease|p", "latest", "verify-tag"},
		[]string{"repo|R", "title|t", "notes|n", "notes-file|F", "target"})
	if err != nil {
		return err
	}
	if len(f.pos) == 0 {
		return errors.New("tag required when not running interactively")
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	notes := f.str("notes")
	if file := f.str("notes-file"); file != "" {
		b, err := g.readFile(file)
		if err != nil {
			return err
		}
		notes = string(b)
	}
	in := map[string]any{"tag_name": f.pos[0], "name": f.str("title"), "body": notes, "draft": f.on("draft"), "prerelease": f.on("prerelease")}
	if t := f.str("target"); t != "" {
		in["target_commitish"] = t
	}
	var rel node
	if err := g.rest(http.MethodPost, "/repos/"+repo+"/releases", nil, in, &rel); err != nil {
		return err
	}
	if err := g.uploadAssets(repo, rel, f.pos[1:], false); err != nil {
		return err
	}
	fmt.Fprintln(&g.out, text(rel, "html_url"))
	return nil
}

func (g *ghRun) releaseDelete(args []string) error {
	f, err := parseFlags("release delete", args, []string{"yes|y", "cleanup-tag"}, []string{"repo|R"})
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return errors.New("accepts 1 arg(s), received " + strconv.Itoa(len(f.pos)))
	}
	if !f.on("yes") {
		return errors.New("--yes required when not running interactively")
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	rel, err := g.fetchRelease(repo, f.pos[0])
	if err != nil {
		return err
	}
	if err := g.rest(http.MethodDelete, fmt.Sprintf("/repos/%s/releases/%v", repo, rel["id"]), nil, nil, nil); err != nil {
		return err
	}
	if f.on("cleanup-tag") {
		return unhandled("the fake gh does not translate gh release delete --cleanup-tag")
	}
	return nil
}

func (g *ghRun) releaseUpload(args []string) error {
	f, err := parseFlags("release upload", args, []string{"clobber"}, []string{"repo|R"})
	if err != nil {
		return err
	}
	if len(f.pos) < 2 {
		return errors.New("requires at least 2 arg(s), only received " + strconv.Itoa(len(f.pos)))
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	rel, err := g.fetchRelease(repo, f.pos[0])
	if err != nil {
		return err
	}
	return g.uploadAssets(repo, rel, f.pos[1:], f.on("clobber"))
}

func (g *ghRun) releaseDownload(args []string) error {
	f, err := parseFlags("release download", args, []string{"clobber", "skip-existing"}, []string{"repo|R", "pattern|p", "dir|D", "output|O"})
	if err != nil {
		return err
	}
	repo, err := g.repo(f)
	if err != nil {
		return err
	}
	tag := ""
	if len(f.pos) > 0 {
		tag = f.pos[0]
	} else if !f.has("pattern") {
		return errors.New("`--pattern` or `--archive` is required when downloading the latest release")
	}
	rel, err := g.fetchRelease(repo, tag)
	if err != nil {
		return err
	}
	dir := cmpOr(f.str("dir"), ".")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(g.call.Dir, dir)
	}
	list, _ := rel["assets"].([]any)
	matched := 0
	for _, e := range list {
		a, _ := e.(map[string]any)
		name := text(a, "name")
		if patterns := f.all("pattern"); len(patterns) > 0 && !slices.ContainsFunc(patterns, func(p string) bool {
			ok, _ := filepath.Match(p, name)
			return ok
		}) {
			continue
		}
		matched++
		target := filepath.Join(dir, name)
		if out := f.str("output"); out != "" {
			target = out
			if !filepath.IsAbs(target) {
				target = filepath.Join(g.call.Dir, target)
			}
		}
		if _, err := os.Stat(target); err == nil && !f.on("clobber") {
			if f.on("skip-existing") {
				continue
			}
			return fmt.Errorf("%s already exists (use `--clobber` to overwrite file or `--skip-existing` to skip file)", target)
		}
		header := http.Header{}
		header.Set("Accept", "application/octet-stream")
		path := fmt.Sprintf("/repos/%s/releases/assets/%v", repo, a["id"])
		resp := g.do(http.MethodGet, path, nil, header, nil)
		if resp.Outage {
			return connectError()
		}
		if resp.Status != http.StatusOK {
			return httpError(resp, http.MethodGet, path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(target, resp.Body, 0o600); err != nil {
			return err
		}
	}
	if matched == 0 {
		return errors.New("no assets match the file pattern")
	}
	return nil
}
