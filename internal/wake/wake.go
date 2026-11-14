// Package wake maps webhook deliveries to wake refs (contract §1.3) and defines the refs Items and
// Changes answer to, which the engine keeps in a cache field index (the wake index). A delivery only
// wakes the engine: nothing in its payload is trusted beyond the ids that say what to read again.
package wake

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
)

// Ref prefixes.
const (
	PrefixIssue = "issue:"
	PrefixPR    = "pr:"
	PrefixCI    = "ci:"
	PrefixBoard = "board:"
)

// Field index names, registered on the manager's cache.
const (
	// IndexRefs indexes Items and Changes by every ref they answer to: their wait's ref and the refs
	// they own (an issue's Item, a board node's Item, a PR's Change, its head and merge commits).
	IndexRefs = "wake.refs"
	// IndexChanges indexes Items by the PR refs in status.changes (a Change's Items).
	IndexChanges = "wake.changes"
)

// Issue is the ref of an issue (owner/repo#n).
func Issue(ref string) string { return PrefixIssue + ref }

// PR is the ref of a pull request (owner/repo#n).
func PR(ref string) string { return PrefixPR + ref }

// CI is the ref of a commit's checks (owner/repo, sha).
func CI(repo, sha string) string { return PrefixCI + repo + "@" + sha }

// Board is the ref of a board item (its node id).
func Board(node string) string { return PrefixBoard + node }

// Split returns a ref's prefix (without the colon) and value; ok is false for an unknown prefix.
func Split(ref string) (kind, value string, ok bool) {
	for _, p := range []string{PrefixIssue, PrefixPR, PrefixCI, PrefixBoard} {
		if strings.HasPrefix(ref, p) {
			return strings.TrimSuffix(p, ":"), strings.TrimPrefix(ref, p), true
		}
	}
	return "", "", false
}

// SplitCI returns the repo and SHA of a ci: ref.
func SplitCI(ref string) (repo, sha string, ok bool) {
	v, found := strings.CutPrefix(ref, PrefixCI)
	if !found {
		return "", "", false
	}
	repo, sha, ok = strings.Cut(v, "@")
	return repo, sha, ok && repo != "" && sha != ""
}

// ItemRef is the issue ref of an Item (owner/repo#n), or "" before it was observed.
func ItemRef(it *v1alpha1.Item) string {
	if is := it.Status.Issue; is != nil && is.Repo != "" && is.Number > 0 {
		return is.Repo + "#" + strconv.Itoa(is.Number)
	}
	return ""
}

// ChangeRef is the PR ref of a Change (owner/repo#n), or "" before it was observed.
func ChangeRef(c *v1alpha1.Change) string {
	if pr := c.Status.PR; pr != nil && pr.Repo != "" && pr.Number > 0 {
		return pr.Repo + "#" + strconv.Itoa(pr.Number)
	}
	return ""
}

// ItemRefs are the refs an Item answers to: its issue, its board node and its wait's ref.
func ItemRefs(it *v1alpha1.Item) []string {
	var refs []string
	if r := ItemRef(it); r != "" {
		refs = append(refs, Issue(r))
	}
	if b := it.Status.Board; b != nil && b.NodeID != "" {
		refs = append(refs, Board(b.NodeID))
	}
	if w := it.Status.Wait; w != nil && w.Ref != "" {
		refs = append(refs, w.Ref)
	}
	return dedup(refs)
}

// ChangeRefs are the refs a Change answers to: its PR, its head and merge commits, and its wait's ref.
func ChangeRefs(c *v1alpha1.Change) []string {
	var refs []string
	ref := ChangeRef(c)
	if ref != "" {
		refs = append(refs, PR(ref))
	}
	if pr := c.Status.PR; pr != nil && pr.Repo != "" {
		if pr.HeadSHA != "" {
			refs = append(refs, CI(pr.Repo, pr.HeadSHA))
		}
		if pr.MergeSHA != "" {
			refs = append(refs, CI(pr.Repo, pr.MergeSHA))
		}
	}
	if w := c.Status.Wait; w != nil && w.Ref != "" {
		refs = append(refs, w.Ref)
	}
	return dedup(refs)
}

func dedup(refs []string) []string {
	if len(refs) < 2 {
		return refs
	}
	sort.Strings(refs)
	out := refs[:1]
	for _, r := range refs[1:] {
		if r != out[len(out)-1] {
			out = append(out, r)
		}
	}
	return out
}

