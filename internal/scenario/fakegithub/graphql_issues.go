//go:build scenario

package fakegithub

import (
	"fmt"
	"slices"
	"strings"
)

// subject is what issues and pull requests share.
type subject struct {
	repo      *Repo
	issue     *Issue
	pull      *Pull
	labels    []string
	assignees []string
	comments  []*Comment
	author    string
}

func subjectOf(obj any) subject {
	if i, ok := obj.(*Issue); ok {
		return subject{repo: i.Repo, issue: i, labels: i.Labels, assignees: i.Assignees, comments: i.Comments, author: i.Author}
	}
	p := obj.(*Pull)
	return subject{repo: p.Repo, pull: p, labels: p.Labels, assignees: p.Assignees, comments: p.Comments, author: p.Author}
}

// sharedFields are the fields an issue and a pull request both have.
func sharedFields[T any]() fieldsOf[T] {
	return fieldsOf[T]{
		"repository": func(_ *gqlExec, o T, _ args) any { return subjectOf(o).repo },
		"author":     func(x *gqlExec, o T, _ args) any { return x.actor(subjectOf(o).author) },
		"authorAssociation": func(x *gqlExec, o T, _ args) any {
			s := subjectOf(o)
			return x.association(s.repo, s.author)
		},
		"labels": func(x *gqlExec, o T, a args) any {
			s := subjectOf(o)
			return connect("labels", x.labels(s.repo, s.labels), a)
		},
		"assignees": func(x *gqlExec, o T, a args) any {
			return connect("assignees", x.actors(subjectOf(o).assignees), a)
		},
		"assignedActors": func(x *gqlExec, o T, a args) any {
			return connect("assignedActors", x.actors(subjectOf(o).assignees), a)
		},
		"comments": func(_ *gqlExec, o T, a args) any {
			return connect("comments", subjectOf(o).comments, a)
		},
		"projectItems": func(x *gqlExec, o T, a args) any {
			s := subjectOf(o)
			items := x.m.Items(s.issue, s.pull)
			if v, given := a["includeArchived"].(bool); given && !v {
				items = slices.DeleteFunc(items, func(it *ProjectItem) bool { return it.Archived })
			}
			return connect("projectItems", items, a)
		},
		"projectCards": func(_ *gqlExec, _ T, a args) any {
			return connect("projectCards", []any{}, a)
		},
		"milestone":        func(_ *gqlExec, _ T, _ args) any { return nil },
		"reactionGroups":   func(_ *gqlExec, _ T, _ args) any { return []any{} },
		"locked":           func(_ *gqlExec, _ T, _ args) any { return false },
		"activeLockReason": func(_ *gqlExec, _ T, _ args) any { return nil },
		"viewerDidAuthor": func(x *gqlExec, o T, _ args) any {
			return strings.EqualFold(subjectOf(o).author, x.m.Viewer)
		},
		"viewerCanUpdate": func(_ *gqlExec, _ T, _ args) any { return true },
	}
}

func issueLinks(name string, issues []*Issue, a args) *connection {
	return connect(name, issues, a)
}

