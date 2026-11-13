//go:build scenario

package fakegithub

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// The REST routes the model serves. A request for any other path is
// recorded as unsupported and fails the scenario.

func init() {
	handle("GET", "/user", func(c *restCtx) (any, error) { return c.user(c.m.Viewer), nil })
	handle("GET", "/users/{login}", func(c *restCtx) (any, error) {
		a, ok := c.m.LookupAccount(c.param("login"))
		if !ok {
			return nil, errNotFound()
		}
		return c.user(a.Login), nil
	})
	handle("GET", "/orgs/{org}", func(c *restCtx) (any, error) {
		a, ok := c.m.LookupAccount(c.param("org"))
		if !ok || a.Kind != kindOrg {
			return nil, errNotFound()
		}
		return c.user(a.Login), nil
	})
	handle("GET", "/user/memberships/orgs/{org}", func(c *restCtx) (any, error) {
		a, ok := c.m.LookupAccount(c.param("org"))
		if !ok || a.Kind != kindOrg {
			return nil, errNotFound()
		}
		return obj{"state": "active", "role": "admin", "organization": c.user(a.Login), "user": c.user(c.m.Viewer)}, nil
	})
	handle("GET", "/orgs/{org}/members", func(c *restCtx) (any, error) {
		a, ok := c.m.LookupAccount(c.param("org"))
		if !ok || a.Kind != kindOrg {
			return nil, errNotFound()
		}
		return list(c, c.m.Members(a.Login), func(l string) obj { return c.user(l).(obj) }), nil
	})
	handle("GET", "/orgs/{org}/members/{login}", func(c *restCtx) (any, error) {
		if !containsFold(c.m.Members(c.param("org")), c.param("login")) {
			return nil, errNotFound()
		}
		c.status = http.StatusNoContent
		return nil, nil
	})
	handle("GET", "/orgs/{org}/teams/{team}/members", func(c *restCtx) (any, error) { return []any{}, nil })
	handle("GET", "/orgs/{org}/teams/{team}/memberships/{login}", func(c *restCtx) (any, error) {
		return nil, errNotFound()
	})

	handle("GET", "/repos/{owner}/{repo}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		return c.repoShape(r), nil
	})

	branches()
	commits()
	issues()
	pulls()
	checks()
	releases()

	handle("GET", "/search/issues", func(c *restCtx) (any, error) {
		hits, err := c.m.Search(c.query.Get("q"), c.query.Get("advanced_search") == "true")
		if err != nil {
			return nil, err
		}
		items := []any{}
		for _, h := range page(c, hits) {
			switch t := h.(type) {
			case *Issue:
				items = append(items, c.issueShape(t))
			case *Pull:
				items = append(items, c.pullAsIssue(t))
			}
		}
		return obj{"total_count": len(hits), "incomplete_results": false, "items": items}, nil
	}).bucket = bucketSearch
}

