//go:build scenario

package fakegithub

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The JSON of REST's resources, with the fields beekeeper, gh and
// go-github read.

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func tsp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func lower(s string) any {
	if s == "" {
		return nil
	}
	return strings.ToLower(s)
}

func (c *restCtx) repoURL(r *Repo) string { return c.base + "/repos/" + r.Full() }

func (c *restCtx) user(login string) any {
	if login == "" {
		return nil
	}
	a := c.m.Account(login, "")
	return obj{
		"login": a.Login, "id": a.ID, "node_id": a.NodeID, "type": a.Kind, "site_admin": false,
		"html_url": webHost + "/" + a.Login, "url": c.base + "/users/" + a.Login,
		"avatar_url": fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", a.ID),
	}
}

func (c *restCtx) users(logins []string) []any {
	out := []any{}
	for _, l := range logins {
		out = append(out, c.user(l))
	}
	return out
}

func (c *restCtx) repoShape(r *Repo) obj {
	visibility := "public"
	if r.Private {
		visibility = "private"
	}
	open := 0
	for _, i := range r.Issues {
		if i.State == stateOpen {
			open++
		}
	}
	for _, p := range r.Pulls {
		if p.State == stateOpen {
			open++
		}
	}
	var description any
	if r.Description != "" {
		description = r.Description
	}
	return obj{
		"id": r.ID, "node_id": r.NodeID, "name": r.Name, "full_name": r.Full(), "private": r.Private, "visibility": visibility,
		"owner": c.user(r.Owner), "html_url": r.URL(), "url": c.repoURL(r), "description": description,
		"fork": false, "archived": false, "disabled": false, "default_branch": r.DefaultBranch,
		"created_at": ts(r.CreatedAt), "updated_at": ts(r.PushedAt), "pushed_at": ts(r.PushedAt),
		"clone_url": r.URL() + ".git", "ssh_url": "git@github.com:" + r.Full() + ".git",
		"has_issues": true, "has_projects": true, "open_issues_count": open, "open_issues": open,
		"allow_squash_merge": true, "allow_merge_commit": true, "allow_rebase_merge": true, "allow_auto_merge": true,
		"delete_branch_on_merge": false,
		"permissions":            obj{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
	}
}

func (c *restCtx) labelShape(r *Repo) func(l *Label) obj {
	return func(l *Label) obj {
		return obj{
			"id": l.ID, "node_id": l.NodeID, "name": l.Name, "color": l.Color, "default": false, "description": l.Description,
			"url": c.repoURL(r) + "/labels/" + url.PathEscape(l.Name),
		}
	}
}

func (c *restCtx) labelShapes(r *Repo, names []string) []any {
	out := []any{}
	shape := c.labelShape(r)
	for _, n := range names {
		out = append(out, shape(c.m.EnsureLabel(r, n, "", "")))
	}
	return out
}

func (c *restCtx) association(r *Repo, login string) string {
	switch {
	case strings.EqualFold(login, r.Owner):
		return "OWNER"
	case strings.EqualFold(login, c.m.Viewer):
		return "MEMBER"
	}
	return "CONTRIBUTOR"
}

func (c *restCtx) issueShape(i *Issue) obj {
	r := i.Repo
	done := 0
	for _, s := range i.Subs {
		if s.State == stateClosed {
			done++
		}
	}
	percent := 0
	if len(i.Subs) > 0 {
		percent = done * 100 / len(i.Subs)
	}
	var assignee any
	if len(i.Assignees) > 0 {
		assignee = c.user(i.Assignees[0])
	}
	api := fmt.Sprintf("%s/issues/%d", c.repoURL(r), i.Number)
	out := obj{
		"id": i.ID, "node_id": i.NodeID, "number": i.Number, "title": i.Title, "body": i.Body,
		"state": strings.ToLower(i.State), "state_reason": lower(i.StateReason), "locked": false,
		"user": c.user(i.Author), "labels": c.labelShapes(r, i.Labels), "assignees": c.users(i.Assignees), "assignee": assignee,
		"comments": len(i.Comments), "created_at": ts(i.CreatedAt), "updated_at": ts(i.UpdatedAt), "closed_at": tsp(i.ClosedAt),
		"html_url": i.URL(), "url": api, "repository_url": c.repoURL(r), "comments_url": api + "/comments",
		"author_association":         c.association(r, i.Author),
		"sub_issues_summary":         obj{"total": len(i.Subs), "completed": done, "percent_completed": percent},
		"milestone":                  nil,
		"active_lock_reason":         nil,
		"issue_dependencies_summary": obj{"blocked_by": len(i.BlockedBy), "total_blocked_by": len(i.BlockedBy)},
	}
	if i.Parent != nil {
		out["parent_issue_url"] = fmt.Sprintf("%s/issues/%d", c.repoURL(i.Parent.Repo), i.Parent.Number)
	}
	return out
}

// pullAsIssue is a pull request as the issues API shows it.
func (c *restCtx) pullAsIssue(p *Pull) obj {
	r := p.Repo
	state := "open"
	if p.State != stateOpen {
		state = "closed"
	}
	api := fmt.Sprintf("%s/issues/%d", c.repoURL(r), p.Number)
	return obj{
		"id": p.ID, "node_id": p.NodeID, "number": p.Number, "title": p.Title, "body": p.Body, "state": state, "state_reason": nil,
		"locked": false, "user": c.user(p.Author), "labels": c.labelShapes(r, p.Labels), "assignees": c.users(p.Assignees), "assignee": nil,
		"comments": len(p.Comments), "created_at": ts(p.CreatedAt), "updated_at": ts(p.UpdatedAt), "closed_at": tsp(p.ClosedAt),
		"html_url": p.URL(), "url": api, "repository_url": c.repoURL(r), "comments_url": api + "/comments",
		"author_association": c.association(r, p.Author), "draft": p.Draft, "milestone": nil,
		"pull_request": obj{
			"url": fmt.Sprintf("%s/pulls/%d", c.repoURL(r), p.Number), "html_url": p.URL(),
			"diff_url": p.URL() + ".diff", "patch_url": p.URL() + ".patch", "merged_at": tsp(p.MergedAt),
		},
	}
}

func (c *restCtx) pullShape(p *Pull) obj {
	r := p.Repo
	state := "open"
	if p.State != stateOpen {
		state = "closed"
	}
	side := func(branch, sha string) obj {
		return obj{"label": r.Owner + ":" + branch, "ref": branch, "sha": sha, "user": c.user(r.Owner), "repo": c.repoShape(r)}
	}
	var mergeable any
	switch c.m.MergeableState(p) {
	case "MERGEABLE":
		mergeable = true
	case "CONFLICTING":
		mergeable = false
	}
	var merge any
	if p.MergeCommit != "" {
		merge = p.MergeCommit
	}
	var auto any
	if p.AutoMerge != "" {
		auto = obj{"enabled_by": c.user(c.m.Viewer), "merge_method": strings.ToLower(p.AutoMerge), "commit_title": "", "commit_message": ""}
	}
	files := c.m.PullFiles(p)
	add, del := 0, 0
	for _, f := range files {
		add, del = add+f.Additions, del+f.Deletions
	}
	api := fmt.Sprintf("%s/pulls/%d", c.repoURL(r), p.Number)
	return obj{
		"id": p.ID, "node_id": p.NodeID, "number": p.Number, "title": p.Title, "body": p.Body, "state": state, "locked": false,
		"draft": p.Draft, "user": c.user(p.Author), "html_url": p.URL(), "url": api,
		"issue_url": fmt.Sprintf("%s/issues/%d", c.repoURL(r), p.Number), "diff_url": p.URL() + ".diff", "patch_url": p.URL() + ".patch",
		"created_at": ts(p.CreatedAt), "updated_at": ts(p.UpdatedAt), "closed_at": tsp(p.ClosedAt), "merged_at": tsp(p.MergedAt),
		"merge_commit_sha": merge, "head": side(p.Head, c.m.HeadSHA(p)), "base": side(p.Base, c.m.BaseSHA(p)),
		"labels": c.labelShapes(r, p.Labels), "assignees": c.users(p.Assignees), "requested_reviewers": []any{}, "requested_teams": []any{},
		"merged": p.State == stateMerged, "mergeable": mergeable, "mergeable_state": strings.ToLower(c.m.MergeStateStatus(p)),
		"rebaseable": mergeable, "merged_by": c.user(p.MergedBy), "comments": len(p.Comments), "review_comments": 0,
		"commits": len(c.m.PullCommits(p)), "additions": add, "deletions": del, "changed_files": len(files),
		"auto_merge": auto, "author_association": c.association(r, p.Author), "maintainer_can_modify": false, "milestone": nil,
	}
}

func (c *restCtx) commentShape(cm *Comment) obj {
	r, number, page := cm.pullOrIssue()
	return obj{
		"id": cm.ID, "node_id": cm.NodeID, "body": cm.Body, "user": c.user(cm.Author),
		"created_at": ts(cm.CreatedAt), "updated_at": ts(cm.UpdatedAt), "html_url": cm.URL(),
		"url":                      fmt.Sprintf("%s/issues/comments/%d", c.repoURL(r), cm.ID),
		"issue_url":                fmt.Sprintf("%s/issues/%d", c.repoURL(r), number),
		"author_association":       c.association(r, cm.Author),
		"performed_via_github_app": nil,
		"reactions":                obj{"total_count": 0, "url": page},
	}
}

// pullOrIssue is the comment's repository, its thread's number and page.
func (cm *Comment) pullOrIssue() (*Repo, int, string) {
	if cm.issue != nil {
		return cm.issue.Repo, cm.issue.Number, cm.issue.URL()
	}
	return cm.pull.Repo, cm.pull.Number, cm.pull.URL()
}

func (c *restCtx) reviewShape(rv *Review) obj {
	p := rv.pull
	return obj{
		"id": rv.ID, "node_id": rv.NodeID, "user": c.user(rv.Author), "body": rv.Body, "state": rv.State,
		"html_url": rv.URL(), "pull_request_url": fmt.Sprintf("%s/pulls/%d", c.repoURL(p.Repo), p.Number),
		"commit_id": rv.Commit, "submitted_at": ts(rv.SubmittedAt), "author_association": c.association(p.Repo, rv.Author),
	}
}

func (c *restCtx) checkShape(ch *CheckRun) obj {
	r := ch.repo
	return obj{
		"id": ch.ID, "node_id": ch.NodeID, "head_sha": ch.SHA, "name": ch.Name, "external_id": "",
		"status": strings.ToLower(ch.Status), "conclusion": lower(ch.Conclusion),
		"started_at": ts(ch.StartedAt), "completed_at": tsp(ch.CompletedAt),
		"html_url": ch.URL(), "details_url": ch.URL(), "url": fmt.Sprintf("%s/check-runs/%d", c.repoURL(r), ch.ID),
		"output":        obj{"title": nil, "summary": nil, "text": nil, "annotations_count": 0, "annotations_url": fmt.Sprintf("%s/check-runs/%d/annotations", c.repoURL(r), ch.ID)},
		"check_suite":   obj{"id": ch.Run.ID},
		"app":           obj{"id": 15368, "slug": "github-actions", "name": "GitHub Actions"},
		"pull_requests": []any{},
	}
}

func (c *restCtx) statusShape(r *Repo) func(s *Status) obj {
	return func(s *Status) obj {
		var target any
		if s.TargetURL != "" {
			target = s.TargetURL
		}
		return obj{
			"id": s.ID, "node_id": s.NodeID, "state": strings.ToLower(s.State), "context": s.Context, "description": s.Description,
			"target_url": target, "created_at": ts(s.CreatedAt), "updated_at": ts(s.CreatedAt),
			"url": fmt.Sprintf("%s/statuses/%s", c.repoURL(r), s.SHA), "creator": c.user(c.m.Viewer),
		}
	}
}

func (c *restCtx) runShape(run *WorkflowRun) obj {
	r := run.repo
	api := fmt.Sprintf("%s/actions/runs/%d", c.repoURL(r), run.ID)
	var branch any
	if run.Branch != "" {
		branch = run.Branch
	}
	return obj{
		"id": run.ID, "node_id": fmt.Sprintf("WFR_%d", run.ID), "name": run.Workflow.Name, "display_title": run.Workflow.Name,
		"path": run.Workflow.Path, "head_branch": branch, "head_sha": run.SHA, "run_number": run.Number, "run_attempt": run.Attempt,
		"event": run.Event, "status": run.Status, "conclusion": lower(run.Conclusion), "workflow_id": run.Workflow.ID,
		"check_suite_id": run.ID, "url": api, "html_url": run.URL(), "jobs_url": api + "/jobs", "logs_url": api + "/logs",
		"created_at": ts(run.CreatedAt), "updated_at": ts(run.UpdatedAt), "run_started_at": ts(run.CreatedAt),
		"actor": c.user(c.m.Viewer), "triggering_actor": c.user(c.m.Viewer), "head_commit": obj{"id": run.SHA},
		"repository": obj{"id": r.ID, "name": r.Name, "full_name": r.Full()}, "pull_requests": []any{},
	}
}

func (c *restCtx) assetShape(rel *Release) func(a *Asset) obj {
	return func(a *Asset) obj {
		return obj{
			"id": a.ID, "node_id": a.NodeID, "name": a.Name, "label": "", "content_type": a.ContentType, "state": "uploaded",
			"size": a.Size(), "download_count": 0, "created_at": ts(a.CreatedAt), "updated_at": ts(a.CreatedAt),
			"browser_download_url": fmt.Sprintf("%s/releases/download/%s/%s", rel.repo.URL(), rel.Tag, a.Name),
			"url":                  fmt.Sprintf("%s/releases/assets/%d", c.repoURL(rel.repo), a.ID),
			"uploader":             c.user(rel.Author),
		}
	}
}

func (c *restCtx) releaseShape(rel *Release) obj {
	api := fmt.Sprintf("%s/releases/%d", c.repoURL(rel.repo), rel.ID)
	assets := []any{}
	shape := c.assetShape(rel)
	for _, a := range rel.Assets {
		assets = append(assets, shape(a))
	}
	return obj{
		"id": rel.ID, "node_id": rel.NodeID, "tag_name": rel.Tag, "target_commitish": rel.Target, "name": rel.Name, "body": rel.Body,
		"draft": rel.Draft, "prerelease": rel.Prerelease, "created_at": ts(rel.CreatedAt), "published_at": tsp(rel.PublishedAt),
		"author": c.user(rel.Author), "html_url": rel.URL(), "url": api, "assets_url": api + "/assets",
		"upload_url":  strings.Replace(api, c.base, c.s.uploadBase(c.base), 1) + "/assets{?name,label}",
		"tarball_url": fmt.Sprintf("%s/tarball/%s", c.repoURL(rel.repo), rel.Tag),
		"zipball_url": fmt.Sprintf("%s/zipball/%s", c.repoURL(rel.repo), rel.Tag),
		"assets":      assets,
	}
}

func (c *restCtx) commitShape(r *Repo, info CommitInfo) obj {
	parents := []any{}
	for _, p := range info.Parents {
		parents = append(parents, obj{"sha": p, "url": c.repoURL(r) + "/commits/" + p, "html_url": r.URL() + "/commit/" + p})
	}
	return obj{
		"sha": info.SHA, "node_id": "C_" + info.SHA, "html_url": r.URL() + "/commit/" + info.SHA, "url": c.repoURL(r) + "/commits/" + info.SHA,
		"commit": obj{
			"author":    obj{"name": info.Author, "email": info.Email, "date": ts(info.Authored)},
			"committer": obj{"name": info.Committer, "email": info.Email, "date": ts(info.Committed)},
			"message":   info.Message(), "tree": obj{"sha": info.Tree}, "comment_count": 0,
			"verification": obj{"verified": false, "reason": "unsigned"},
		},
		"author": nil, "committer": nil, "parents": parents,
	}
}

// commitInfo reads a commit: the origin's, or the commit a repository of
// declared heads has.
func (c *restCtx) commitInfo(r *Repo, sha string) CommitInfo {
	if c.m.Origins.Has(r) {
		if info, err := c.m.Origins.CommitInfo(r, sha); err == nil {
			return info
		}
	}
	return CommitInfo{SHA: sha, Author: c.m.Viewer, Email: c.m.Viewer + "@users.noreply.github.com", Committer: "GitHub",
		Authored: r.CreatedAt, Committed: r.CreatedAt, Subject: "commit " + sha}
}

func (c *restCtx) contentShape(r *Repo, sha, path string, data []byte) obj {
	api := fmt.Sprintf("%s/contents/%s", c.repoURL(r), path)
	blob := ""
	if c.m.Origins.Has(r) {
		blob = c.m.Origins.BlobSHA(r, sha, path)
	}
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	return obj{
		"type": "file", "encoding": "base64", "size": len(data), "name": name, "path": path, "sha": blob,
		"content": base64.StdEncoding.EncodeToString(data) + "\n", "url": api + "?ref=" + sha,
		"html_url":     fmt.Sprintf("%s/blob/%s/%s", r.URL(), sha, path),
		"download_url": fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", r.Full(), sha, path),
	}
}