func defineIssues() {
	define("Issue", sharedFields[*Issue](), fieldsOf[*Issue]{
		"id":             func(_ *gqlExec, i *Issue, _ args) any { return i.NodeID },
		"databaseId":     func(_ *gqlExec, i *Issue, _ args) any { return i.ID },
		"fullDatabaseId": func(_ *gqlExec, i *Issue, _ args) any { return fmt.Sprint(i.ID) },
		"number":         func(_ *gqlExec, i *Issue, _ args) any { return i.Number },
		"title":          func(_ *gqlExec, i *Issue, _ args) any { return i.Title },
		"body":           func(_ *gqlExec, i *Issue, _ args) any { return i.Body },
		"bodyText":       func(_ *gqlExec, i *Issue, _ args) any { return i.Body },
		"url":            func(_ *gqlExec, i *Issue, _ args) any { return i.URL() },
		"resourcePath":   func(_ *gqlExec, i *Issue, _ args) any { return strings.TrimPrefix(i.URL(), webHost) },
		"state":          func(_ *gqlExec, i *Issue, _ args) any { return i.State },
		"stateReason": func(_ *gqlExec, i *Issue, _ args) any {
			if i.StateReason == "" {
				return nil
			}
			return i.StateReason
		},
		"closed":    func(_ *gqlExec, i *Issue, _ args) any { return i.State == stateClosed },
		"createdAt": func(_ *gqlExec, i *Issue, _ args) any { return i.CreatedAt },
		"updatedAt": func(_ *gqlExec, i *Issue, _ args) any { return i.UpdatedAt },
		"closedAt":  func(_ *gqlExec, i *Issue, _ args) any { return i.ClosedAt },
		"isPinned":  func(_ *gqlExec, _ *Issue, _ args) any { return false },
		"issueType": func(_ *gqlExec, _ *Issue, _ args) any { return nil },
		"parent": func(_ *gqlExec, i *Issue, _ args) any {
			if i.Parent == nil {
				return nil
			}
			return i.Parent
		},
		"subIssues": func(_ *gqlExec, i *Issue, a args) any { return issueLinks("subIssues", i.Subs, a) },
		"subIssuesSummary": func(_ *gqlExec, i *Issue, _ args) any {
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
			return &payload{typ: "SubIssuesSummary", fields: map[string]any{"total": len(i.Subs), "completed": done, "percentCompleted": percent}}
		},
		"blockedBy": func(_ *gqlExec, i *Issue, a args) any { return issueLinks("blockedBy", i.BlockedBy, a) },
		"blocking": func(x *gqlExec, i *Issue, a args) any {
			var out []*Issue
			for _, r := range x.m.Repos() {
				for _, other := range r.Issues {
					if slices.Contains(other.BlockedBy, i) {
						out = append(out, other)
					}
				}
			}
			return issueLinks("blocking", out, a)
		},
		"closedByPullRequestsReferences": func(x *gqlExec, i *Issue, a args) any {
			var out []*Pull
			for _, p := range x.m.ClosedByPulls(i) {
				if p.State == stateOpen || a.flag("includeClosedPrs") {
					out = append(out, p)
				}
			}
			return connect("closedByPullRequestsReferences", out, a)
		},
		"timelineItems": func(_ *gqlExec, i *Issue, a args) any {
			var out []any
			for _, kind := range a.strs("itemTypes") {
				switch kind {
				case "CLOSED_EVENT":
					if i.State == stateClosed {
						out = append(out, &closedEvent{issue: i})
					}
				case "REOPENED_EVENT":
					if i.Reopened != nil {
						out = append(out, &reopenedEvent{at: *i.Reopened})
					}
				default:
					return unsupported("timeline items of type %s", kind)
				}
			}
			if !a.has("itemTypes") {
				return unsupported("timelineItems without itemTypes")
			}
			return connect("timelineItems", out, a)
		},
	})

	define("ClosedEvent", fieldsOf[*closedEvent]{
		"id": func(_ *gqlExec, e *closedEvent, _ args) any {
			if e.issue != nil {
				return "CE_" + e.issue.NodeID
			}
			return "CE_" + e.pull.NodeID
		},
		"createdAt": func(_ *gqlExec, e *closedEvent, _ args) any {
			if e.issue != nil {
				return e.issue.ClosedAt
			}
			return e.pull.ClosedAt
		},
		"closer": func(_ *gqlExec, e *closedEvent, _ args) any {
			if e.issue != nil && e.issue.ClosedBy != nil {
				return e.issue.ClosedBy
			}
			return nil
		},
		"stateReason": func(_ *gqlExec, e *closedEvent, _ args) any {
			if e.issue != nil && e.issue.StateReason != "" {
				return e.issue.StateReason
			}
			return nil
		},
	})
	define("ReopenedEvent", fieldsOf[*reopenedEvent]{
		"createdAt": func(_ *gqlExec, e *reopenedEvent, _ args) any { return e.at },
	})

	define("PullRequest", sharedFields[*Pull](), fieldsOf[*Pull]{
		"id":             func(_ *gqlExec, p *Pull, _ args) any { return p.NodeID },
		"databaseId":     func(_ *gqlExec, p *Pull, _ args) any { return p.ID },
		"fullDatabaseId": func(_ *gqlExec, p *Pull, _ args) any { return fmt.Sprint(p.ID) },
		"number":         func(_ *gqlExec, p *Pull, _ args) any { return p.Number },
		"title":          func(_ *gqlExec, p *Pull, _ args) any { return p.Title },
		"body":           func(_ *gqlExec, p *Pull, _ args) any { return p.Body },
		"bodyText":       func(_ *gqlExec, p *Pull, _ args) any { return p.Body },
		"url":            func(_ *gqlExec, p *Pull, _ args) any { return p.URL() },
		"resourcePath":   func(_ *gqlExec, p *Pull, _ args) any { return strings.TrimPrefix(p.URL(), webHost) },
		"state":          func(_ *gqlExec, p *Pull, _ args) any { return p.State },
		"closed":         func(_ *gqlExec, p *Pull, _ args) any { return p.State != stateOpen },
		"merged":         func(_ *gqlExec, p *Pull, _ args) any { return p.State == stateMerged },
		"isDraft":        func(_ *gqlExec, p *Pull, _ args) any { return p.Draft },
		"createdAt":      func(_ *gqlExec, p *Pull, _ args) any { return p.CreatedAt },
		"updatedAt":      func(_ *gqlExec, p *Pull, _ args) any { return p.UpdatedAt },
		"closedAt":       func(_ *gqlExec, p *Pull, _ args) any { return p.ClosedAt },
		"mergedAt":       func(_ *gqlExec, p *Pull, _ args) any { return p.MergedAt },
		"mergedBy":       func(x *gqlExec, p *Pull, _ args) any { return x.actor(p.MergedBy) },
		"baseRefName":    func(_ *gqlExec, p *Pull, _ args) any { return p.Base },
		"baseRefOid":     func(x *gqlExec, p *Pull, _ args) any { return x.m.BaseSHA(p) },
		"headRefName":    func(_ *gqlExec, p *Pull, _ args) any { return p.Head },
		"headRefOid":     func(x *gqlExec, p *Pull, _ args) any { return x.m.HeadSHA(p) },
		"baseRef":        func(x *gqlExec, p *Pull, _ args) any { return x.ref(p.Repo, "refs/heads/", p.Base) },
		"headRef": func(x *gqlExec, p *Pull, _ args) any {
			if p.State != stateOpen {
				return nil
			}
			return x.ref(p.Repo, "refs/heads/", p.Head)
		},
		"baseRepository":      func(_ *gqlExec, p *Pull, _ args) any { return p.Repo },
		"headRepository":      func(_ *gqlExec, p *Pull, _ args) any { return p.Repo },
		"headRepositoryOwner": func(x *gqlExec, p *Pull, _ args) any { return x.m.Account(p.Repo.Owner, "") },
		"isCrossRepository":   func(_ *gqlExec, _ *Pull, _ args) any { return false },
		"maintainerCanModify": func(_ *gqlExec, _ *Pull, _ args) any { return false },
		"mergeable":           func(x *gqlExec, p *Pull, _ args) any { return x.m.MergeableState(p) },
		"mergeStateStatus":    func(x *gqlExec, p *Pull, _ args) any { return x.m.MergeStateStatus(p) },
		"reviewDecision": func(_ *gqlExec, p *Pull, _ args) any {
			if d := p.ReviewDecision(); d != "" {
				return d
			}
			return nil
		},
		"additions": func(x *gqlExec, p *Pull, _ args) any {
			n := 0
			for _, f := range x.m.PullFiles(p) {
				n += f.Additions
			}
			return n
		},
		"deletions": func(x *gqlExec, p *Pull, _ args) any {
			n := 0
			for _, f := range x.m.PullFiles(p) {
				n += f.Deletions
			}
			return n
		},
		"changedFiles": func(x *gqlExec, p *Pull, _ args) any { return len(x.m.PullFiles(p)) },
		"files": func(x *gqlExec, p *Pull, a args) any {
			var out []any
			for _, f := range x.m.PullFiles(p) {
				out = append(out, &payload{typ: "PullRequestChangedFile", fields: map[string]any{
					"path": f.Path, "additions": f.Additions, "deletions": f.Deletions, "changeType": "MODIFIED",
				}})
			}
			return connect("files", out, a)
		},
		"commits": func(x *gqlExec, p *Pull, a args) any {
			var out []any
			for _, c := range x.m.PullCommits(p) {
				info := c
				out = append(out, &payload{typ: "PullRequestCommit", fields: map[string]any{
					"id": "PRC_" + c.SHA, "url": p.URL() + "/commits/" + c.SHA,
					"commit": &commitObj{repo: p.Repo, sha: c.SHA, info: &info},
				}})
			}
			return connect("commits", out, a)
		},
		"mergeCommit":          func(x *gqlExec, p *Pull, _ args) any { return x.commit(p.Repo, p.MergeCommit) },
		"potentialMergeCommit": func(_ *gqlExec, _ *Pull, _ args) any { return nil },
		"autoMergeRequest": func(x *gqlExec, p *Pull, _ args) any {
			if p.AutoMerge == "" {
				return nil
			}
			return &payload{typ: "AutoMergeRequest", fields: map[string]any{
				"authorEmail": nil, "commitBody": nil, "commitHeadline": nil, "mergeMethod": p.AutoMerge,
				"enabledAt": p.UpdatedAt, "enabledBy": x.m.Account(x.m.Viewer, ""),
			}}
		},
		"isInMergeQueue": func(_ *gqlExec, _ *Pull, _ args) any { return false },
		"isMergeQueueEnabled": func(_ *gqlExec, p *Pull, _ args) any {
			prot, ok := p.Repo.Protection(p.Base)
			return ok && prot.MergeQueue
		},
		"reviews": func(_ *gqlExec, p *Pull, a args) any {
			states := upperStates(a, "states")
			var out []*Review
			for _, rv := range p.Reviews {
				if len(states) == 0 || slices.Contains(states, rv.State) {
					out = append(out, rv)
				}
			}
			return connect("reviews", out, a)
		},
		"latestReviews": func(_ *gqlExec, p *Pull, a args) any { return connect("latestReviews", p.LatestReviews(), a) },
		"latestOpinionatedReviews": func(_ *gqlExec, p *Pull, a args) any {
			var out []*Review
			for _, rv := range p.LatestReviews() {
				if rv.State != reviewCommented {
					out = append(out, rv)
				}
			}
			return connect("latestOpinionatedReviews", out, a)
		},
		"reviewRequests": func(_ *gqlExec, _ *Pull, a args) any { return connect("reviewRequests", []any{}, a) },
		"reviewThreads":  func(_ *gqlExec, _ *Pull, a args) any { return connect("reviewThreads", []any{}, a) },
		"closingIssuesReferences": func(x *gqlExec, p *Pull, a args) any {
			return connect("closingIssuesReferences", x.m.Closes(p), a)
		},
		"statusCheckRollup": func(x *gqlExec, p *Pull, _ args) any {
			head := x.m.HeadSHA(p)
			if p.Repo.Rollup(head) == "" {
				return nil
			}
			return &rollupObj{repo: p.Repo, sha: head}
		},
		"timelineItems": func(_ *gqlExec, p *Pull, a args) any {
			var out []any
			for _, kind := range a.strs("itemTypes") {
				if kind != "CLOSED_EVENT" {
					return unsupported("timeline items of type %s", kind)
				}
				if p.State != stateOpen {
					out = append(out, &closedEvent{pull: p})
				}
			}
			if !a.has("itemTypes") {
				return unsupported("timelineItems without itemTypes")
			}
			return connect("timelineItems", out, a)
		},
	})
}