func branches() {
	branch := func(c *restCtx, r *Repo, name string) (obj, bool) {
		sha, ok := c.m.Branch(r, name)
		if !ok {
			return nil, false
		}
		_, protected := r.Protection(name)
		return obj{
			"name": name, "protected": protected,
			"commit":         c.commitShape(r, c.commitInfo(r, sha)),
			"protection_url": fmt.Sprintf("%s/branches/%s/protection", c.repoURL(r), name),
		}, true
	}
	handle("GET", "/repos/{owner}/{repo}/branches", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		return list(c, c.m.BranchNames(r), func(name string) obj {
			sha, _ := c.m.Branch(r, name)
			_, protected := r.Protection(name)
			return obj{"name": name, "protected": protected, "commit": obj{"sha": sha, "url": c.repoURL(r) + "/commits/" + sha}}
		}), nil
	})
	handle("GET", "/repos/{owner}/{repo}/branches/{branch...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		name := c.param("branch")
		if rest, ok := strings.CutSuffix(name, "/protection"); ok {
			prot, protected := r.Protection(rest)
			if !protected {
				return nil, &apiError{Status: http.StatusNotFound, Message: "Branch not protected"}
			}
			contexts := []any{}
			for _, x := range prot.RequiredChecks {
				contexts = append(contexts, x)
			}
			return obj{
				"url":                           fmt.Sprintf("%s/branches/%s/protection", c.repoURL(r), rest),
				"required_status_checks":        obj{"strict": prot.Strict, "contexts": contexts},
				"required_pull_request_reviews": obj{"required_approving_review_count": prot.RequiredReviews},
				"enforce_admins":                obj{"enabled": prot.EnforceAdmins},
			}, nil
		}
		b, ok := branch(c, r, name)
		if !ok {
			return nil, &apiError{Status: http.StatusNotFound, Message: "Branch not found"}
		}
		return b, nil
	})
	handle("GET", "/repos/{owner}/{repo}/rules/branches/{branch...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		rules := []any{}
		prot, ok := r.Protection(c.param("branch"))
		if !ok {
			return rules, nil
		}
		source := obj{"ruleset_source_type": "Repository", "ruleset_source": r.Full(), "ruleset_id": prot.NodeID}
		with := func(kind string, parameters obj) obj {
			rule := obj{"type": kind, "parameters": parameters}
			for k, v := range source {
				rule[k] = v
			}
			return rule
		}
		if prot.RequiredReviews > 0 {
			rules = append(rules, with("pull_request", obj{"required_approving_review_count": prot.RequiredReviews,
				"dismiss_stale_reviews_on_push": false, "require_code_owner_review": false, "require_last_push_approval": false,
				"required_review_thread_resolution": false}))
		}
		if len(prot.RequiredChecks) > 0 {
			required := []any{}
			for _, x := range prot.RequiredChecks {
				required = append(required, obj{"context": x})
			}
			rules = append(rules, with("required_status_checks", obj{"required_status_checks": required,
				"strict_required_status_checks_policy": prot.Strict}))
		}
		if prot.MergeQueue {
			rules = append(rules, with("merge_queue", obj{"merge_method": "SQUASH"}))
		}
		return rules, nil
	})

	ref := func(c *restCtx, r *Repo, name string) (obj, bool) {
		full := "refs/" + strings.TrimPrefix(name, "refs/")
		var sha string
		var ok bool
		switch {
		case strings.HasPrefix(full, "refs/heads/"):
			sha, ok = c.m.Branch(r, strings.TrimPrefix(full, "refs/heads/"))
		case strings.HasPrefix(full, "refs/tags/"):
			sha, ok = c.m.Tag(r, strings.TrimPrefix(full, "refs/tags/"))
		}
		if !ok {
			return nil, false
		}
		return obj{
			"ref": full, "node_id": "REF_" + r.NodeID + "_" + full,
			"url":    fmt.Sprintf("%s/git/%s", c.repoURL(r), full),
			"object": obj{"type": "commit", "sha": sha, "url": fmt.Sprintf("%s/git/commits/%s", c.repoURL(r), sha)},
		}, true
	}
	getRef := func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		out, ok := ref(c, r, c.param("ref"))
		if !ok {
			return nil, errNotFound()
		}
		return out, nil
	}
	// The refs whose name starts with the given one, branches before tags
	// and each sorted by name: an empty list when none does, no 404.
	handle("GET", "/repos/{owner}/{repo}/git/matching-refs/{ref...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		prefix := "refs/" + strings.TrimPrefix(c.param("ref"), "refs/")
		var names []string
		for _, b := range c.m.BranchNames(r) {
			names = append(names, "refs/heads/"+b)
		}
		for _, t := range c.m.TagNames(r) {
			names = append(names, "refs/tags/"+t)
		}
		names = slices.DeleteFunc(names, func(name string) bool { return !strings.HasPrefix(name, prefix) })
		return list(c, names, func(name string) obj {
			out, _ := ref(c, r, name)
			return out
		}), nil
	})
	handle("GET", "/repos/{owner}/{repo}/git/ref/{ref...}", getRef)
	handle("GET", "/repos/{owner}/{repo}/git/refs/{ref...}", getRef)
	handle("DELETE", "/repos/{owner}/{repo}/git/refs/{ref...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		name, ok := strings.CutPrefix(c.param("ref"), "heads/")
		if !ok {
			return nil, unsupported("deleting the ref %s (only branches are deleted)", c.param("ref"))
		}
		if err := c.m.DeleteBranch(r, name); err != nil {
			return nil, err
		}
		c.status = http.StatusNoContent
		return nil, nil
	})
	handle("POST", "/repos/{owner}/{repo}/git/refs", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		name, sha := c.str("ref"), c.str("sha")
		if _, exists := ref(c, r, name); exists {
			return nil, &apiError{Status: http.StatusUnprocessableEntity, Message: "Reference already exists"}
		}
		if _, ok := c.m.Commit(r, sha); !ok {
			return nil, &apiError{Status: http.StatusUnprocessableEntity, Message: "Object does not exist"}
		}
		if err := c.m.SetRef(r, name, sha); err != nil {
			return nil, err
		}
		c.status = http.StatusCreated
		out, _ := ref(c, r, name)
		return out, nil
	})
	handle("GET", "/repos/{owner}/{repo}/tags", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		names := c.m.TagNames(r)
		slices.Reverse(names)
		return list(c, names, func(name string) obj {
			sha, _ := c.m.Tag(r, name)
			return obj{"name": name, "commit": obj{"sha": sha, "url": c.repoURL(r) + "/commits/" + sha}, "node_id": "REF_" + r.NodeID + "_refs/tags/" + name}
		}), nil
	})
}

