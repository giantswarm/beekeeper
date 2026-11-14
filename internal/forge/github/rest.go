package github

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// splitRepo splits owner/repo.
func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("invalid repo %q (want owner/repo)", repo)
	}
	return owner, name, nil
}

// splitRef splits owner/repo#n.
func splitRef(ref string) (repo string, number int, err error) {
	repo, n, ok := strings.Cut(ref, "#")
	if !ok {
		return "", 0, fmt.Errorf("invalid ref %q (want owner/repo#n)", ref)
	}
	if _, _, err := splitRepo(repo); err != nil {
		return "", 0, err
	}
	number, err = strconv.Atoi(n)
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("invalid ref %q (want owner/repo#n)", ref)
	}
	return repo, number, nil
}

func repoPath(repo string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

func badInput(op string, err error) error {
	return &forge.Error{Kind: forge.Refused, API: forge.APIREST, Operation: op, Err: err}
}

// Issue reads an issue (issues/get). Its links (parent, sub-issues, blockers, closing PRs) come
// with the board queries, not with this read.
func (c *Client) Issue(ctx context.Context, ref string, etag string) (forge.Issue, forge.Meta, error) {
	repo, n, err := splitRef(ref)
	if err != nil {
		return forge.Issue{}, forge.Meta{}, badInput(forge.OpIssuesGet, err)
	}
	rp, _ := repoPath(repo)
	var w wire.Issue
	meta, _, err := c.get(ctx, forge.APIREST, forge.OpIssuesGet, fmt.Sprintf("%s/issues/%d", rp, n), etag, &w)
	if err != nil || meta.NotModified {
		return forge.Issue{}, meta, err
	}
	is := forge.Issue{
		Ref:         ref,
		NodeID:      w.NodeID,
		Title:       w.Title,
		Body:        w.Body,
		State:       w.State,
		StateReason: w.StateReason,
		CreatedAt:   w.CreatedAt,
		UpdatedAt:   w.UpdatedAt,
	}
	for _, a := range w.Assignees {
		is.Assignees = append(is.Assignees, a.Login)
	}
	for _, l := range w.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is, meta, nil
}

// Repo reads a repository (repos/get).
func (c *Client) Repo(ctx context.Context, repo string, etag string) (forge.Repo, forge.Meta, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return forge.Repo{}, forge.Meta{}, badInput(forge.OpReposGet, err)
	}
	var w wire.Repository
	meta, _, err := c.get(ctx, forge.APIREST, forge.OpReposGet, rp, etag, &w)
	if err != nil || meta.NotModified {
		return forge.Repo{}, meta, err
	}
	return forge.Repo{FullName: w.FullName, Private: w.Private, DefaultBranch: w.DefaultBranch, Archived: w.Archived}, meta, nil
}

// PullRequest reads a PR (pulls/get).
func (c *Client) PullRequest(ctx context.Context, ref string, etag string) (forge.PullRequest, forge.Meta, error) {
	repo, n, err := splitRef(ref)
	if err != nil {
		return forge.PullRequest{}, forge.Meta{}, badInput(forge.OpPullsGet, err)
	}
	rp, _ := repoPath(repo)
	var w wire.PullRequest
	meta, _, err := c.get(ctx, forge.APIREST, forge.OpPullsGet, fmt.Sprintf("%s/pulls/%d", rp, n), etag, &w)
	if err != nil || meta.NotModified {
		return forge.PullRequest{}, meta, err
	}
	pr := forge.PullRequest{
		Ref:       ref,
		NodeID:    w.NodeID,
		State:     w.State,
		Merged:    w.Merged,
		Draft:     w.Draft,
		Author:    w.User.Login,
		HeadSHA:   w.Head.SHA,
		Base:      w.Base.Ref,
		MergeSHA:  w.MergeCommitSHA,
		UpdatedAt: w.UpdatedAt,
	}
	if w.MergedAt != nil {
		pr.MergedAt = *w.MergedAt
		pr.Merged = true
	}
	if !pr.Merged {
		// GitHub reports a test-merge commit for open PRs; only a merge's commit is the merge SHA.
		pr.MergeSHA = ""
	}
	return pr, meta, nil
}

// list reads every page of a REST list. Only the first page is conditional: when it is unchanged,
// the list is.
func (c *Client) list(ctx context.Context, op, path, etag string, page func(*answer) (int, error)) (forge.Meta, error) {
	meta, a, err := c.get(ctx, forge.APIREST, op, path, etag, nil)
	if err != nil || meta.NotModified {
		return meta, err
	}
	for i := 0; ; i++ {
		if _, err := page(a); err != nil {
			return forge.Meta{}, &forge.Error{Kind: forge.Transient, API: forge.APIREST, Operation: op, Code: a.code,
				Err: fmt.Errorf("decoding the answer: %w", err)}
		}
		next := c.nextPage(a.header)
		if next == "" || i >= maxPages {
			return meta, nil
		}
		if _, a, err = c.get(ctx, forge.APIREST, op, next, "", nil); err != nil {
			return forge.Meta{}, err
		}
	}
}

// Reviews lists a PR's reviews (pulls/list-reviews).
func (c *Client) Reviews(ctx context.Context, ref string, etag string) ([]forge.Review, forge.Meta, error) {
	repo, n, err := splitRef(ref)
	if err != nil {
		return nil, forge.Meta{}, badInput(forge.OpPullsListReviews, err)
	}
	rp, _ := repoPath(repo)
	var out []forge.Review
	meta, err := c.list(ctx, forge.OpPullsListReviews, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", rp, n), etag,
		func(a *answer) (int, error) {
			var ws []wire.Review
			if err := jsonUnmarshal(a.body, &ws); err != nil {
				return 0, err
			}
			for _, w := range ws {
				out = append(out, forge.Review{By: w.User.Login, State: strings.ToUpper(w.State), SubmittedAt: w.SubmittedAt})
			}
			return len(ws), nil
		})
	if err != nil || meta.NotModified {
		return nil, meta, err
	}
	return out, meta, nil
}