// PullFiles are the files a pull request changes: the declared ones, the
// ones kept when it merged, or the origin's diff of its head against its
// base.
func (m *Model) PullFiles(p *Pull) []PullFile {
	if p.Files != nil || !m.Origins.Has(p.Repo) {
		return p.Files
	}
	base, head := m.BaseSHA(p), m.HeadSHA(p)
	if base == "" || head == "" {
		return nil
	}
	files, err := m.Origins.Changed(p.Repo, base, head)
	if err != nil {
		return nil
	}
	return files
}

// PullCommits are a pull request's commits, oldest first: the ones kept
// when it merged, the origin's commits of its head beyond its base, or
// its declared head alone.
func (m *Model) PullCommits(p *Pull) []CommitInfo {
	if p.commits != nil {
		return p.commits
	}
	head := m.HeadSHA(p)
	if head == "" {
		return nil
	}
	if m.Origins.Has(p.Repo) {
		if base := m.BaseSHA(p); base != "" {
			if commits, err := m.Origins.Log(p.Repo, base, head, 250); err == nil {
				return commits
			}
		}
	}
	return []CommitInfo{{SHA: head, Author: p.Author, Email: p.Author + "@users.noreply.github.com", Committer: "GitHub",
		Authored: p.CreatedAt, Committed: p.CreatedAt, Subject: p.Title}}
}