func commits() {
	handle("GET", "/repos/{owner}/{repo}/commits", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		sha, ok := c.m.Commit(r, cmpOr(c.query.Get("sha"), r.DefaultBranch))
		if !ok {
			return nil, errNotFound()
		}
		infos := []CommitInfo{c.commitInfo(r, sha)}
		if c.m.Origins.Has(r) {
			if log, err := c.m.Origins.Log(r, "", sha, 250); err == nil {
				slices.Reverse(log)
				infos = log
			}
		}
		return list(c, infos, func(info CommitInfo) obj { return c.commitShape(r, info) }), nil
	})
	handle("GET", "/repos/{owner}/{repo}/commits/{ref...}/check-runs", func(c *restCtx) (any, error) {
		r, sha, err := c.commit("ref")
		if err != nil {
			return nil, err
		}
		var runs []*CheckRun
		for _, ch := range r.Checks {
			if ch.SHA == sha && (c.query.Get("check_name") == "" || c.query.Get("check_name") == ch.Name) {
				runs = append(runs, ch)
			}
		}
		if c.query.Get("filter") != "all" {
			runs, _ = r.Contexts(sha)
			if name := c.query.Get("check_name"); name != "" {
				runs = slices.DeleteFunc(runs, func(ch *CheckRun) bool { return ch.Name != name })
			}
		}
		return obj{"total_count": len(runs), "check_runs": list(c, runs, c.checkShape)}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/commits/{ref...}/status", func(c *restCtx) (any, error) {
		r, sha, err := c.commit("ref")
		if err != nil {
			return nil, err
		}
		_, statuses := r.Contexts(sha)
		state := "pending"
		if len(statuses) > 0 {
			state = "success"
		}
		for _, s := range statuses {
			switch s.State {
			case statusFailure, statusError:
				state = "failure"
			case statusPending:
				if state != "failure" {
					state = "pending"
				}
			}
		}
		return obj{
			"state": state, "sha": sha, "total_count": len(statuses), "statuses": list(c, statuses, c.statusShape(r)),
			"repository": obj{"id": r.ID, "name": r.Name, "full_name": r.Full()},
			"commit_url": c.repoURL(r) + "/commits/" + sha, "url": c.repoURL(r) + "/commits/" + sha + "/status",
		}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/commits/{ref...}/statuses", func(c *restCtx) (any, error) {
		r, sha, err := c.commit("ref")
		if err != nil {
			return nil, err
		}
		var statuses []*Status
		for _, s := range r.Statuses {
			if s.SHA == sha {
				statuses = append(statuses, s)
			}
		}
		slices.Reverse(statuses)
		return list(c, statuses, c.statusShape(r)), nil
	})
	handle("GET", "/repos/{owner}/{repo}/commits/{ref...}/pulls", func(c *restCtx) (any, error) {
		r, sha, err := c.commit("ref")
		if err != nil {
			return nil, err
		}
		var out []*Pull
		for _, p := range r.Pulls {
			if p.MergeCommit == sha || c.m.HeadSHA(p) == sha {
				out = append(out, p)
			}
		}
		return list(c, out, c.pullShape), nil
	})
	handle("GET", "/repos/{owner}/{repo}/commits/{ref...}", func(c *restCtx) (any, error) {
		r, sha, err := c.commit("ref")
		if err != nil {
			return nil, err
		}
		out := c.commitShape(r, c.commitInfo(r, sha))
		out["files"], out["stats"] = []any{}, obj{"total": 0, "additions": 0, "deletions": 0}
		return out, nil
	})
	handle("POST", "/repos/{owner}/{repo}/statuses/{sha}", func(c *restCtx) (any, error) {
		r, sha, err := c.commit("sha")
		if err != nil {
			return nil, err
		}
		state := strings.ToUpper(c.str("state"))
		if !slices.Contains(statusStates, state) {
			return nil, errInvalid("state is not one of error, failure, pending, success")
		}
		s := c.m.SetStatus(r, sha, cmpOr(c.str("context"), "default"), state, c.str("description"), c.str("target_url"))
		c.status = http.StatusCreated
		return c.statusShape(r)(s), nil
	})
	handle("GET", "/repos/{owner}/{repo}/compare/{basehead...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		baseRef, headRef, ok := strings.Cut(c.param("basehead"), "...")
		if !ok {
			return nil, errNotFound()
		}
		base, okBase := c.m.Commit(r, baseRef)
		head, okHead := c.m.Commit(r, headRef)
		if !okBase || !okHead {
			return nil, errNotFound()
		}
		ahead, behind := 0, 0
		commits, files := []any{}, []any{}
		merge := base
		if c.m.Origins.Has(r) {
			if ahead, behind, err = c.m.Origins.AheadBehind(r, base, head); err != nil {
				return nil, errNotFound()
			}
			if mb, err := c.m.Origins.MergeBase(r, base, head); err == nil {
				merge = mb
			}
			if log, err := c.m.Origins.Log(r, base, head, 250); err == nil {
				for _, info := range log {
					commits = append(commits, c.commitShape(r, info))
				}
			}
			if changed, err := c.m.Origins.Changed(r, base, head); err == nil {
				for _, f := range changed {
					files = append(files, obj{"filename": f.Path, "status": "modified", "additions": f.Additions, "deletions": f.Deletions, "changes": f.Additions + f.Deletions})
				}
			}
		} else if base != head {
			return nil, unsupported("comparing %s...%s of %s, which has no origin", baseRef, headRef, r.Full())
		}
		status := "identical"
		switch {
		case ahead > 0 && behind > 0:
			status = "diverged"
		case ahead > 0:
			status = "ahead"
		case behind > 0:
			status = "behind"
		}
		return obj{
			"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": ahead,
			"base_commit":       c.commitShape(r, c.commitInfo(r, base)),
			"merge_base_commit": c.commitShape(r, c.commitInfo(r, merge)),
			"commits":           commits, "files": files,
			"html_url": fmt.Sprintf("%s/compare/%s...%s", r.URL(), baseRef, headRef),
			"url":      fmt.Sprintf("%s/compare/%s...%s", c.repoURL(r), baseRef, headRef),
		}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/contents/{path...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		sha, ok := c.m.Commit(r, cmpOr(c.query.Get("ref"), r.DefaultBranch))
		if !ok {
			return nil, errNotFound()
		}
		var data []byte
		found := false
		if c.m.Origins.Has(r) {
			data, found = c.m.Origins.File(r, sha, c.param("path"))
		}
		if !found {
			if text, declared := r.Files[c.param("path")]; declared {
				data, found = []byte(text), true
			}
		}
		if !found {
			return nil, errNotFound()
		}
		if c.accepts("application/vnd.github.raw") || c.accepts("application/vnd.github.v3.raw") {
			return rawBody{ContentType: "application/vnd.github.raw; charset=utf-8", Data: data}, nil
		}
		return c.contentShape(r, sha, c.param("path"), data), nil
	})
}