// Delivery is what a webhook delivery names, read from ids only.
type Delivery struct {
	Event  string
	Action string
	// Refs are the wake refs: the Items and Changes owning or waiting on them are reconciled.
	Refs []string
	// Repo is the repository the delivery is about (owner/repo), when it has one.
	Repo string
	// ProjectNode is the board (ProjectV2) node id of a projects_v2_item delivery: deliveries for other
	// boards are ignored.
	ProjectNode string
	// BoardItem is the board item node id of a projects_v2_item delivery. When no Item owns it, the
	// engine resolves it by reading the board item.
	BoardItem string
	// ContentType of a projects_v2_item delivery: Issue, PullRequest or DraftIssue.
	ContentType string
	// LinkPR is a PR whose closing references may have changed (opened, edited, reopened): the engine
	// reads the issues it closes and re-reads those Items and the ones that listed it.
	LinkPR string
	// InstallationRepos is true for installation_repositories: the installation's repos changed.
	InstallationRepos bool
	ReposAdded        []string
	ReposRemoved      []string
	// Ignore is true for deliveries that name nothing to read (ping, unknown events).
	Ignore bool
}

// Webhook events the engine subscribes to.
const (
	eventProjectsV2Item    = "projects_v2_item"
	eventIssues            = "issues"
	eventSubIssues         = "sub_issues"
	eventIssueDependencies = "issue_dependencies"
	eventPullRequest       = "pull_request"
	eventPullRequestReview = "pull_request_review"
	eventCheckRun          = "check_run"
	eventCheckSuite        = "check_suite"
	eventStatus            = "status"
	eventPush              = "push"
	eventInstallationRepos = "installation_repositories"
)

// Subscribed are the events the engine subscribes to (contract §1.3).
var Subscribed = map[string]bool{
	eventProjectsV2Item: true, eventIssues: true, eventSubIssues: true, eventIssueDependencies: true,
	eventPullRequest: true, eventPullRequestReview: true, eventCheckRun: true, eventCheckSuite: true,
	eventStatus: true, eventPush: true, eventInstallationRepos: true,
}

// payload is the subset of every delivery the mapping reads: ids only.
type payload struct {
	Action     string    `json:"action"`
	Repository *repoObj  `json:"repository"`
	Issue      *issueObj `json:"issue"`

	PullRequest *struct {
		Number int     `json:"number"`
		Base   *refObj `json:"base"`
		URL    string  `json:"url"`
	} `json:"pull_request"`

	ProjectsV2Item *struct {
		NodeID        string `json:"node_id"`
		ProjectNodeID string `json:"project_node_id"`
		ContentNodeID string `json:"content_node_id"`
		ContentType   string `json:"content_type"`
	} `json:"projects_v2_item"`

	// sub_issues
	ParentIssue     *issueObj `json:"parent_issue"`
	ParentIssueRepo *repoObj  `json:"parent_issue_repo"`
	SubIssue        *issueObj `json:"sub_issue"`
	SubIssueRepo    *repoObj  `json:"sub_issue_repo"`
	// issue_dependencies
	BlockedIssue     *issueObj `json:"blocked_issue"`
	BlockedIssueRepo *repoObj  `json:"blocked_issue_repo"`
	BlockingIssue    *issueObj `json:"blocking_issue"`
	BlockingIssueRep *repoObj  `json:"blocking_issue_repo"`

	CheckRun *struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_run"`
	CheckSuite *struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_suite"`
	SHA   string `json:"sha"`   // status
	After string `json:"after"` // push

	RepositoriesAdded   []repoObj `json:"repositories_added"`
	RepositoriesRemoved []repoObj `json:"repositories_removed"`
}

type repoObj struct {
	FullName string `json:"full_name"`
}

type refObj struct {
	Repo *repoObj `json:"repo"`
}

type issueObj struct {
	Number        int    `json:"number"`
	RepositoryURL string `json:"repository_url"`
	HTMLURL       string `json:"html_url"`
	URL           string `json:"url"`
}