// CheckRuns lists the check runs on a commit (checks/list-for-ref).
func (c *Client) CheckRuns(ctx context.Context, repo, sha string, etag string) ([]forge.CheckRun, forge.Meta, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, forge.Meta{}, badInput(forge.OpChecksListForRef, err)
	}
	var out []forge.CheckRun
	meta, err := c.list(ctx, forge.OpChecksListForRef, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", rp, url.PathEscape(sha)), etag,
		func(a *answer) (int, error) {
			var w wire.CheckRunList
			if err := jsonUnmarshal(a.body, &w); err != nil {
				return 0, err
			}
			for _, r := range w.CheckRuns {
				cr := forge.CheckRun{Name: r.Name, Status: r.Status, Conclusion: r.Conclusion}
				if r.StartedAt != nil {
					cr.StartedAt = *r.StartedAt
				}
				if r.CompletedAt != nil {
					cr.CompletedAt = *r.CompletedAt
				}
				out = append(out, cr)
			}
			return len(w.CheckRuns), nil
		})
	if err != nil || meta.NotModified {
		return nil, meta, err
	}
	return out, meta, nil
}

// Statuses lists a commit's statuses (repos/list-commit-statuses-for-ref), the latest per context.
func (c *Client) Statuses(ctx context.Context, repo, sha string, etag string) ([]forge.CommitStatus, forge.Meta, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, forge.Meta{}, badInput(forge.OpStatusesListForRef, err)
	}
	latest := map[string]forge.CommitStatus{}
	meta, err := c.list(ctx, forge.OpStatusesListForRef, fmt.Sprintf("%s/commits/%s/statuses?per_page=100", rp, url.PathEscape(sha)), etag,
		func(a *answer) (int, error) {
			var ws []wire.Status
			if err := jsonUnmarshal(a.body, &ws); err != nil {
				return 0, err
			}
			for _, w := range ws {
				// Newest first: keep the first of each context, unless a later page has a newer one.
				if cur, ok := latest[w.Context]; ok && !w.CreatedAt.After(cur.CreatedAt) {
					continue
				}
				latest[w.Context] = forge.CommitStatus{Context: w.Context, State: w.State, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
			}
			return len(ws), nil
		})
	if err != nil || meta.NotModified {
		return nil, meta, err
	}
	out := make([]forge.CommitStatus, 0, len(latest))
	for _, s := range latest {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context < out[j].Context })
	return out, meta, nil
}

// BranchRules reads the rules that apply to a branch (repos/get-branch-rules).
func (c *Client) BranchRules(ctx context.Context, repo, branch string, etag string) (forge.BranchRules, forge.Meta, error) {
	rp, err := repoPath(repo)
	if err != nil || branch == "" {
		if err == nil {
			err = fmt.Errorf("no branch given for %s", repo)
		}
		return forge.BranchRules{}, forge.Meta{}, badInput(forge.OpReposGetBranchRules, err)
	}
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	var br forge.BranchRules
	seen := map[string]bool{}
	meta, err := c.list(ctx, forge.OpReposGetBranchRules, fmt.Sprintf("%s/rules/branches/%s?per_page=100", rp, strings.Join(segs, "/")), etag,
		func(a *answer) (int, error) {
			var rules []wire.BranchRule
			if err := jsonUnmarshal(a.body, &rules); err != nil {
				return 0, err
			}
			for _, r := range rules {
				switch r.Type {
				case wire.RuleRequiredStatusChecks:
					var p wire.RequiredStatusChecksParams
					if err := jsonUnmarshal(r.Parameters, &p); err != nil {
						return 0, err
					}
					for _, rc := range p.RequiredStatusChecks {
						if !seen[rc.Context] {
							seen[rc.Context] = true
							br.RequiredChecks = append(br.RequiredChecks, rc.Context)
						}
					}
				case wire.RulePullRequest:
					var p wire.PullRequestRuleParams
					if err := jsonUnmarshal(r.Parameters, &p); err != nil {
						return 0, err
					}
					if p.RequiredApprovingReviewCount > br.RequiredApprovals {
						br.RequiredApprovals = p.RequiredApprovingReviewCount
					}
				}
			}
			return len(rules), nil
		})
	if err != nil || meta.NotModified {
		return forge.BranchRules{}, meta, err
	}
	sort.Strings(br.RequiredChecks)
	return br, meta, nil
}

// InstallationRepos lists the repos the installation covers
// (apps/list-repos-accessible-to-installation).
func (c *Client) InstallationRepos(ctx context.Context, etag string) ([]string, forge.Meta, error) {
	var out []string
	meta, err := c.list(ctx, forge.OpListInstallationRepos, "/installation/repositories?per_page=100", etag,
		func(a *answer) (int, error) {
			var w wire.InstallationRepositories
			if err := jsonUnmarshal(a.body, &w); err != nil {
				return 0, err
			}
			for _, r := range w.Repositories {
				out = append(out, r.FullName)
			}
			return len(w.Repositories), nil
		})
	if err != nil || meta.NotModified {
		return nil, meta, err
	}
	sort.Strings(out)
	return out, meta, nil
}