func issues() {
	handle("GET", "/repos/{owner}/{repo}/issues", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		state := cmpOr(c.query.Get("state"), "open")
		var labels []string
		if l := c.query.Get("labels"); l != "" {
			labels = strings.Split(l, ",")
		}
		type entry struct {
			number int
			shape  obj
		}
		var all []entry
		for _, i := range r.Issues {
			if (state == "all" || strings.EqualFold(state, i.State)) && hasAll(i.Labels, labels) {
				all = append(all, entry{i.Number, c.issueShape(i)})
			}
		}
		for _, p := range r.Pulls {
			open := p.State == stateOpen
			if (state == "all" || open == (state == "open")) && hasAll(p.Labels, labels) {
				all = append(all, entry{p.Number, c.pullAsIssue(p)})
			}
		}
		slices.SortFunc(all, func(a, b entry) int { return b.number - a.number })
		if c.query.Get("direction") == "asc" {
			slices.Reverse(all)
		}
		return list(c, all, func(e entry) obj { return e.shape }), nil
	})
	handle("POST", "/repos/{owner}/{repo}/issues", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		if c.str("title") == "" {
			return nil, errInvalid("title can't be blank")
		}
		i, err := c.m.AddIssue(r, IssueSpec{Title: c.str("title"), Body: c.str("body"), Labels: c.strs("labels"), Assignees: c.strs("assignees")})
		if err != nil {
			return nil, err
		}
		c.status = http.StatusCreated
		return c.issueShape(i), nil
	})
	handle("GET", "/repos/{owner}/{repo}/issues/comments/{id}", func(c *restCtx) (any, error) {
		cm, err := c.comment()
		if err != nil {
			return nil, err
		}
		return c.commentShape(cm), nil
	})
	handle("PATCH", "/repos/{owner}/{repo}/issues/comments/{id}", func(c *restCtx) (any, error) {
		cm, err := c.comment()
		if err != nil {
			return nil, err
		}
		cm.Body, cm.UpdatedAt = c.str("body"), c.m.now()
		return c.commentShape(cm), nil
	})
	handle("DELETE", "/repos/{owner}/{repo}/issues/comments/{id}", func(c *restCtx) (any, error) {
		cm, err := c.comment()
		if err != nil {
			return nil, err
		}
		if cm.issue != nil {
			cm.issue.Comments = slices.DeleteFunc(cm.issue.Comments, func(o *Comment) bool { return o == cm })
		} else {
			cm.pull.Comments = slices.DeleteFunc(cm.pull.Comments, func(o *Comment) bool { return o == cm })
		}
		c.status = http.StatusNoContent
		return nil, nil
	})
	handle("GET", "/repos/{owner}/{repo}/issues/{number}", func(c *restCtx) (any, error) {
		_, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		if p != nil {
			return c.pullAsIssue(p), nil
		}
		return c.issueShape(i), nil
	})
	handle("PATCH", "/repos/{owner}/{repo}/issues/{number}", func(c *restCtx) (any, error) {
		r, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		if p != nil {
			if err := c.patchPull(p); err != nil {
				return nil, err
			}
			if c.has("labels") {
				p.Labels = nil
				for _, l := range c.strs("labels") {
					p.Labels = append(p.Labels, c.m.EnsureLabel(r, l, "", "").Name)
				}
			}
			return c.pullAsIssue(p), nil
		}
		if c.has("title") {
			i.Title = c.str("title")
		}
		if c.has("body") {
			i.Body = c.str("body")
		}
		if c.has("labels") {
			i.Labels = nil
			for _, l := range c.strs("labels") {
				i.Labels = append(i.Labels, c.m.EnsureLabel(r, l, "", "").Name)
			}
		}
		if c.has("assignees") {
			i.Assignees = nil
			for _, a := range c.strs("assignees") {
				i.Assignees = append(i.Assignees, c.m.Account(a, "").Login)
			}
		}
		switch c.str("state") {
		case "closed":
			c.m.CloseIssue(i, strings.ToUpper(c.str("state_reason")))
		case "open":
			c.m.ReopenIssue(i)
		}
		i.UpdatedAt = c.m.now()
		return c.issueShape(i), nil
	})
	handle("GET", "/repos/{owner}/{repo}/issues/{number}/comments", func(c *restCtx) (any, error) {
		_, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		comments := subjectComments(i, p)
		return list(c, comments, c.commentShape), nil
	})
	handle("POST", "/repos/{owner}/{repo}/issues/{number}/comments", func(c *restCtx) (any, error) {
		_, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		if c.str("body") == "" {
			return nil, errInvalid("body can't be blank")
		}
		c.status = http.StatusCreated
		return c.commentShape(c.m.AddComment(i, p, "", c.str("body"))), nil
	})
	handle("GET", "/repos/{owner}/{repo}/issues/{number}/labels", func(c *restCtx) (any, error) {
		r, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		_, names := labelsOf(i, p)
		return c.labelShapes(r, *names), nil
	})
	handle("POST", "/repos/{owner}/{repo}/issues/{number}/labels", func(c *restCtx) (any, error) {
		r, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		_, names := labelsOf(i, p)
		for _, l := range c.strs("labels") {
			if !containsFold(*names, l) {
				*names = append(*names, c.m.EnsureLabel(r, l, "", "").Name)
			}
		}
		return c.labelShapes(r, *names), nil
	})
	handle("DELETE", "/repos/{owner}/{repo}/issues/{number}/labels/{name}", func(c *restCtx) (any, error) {
		r, i, p, err := c.subject()
		if err != nil {
			return nil, err
		}
		_, names := labelsOf(i, p)
		if !containsFold(*names, c.param("name")) {
			return nil, &apiError{Status: http.StatusNotFound, Message: "Label does not exist"}
		}
		*names = slices.DeleteFunc(*names, func(n string) bool { return strings.EqualFold(n, c.param("name")) })
		return c.labelShapes(r, *names), nil
	})
	handle("GET", "/repos/{owner}/{repo}/issues/{number}/sub_issues", func(c *restCtx) (any, error) {
		_, i, err := c.issue()
		if err != nil {
			return nil, err
		}
		return list(c, i.Subs, c.issueShape), nil
	})

	handle("GET", "/repos/{owner}/{repo}/labels", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		return list(c, r.Labels, c.labelShape(r)), nil
	})
	handle("POST", "/repos/{owner}/{repo}/labels", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		if _, exists := r.Label(c.str("name")); exists {
			return nil, &apiError{Status: http.StatusUnprocessableEntity, Message: "Validation Failed", Errors: []string{"already_exists"}}
		}
		if c.str("name") == "" {
			return nil, errInvalid("name can't be blank")
		}
		c.status = http.StatusCreated
		return c.labelShape(r)(c.m.EnsureLabel(r, c.str("name"), strings.TrimPrefix(c.str("color"), "#"), c.str("description"))), nil
	})
	handle("GET", "/repos/{owner}/{repo}/labels/{name}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		l, ok := r.Label(c.param("name"))
		if !ok {
			return nil, errNotFound()
		}
		return c.labelShape(r)(l), nil
	})
	handle("PATCH", "/repos/{owner}/{repo}/labels/{name}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		l, ok := r.Label(c.param("name"))
		if !ok {
			return nil, errNotFound()
		}
		if c.has("color") {
			l.Color = strings.TrimPrefix(c.str("color"), "#")
		}
		if c.has("description") {
			l.Description = c.str("description")
		}
		if name := c.str("new_name"); name != "" && name != l.Name {
			renameLabel(r, l, name)
		}
		return c.labelShape(r)(l), nil
	})
}