// Map reads the refs a delivery names. event is the X-GitHub-Event header; the action is read from the
// payload. Unknown events and ping are returned with Ignore set.
func Map(event string, body []byte) (Delivery, error) {
	d := Delivery{Event: event}
	if !Subscribed[event] {
		d.Ignore = true
		return d, nil
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return d, fmt.Errorf("decode %s payload: %w", event, err)
	}
	d.Action = p.Action
	if p.Repository != nil {
		d.Repo = p.Repository.FullName
	}
	switch event {
	case eventProjectsV2Item:
		it := p.ProjectsV2Item
		if it == nil || it.NodeID == "" {
			return d, fmt.Errorf("projects_v2_item without node_id")
		}
		d.ProjectNode, d.BoardItem, d.ContentType = it.ProjectNodeID, it.NodeID, it.ContentType
		if d.Action == "reordered" {
			// Reordering changes nothing the engine observes.
			d.Ignore = true
			return d, nil
		}
		d.Refs = []string{Board(it.NodeID)}
	case eventIssues:
		if r := issueRef(p.Issue, nil, d.Repo); r != "" {
			d.Refs = []string{Issue(r)}
		}
	case eventSubIssues:
		d.Refs = issueRefs(d.Repo, pair{p.ParentIssue, p.ParentIssueRepo}, pair{p.SubIssue, p.SubIssueRepo})
	case eventIssueDependencies:
		d.Refs = issueRefs(d.Repo, pair{p.BlockedIssue, p.BlockedIssueRepo}, pair{p.BlockingIssue, p.BlockingIssueRep},
			pair{p.Issue, nil})
	case eventPullRequest, eventPullRequestReview:
		if p.PullRequest == nil || p.PullRequest.Number <= 0 {
			return d, fmt.Errorf("%s without pull_request.number", event)
		}
		repo := d.Repo
		if repo == "" && p.PullRequest.Base != nil && p.PullRequest.Base.Repo != nil {
			repo = p.PullRequest.Base.Repo.FullName
		}
		if repo == "" {
			repo = repoFromURL(p.PullRequest.URL)
		}
		if repo == "" {
			return d, fmt.Errorf("%s without repository", event)
		}
		d.Repo = repo
		ref := repo + "#" + strconv.Itoa(p.PullRequest.Number)
		d.Refs = []string{PR(ref)}
		if event == eventPullRequest {
			switch d.Action {
			case "opened", "edited", "reopened":
				d.LinkPR = ref
			}
		}
	case eventCheckRun:
		if p.CheckRun != nil {
			d.Refs = ciRefs(d.Repo, p.CheckRun.HeadSHA)
		}
	case eventCheckSuite:
		if p.CheckSuite != nil {
			d.Refs = ciRefs(d.Repo, p.CheckSuite.HeadSHA)
		}
	case eventStatus:
		d.Refs = ciRefs(d.Repo, p.SHA)
	case eventPush:
		if p.After != "" && strings.Trim(p.After, "0") != "" { // a deleted branch's after is all zeros
			d.Refs = ciRefs(d.Repo, p.After)
		}
	case eventInstallationRepos:
		d.InstallationRepos = true
		for _, r := range p.RepositoriesAdded {
			d.ReposAdded = append(d.ReposAdded, r.FullName)
		}
		for _, r := range p.RepositoriesRemoved {
			d.ReposRemoved = append(d.ReposRemoved, r.FullName)
		}
	}
	return d, nil
}

type pair struct {
	issue *issueObj
	repo  *repoObj
}

func issueRefs(fallback string, pairs ...pair) []string {
	var refs []string
	for _, p := range pairs {
		if r := issueRef(p.issue, p.repo, fallback); r != "" {
			refs = append(refs, Issue(r))
		}
	}
	return dedup(refs)
}

// issueRef builds owner/repo#n from an issue object: its own repo object, its API or HTML URL, else the
// delivery's repository.
func issueRef(is *issueObj, repo *repoObj, fallback string) string {
	if is == nil || is.Number <= 0 {
		return ""
	}
	name := ""
	switch {
	case repo != nil && repo.FullName != "":
		name = repo.FullName
	case is.RepositoryURL != "":
		name = repoFromURL(is.RepositoryURL)
	case is.URL != "":
		name = repoFromURL(is.URL)
	case is.HTMLURL != "":
		name = repoFromURL(is.HTMLURL)
	}
	if name == "" {
		name = fallback
	}
	if name == "" {
		return ""
	}
	return name + "#" + strconv.Itoa(is.Number)
}

// repoFromURL reads owner/repo from https://api.github.com/repos/owner/repo[/...] or
// https://github.com/owner/repo[/...].
func repoFromURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Path == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		parts = parts[1:]
	}
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func ciRefs(repo, sha string) []string {
	if repo == "" || sha == "" {
		return nil
	}
	return []string{CI(repo, sha)}
}