func renameLabel(r *Repo, l *Label, name string) {
	swap := func(names []string) {
		for i, n := range names {
			if strings.EqualFold(n, l.Name) {
				names[i] = name
			}
		}
	}
	for _, i := range r.Issues {
		swap(i.Labels)
	}
	for _, p := range r.Pulls {
		swap(p.Labels)
	}
	l.Name = name
}

func subjectComments(i *Issue, p *Pull) []*Comment {
	if i != nil {
		return i.Comments
	}
	return p.Comments
}

// comment is the comment of the path's id in the path's repository.
func (c *restCtx) comment() (*Comment, error) {
	r, err := c.repo()
	if err != nil {
		return nil, err
	}
	id, _ := strconv.Atoi(c.param("id"))
	for _, i := range r.Issues {
		for _, cm := range i.Comments {
			if cm.ID == id {
				return cm, nil
			}
		}
	}
	for _, p := range r.Pulls {
		for _, cm := range p.Comments {
			if cm.ID == id {
				return cm, nil
			}
		}
	}
	return nil, errNotFound()
}

// patchPull applies a PATCH's title, body, base and state to a pull
// request.
func (c *restCtx) patchPull(p *Pull) error {
	if c.has("title") {
		p.Title = c.str("title")
	}
	if c.has("body") {
		p.Body = c.str("body")
	}
	if c.has("base") {
		p.Base = c.str("base")
	}
	switch c.str("state") {
	case "closed":
		c.m.ClosePull(p)
	case "open":
		if err := c.m.ReopenPull(p); err != nil {
			return err
		}
	}
	p.UpdatedAt = c.m.now()
	return nil
}

func pulls() {
	handle("GET", "/repos/{owner}/{repo}/pulls", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		state := cmpOr(c.query.Get("state"), "open")
		head := c.query.Get("head")
		if _, branch, ok := strings.Cut(head, ":"); ok {
			head = branch
		}
		var out []*Pull
		for _, p := range r.Pulls {
			open := p.State == stateOpen
			if state != "all" && open != (state == "open") {
				continue
			}
			if head != "" && p.Head != head {
				continue
			}
			if base := c.query.Get("base"); base != "" && p.Base != base {
				continue
			}
			out = append(out, p)
		}
		desc := c.query.Get("direction") != "asc"
		updated := c.query.Get("sort") == "updated"
		if !updated && c.query.Get("direction") == "" {
			desc = true
		}
		slices.SortStableFunc(out, func(a, b *Pull) int {
			cmp := a.CreatedAt.Compare(b.CreatedAt)
			if updated {
				cmp = a.UpdatedAt.Compare(b.UpdatedAt)
			}
			if cmp == 0 {
				cmp = a.Number - b.Number
			}
			if desc {
				return -cmp
			}
			return cmp
		})
		return list(c, out, c.pullShape), nil
	})
	handle("POST", "/repos/{owner}/{repo}/pulls", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		head := c.str("head")
		if _, branch, ok := strings.Cut(head, ":"); ok {
			head = branch
		}
		if _, ok := c.m.Branch(r, head); !ok {
			return nil, errInvalid("head: invalid")
		}
		if _, ok := c.m.Branch(r, cmpOr(c.str("base"), r.DefaultBranch)); !ok {
			return nil, errInvalid("base: invalid")
		}
		p, err := c.m.AddPull(r, PullSpec{Title: c.str("title"), Body: c.str("body"), Base: c.str("base"), Head: head, Draft: c.flag("draft")})
		if err != nil {
			return nil, err
		}
		c.status = http.StatusCreated
		return c.pullShape(p), nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}", func(c *restCtx) (any, error) {
		r, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		if c.accepts("application/vnd.github.diff") || c.accepts("application/vnd.github.v3.diff") {
			if !c.m.Origins.Has(r) {
				return nil, unsupported("the diff of %s, whose repository has no origin", p.Ref())
			}
			diff, err := c.m.Origins.Diff(r, c.m.BaseSHA(p), c.m.HeadSHA(p))
			if err != nil {
				return nil, errNotFound()
			}
			return rawBody{ContentType: "application/vnd.github.diff; charset=utf-8", Data: []byte(diff)}, nil
		}
		return c.pullShape(p), nil
	})
	handle("PATCH", "/repos/{owner}/{repo}/pulls/{number}", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		if err := c.patchPull(p); err != nil {
			return nil, err
		}
		return c.pullShape(p), nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/merge", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		if p.State != stateMerged {
			return nil, errNotFound()
		}
		c.status = http.StatusNoContent
		return nil, nil
	})
	handle("PUT", "/repos/{owner}/{repo}/pulls/{number}/merge", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		err = c.m.Merge(p, MergeSpec{Method: c.str("merge_method"), Title: c.str("commit_title"), Body: c.str("commit_message"), SHA: c.str("sha"), Admin: true})
		if err != nil {
			return nil, err
		}
		return obj{"sha": p.MergeCommit, "merged": true, "message": "Pull Request successfully merged"}, nil
	})
	handle("PUT", "/repos/{owner}/{repo}/pulls/{number}/update-branch", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		if err := c.m.UpdateBranch(p, c.str("expected_head_sha"), false); err != nil {
			return nil, err
		}
		c.status = http.StatusAccepted
		return obj{"message": "Updating pull request branch.", "url": fmt.Sprintf("%s/pulls/%d", c.repoURL(p.Repo), p.Number)}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/reviews", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		return list(c, p.Reviews, c.reviewShape), nil
	})
	handle("POST", "/repos/{owner}/{repo}/pulls/{number}/reviews", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		state, ok := reviewEvents[c.str("event")]
		if !ok {
			return nil, errInvalid("event is not one of APPROVE, REQUEST_CHANGES, COMMENT")
		}
		if state != reviewCommented && strings.EqualFold(p.Author, c.m.Viewer) {
			return nil, errInvalid("Can not %s your own pull request", map[string]string{reviewApproved: "approve", reviewChanges: "request changes on"}[state])
		}
		return c.reviewShape(c.m.AddReview(p, "", state, c.str("body"), c.str("commit_id"))), nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/reviews/{id}", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		id, _ := strconv.Atoi(c.param("id"))
		for _, rv := range p.Reviews {
			if rv.ID == id {
				return c.reviewShape(rv), nil
			}
		}
		return nil, errNotFound()
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/reviews/{id}/comments", func(c *restCtx) (any, error) {
		if _, _, err := c.pull(); err != nil {
			return nil, err
		}
		return []any{}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/comments", func(c *restCtx) (any, error) {
		if _, _, err := c.pull(); err != nil {
			return nil, err
		}
		return []any{}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", func(c *restCtx) (any, error) {
		if _, _, err := c.pull(); err != nil {
			return nil, err
		}
		return obj{"users": []any{}, "teams": []any{}}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/commits", func(c *restCtx) (any, error) {
		r, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		return list(c, c.m.PullCommits(p), func(info CommitInfo) obj { return c.commitShape(r, info) }), nil
	})
	handle("GET", "/repos/{owner}/{repo}/pulls/{number}/files", func(c *restCtx) (any, error) {
		_, p, err := c.pull()
		if err != nil {
			return nil, err
		}
		return list(c, c.m.PullFiles(p), func(f PullFile) obj {
			return obj{"filename": f.Path, "status": "modified", "additions": f.Additions, "deletions": f.Deletions, "changes": f.Additions + f.Deletions}
		}), nil
	})
}

func checks() {
	check := func(c *restCtx) (*Repo, *CheckRun, error) {
		r, err := c.repo()
		if err != nil {
			return nil, nil, err
		}
		id, _ := strconv.Atoi(c.param("id"))
		for _, ch := range r.Checks {
			if ch.ID == id {
				return r, ch, nil
			}
		}
		return r, nil, errNotFound()
	}
	handle("GET", "/repos/{owner}/{repo}/check-runs/{id}", func(c *restCtx) (any, error) {
		_, ch, err := check(c)
		if err != nil {
			return nil, err
		}
		return c.checkShape(ch), nil
	})
	handle("GET", "/repos/{owner}/{repo}/check-runs/{id}/annotations", func(c *restCtx) (any, error) {
		if _, _, err := check(c); err != nil {
			return nil, err
		}
		return []any{}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/actions/workflows", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		return obj{"total_count": len(r.Workflows), "workflows": list(c, r.Workflows, func(w *Workflow) obj {
			return obj{"id": w.ID, "name": w.Name, "path": w.Path, "state": "active", "node_id": fmt.Sprintf("W_%d", w.ID)}
		})}, nil
	})
	run := func(c *restCtx) (*Repo, *WorkflowRun, error) {
		r, err := c.repo()
		if err != nil {
			return nil, nil, err
		}
		id, _ := strconv.Atoi(c.param("id"))
		for _, x := range r.Runs {
			if x.ID == id {
				return r, x, nil
			}
		}
		return r, nil, errNotFound()
	}
	handle("GET", "/repos/{owner}/{repo}/actions/runs", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		var out []*WorkflowRun
		for _, x := range r.Runs {
			if sha := c.query.Get("head_sha"); sha != "" && x.SHA != sha {
				continue
			}
			if branch := c.query.Get("branch"); branch != "" && x.Branch != branch {
				continue
			}
			if event := c.query.Get("event"); event != "" && x.Event != event {
				continue
			}
			if status := c.query.Get("status"); status != "" && x.Status != status && x.Conclusion != status {
				continue
			}
			out = append(out, x)
		}
		slices.Reverse(out)
		return obj{"total_count": len(out), "workflow_runs": list(c, out, c.runShape)}, nil
	})
	handle("GET", "/repos/{owner}/{repo}/actions/runs/{id}", func(c *restCtx) (any, error) {
		_, x, err := run(c)
		if err != nil {
			return nil, err
		}
		return c.runShape(x), nil
	})
	handle("GET", "/repos/{owner}/{repo}/actions/runs/{id}/jobs", func(c *restCtx) (any, error) {
		r, x, err := run(c)
		if err != nil {
			return nil, err
		}
		var jobs []*CheckRun
		for _, ch := range r.Checks {
			if ch.Run == x {
				jobs = append(jobs, ch)
			}
		}
		return obj{"total_count": len(jobs), "jobs": list(c, jobs, func(ch *CheckRun) obj {
			return obj{
				"id": ch.ID, "run_id": x.ID, "node_id": ch.NodeID, "head_sha": ch.SHA, "name": ch.Name, "workflow_name": x.Workflow.Name,
				"status": strings.ToLower(ch.Status), "conclusion": lower(ch.Conclusion), "run_attempt": x.Attempt,
				"started_at": ts(ch.StartedAt), "completed_at": tsp(ch.CompletedAt), "html_url": ch.URL(), "steps": []any{},
				"url": fmt.Sprintf("%s/actions/jobs/%d", c.repoURL(r), ch.ID),
			}
		})}, nil
	})
	rerun := func(c *restCtx) (any, error) {
		r, x, err := run(c)
		if err != nil {
			return nil, err
		}
		if x.Status != "completed" {
			return nil, &apiError{Status: http.StatusForbidden, Message: "This workflow is already running"}
		}
		failedOnly := strings.HasSuffix(c.req.URL.Path, "/rerun-failed-jobs")
		for _, ch := range r.Checks {
			if ch.Run == x && (!failedOnly || ch.Conclusion != conclusionSuccess) {
				ch.Status, ch.Conclusion, ch.CompletedAt, ch.StartedAt = checkQueued, "", nil, c.m.now()
			}
		}
		x.Attempt++
		c.m.settleRun(r, x)
		c.status = http.StatusCreated
		return obj{}, nil
	}
	handle("POST", "/repos/{owner}/{repo}/actions/runs/{id}/rerun", rerun)
	handle("POST", "/repos/{owner}/{repo}/actions/runs/{id}/rerun-failed-jobs", rerun)
	handle("GET", "/repos/{owner}/{repo}/actions/jobs/{id}/logs", func(c *restCtx) (any, error) {
		_, ch, err := check(c)
		if err != nil {
			return nil, err
		}
		log := fmt.Sprintf("%s\t%s\t%s %s\n", ch.Name, ch.Run.Workflow.Name, strings.ToLower(ch.Status), strings.ToLower(ch.Conclusion))
		return rawBody{ContentType: "text/plain; charset=utf-8", Data: []byte(log)}, nil
	})
}

func releases() {
	release := func(c *restCtx) (*Repo, *Release, error) {
		r, err := c.repo()
		if err != nil {
			return nil, nil, err
		}
		id, _ := strconv.Atoi(c.param("id"))
		for _, rel := range r.Releases {
			if rel.ID == id {
				return r, rel, nil
			}
		}
		return r, nil, errNotFound()
	}
	asset := func(c *restCtx) (*Release, *Asset, error) {
		r, err := c.repo()
		if err != nil {
			return nil, nil, err
		}
		id, _ := strconv.Atoi(c.param("id"))
		for _, rel := range r.Releases {
			for _, a := range rel.Assets {
				if a.ID == id {
					return rel, a, nil
				}
			}
		}
		return nil, nil, errNotFound()
	}
	handle("GET", "/repos/{owner}/{repo}/releases", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		return list(c, releasesNewestFirst(r), c.releaseShape), nil
	})
	handle("POST", "/repos/{owner}/{repo}/releases", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		if c.str("tag_name") == "" {
			return nil, errInvalid("tag_name can't be blank")
		}
		rel, err := c.m.AddRelease(r, Release{Tag: c.str("tag_name"), Name: c.str("name"), Body: c.str("body"),
			Draft: c.flag("draft"), Prerelease: c.flag("prerelease"), Target: c.str("target_commitish")})
		if err != nil {
			return nil, err
		}
		if _, tagged := c.m.Tag(r, rel.Tag); !tagged && !rel.Draft {
			if sha, ok := c.m.Commit(r, rel.Target); ok {
				if err := c.m.SetRef(r, "refs/tags/"+rel.Tag, sha); err != nil {
					return nil, err
				}
			}
		}
		c.status = http.StatusCreated
		return c.releaseShape(rel), nil
	})
	handle("GET", "/repos/{owner}/{repo}/releases/latest", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		rel := latestRelease(r)
		if rel == nil {
			return nil, errNotFound()
		}
		return c.releaseShape(rel), nil
	})
	handle("GET", "/repos/{owner}/{repo}/releases/tags/{tag...}", func(c *restCtx) (any, error) {
		r, err := c.repo()
		if err != nil {
			return nil, err
		}
		rel, ok := r.Release(c.param("tag"))
		if !ok || rel.Draft {
			return nil, errNotFound()
		}
		return c.releaseShape(rel), nil
	})
	handle("GET", "/repos/{owner}/{repo}/releases/assets/{id}", func(c *restCtx) (any, error) {
		rel, a, err := asset(c)
		if err != nil {
			return nil, err
		}
		if c.accepts("application/octet-stream") {
			data, err := a.Bytes()
			if err != nil {
				return nil, fmt.Errorf("release asset %s: %w", a.Name, err)
			}
			return rawBody{ContentType: a.ContentType, Data: data}, nil
		}
		return c.assetShape(rel)(a), nil
	})
	handle("DELETE", "/repos/{owner}/{repo}/releases/assets/{id}", func(c *restCtx) (any, error) {
		rel, a, err := asset(c)
		if err != nil {
			return nil, err
		}
		rel.Assets = slices.DeleteFunc(rel.Assets, func(o *Asset) bool { return o == a })
		c.status = http.StatusNoContent
		return nil, nil
	})
	handle("GET", "/repos/{owner}/{repo}/releases/{id}", func(c *restCtx) (any, error) {
		_, rel, err := release(c)
		if err != nil {
			return nil, err
		}
		return c.releaseShape(rel), nil
	})
	handle("PATCH", "/repos/{owner}/{repo}/releases/{id}", func(c *restCtx) (any, error) {
		_, rel, err := release(c)
		if err != nil {
			return nil, err
		}
		if c.has("name") {
			rel.Name = c.str("name")
		}
		if c.has("body") {
			rel.Body = c.str("body")
		}
		if c.has("prerelease") {
			rel.Prerelease = c.flag("prerelease")
		}
		if c.has("draft") {
			rel.Draft = c.flag("draft")
			if !rel.Draft && rel.PublishedAt == nil {
				t := c.m.now()
				rel.PublishedAt = &t
			}
		}
		return c.releaseShape(rel), nil
	})
	handle("DELETE", "/repos/{owner}/{repo}/releases/{id}", func(c *restCtx) (any, error) {
		r, rel, err := release(c)
		if err != nil {
			return nil, err
		}
		r.Releases = slices.DeleteFunc(r.Releases, func(o *Release) bool { return o == rel })
		c.status = http.StatusNoContent
		return nil, nil
	})
	handle("GET", "/repos/{owner}/{repo}/releases/{id}/assets", func(c *restCtx) (any, error) {
		_, rel, err := release(c)
		if err != nil {
			return nil, err
		}
		return list(c, rel.Assets, c.assetShape(rel)), nil
	})
	handle("POST", "/repos/{owner}/{repo}/releases/{id}/assets", func(c *restCtx) (any, error) {
		_, rel, err := release(c)
		if err != nil {
			return nil, err
		}
		name := c.query.Get("name")
		if name == "" {
			return nil, errInvalid("name can't be blank")
		}
		for _, a := range rel.Assets {
			if a.Name == name {
				return nil, &apiError{Status: http.StatusUnprocessableEntity, Message: "Validation Failed", Errors: []string{"already_exists"}}
			}
		}
		c.status = http.StatusCreated
		return c.assetShape(rel)(c.m.AddAsset(rel, name, c.req.Header.Get("Content-Type"), c.raw)), nil
	})
}
