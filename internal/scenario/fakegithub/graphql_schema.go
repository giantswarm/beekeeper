//go:build scenario

package fakegithub

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The GraphQL schema the model serves: the types and fields beekeeper and
// gh read. A field outside it is not answered with null: the request is
// recorded as unsupported and fails the scenario, so a gap shows where it
// is and is closed here.

// resolver resolves one field of an object. It returns the value, a
// *gqlError for a field GitHub would null with an error, or another error
// to fail the request.
type resolver func(x *gqlExec, obj any, a args) any

// fieldsOf are a type's resolvers by field name.
type fieldsOf[T any] map[string]func(x *gqlExec, o T, a args) any

var schema = map[string]map[string]resolver{}

func define[T any](name string, groups ...fieldsOf[T]) {
	out := map[string]resolver{}
	for _, fields := range groups {
		for field, fn := range fields {
			out[field] = func(x *gqlExec, obj any, a args) any { return fn(x, obj.(T), a) }
		}
	}
	schema[name] = out
}

// abstractTypes are the interfaces and unions each object type belongs
// to, as far as a query's type conditions name them.
var abstractTypes = map[string][]string{
	"Issue":       {"Node", "Assignable", "Closable", "Comment", "Labelable", "Lockable", "Reactable", "RepositoryNode", "UniformResourceLocatable", "Updatable", "IssueOrPullRequest", "ProjectV2ItemContent", "SearchResultItem", "ReferencedSubject"},
	"PullRequest": {"Node", "Assignable", "Closable", "Comment", "Labelable", "Lockable", "Reactable", "RepositoryNode", "UniformResourceLocatable", "Updatable", "IssueOrPullRequest", "ProjectV2ItemContent", "SearchResultItem", "ReferencedSubject", "Closer"},
	"DraftIssue":  {"Node", "ProjectV2ItemContent"},
	kindUser:      {"Node", "Actor", "RepositoryOwner", "ProjectV2Owner", "Assignee", "RequestedReviewer", "UniformResourceLocatable", "ProfileOwner"},
	kindOrg:       {"Node", "Actor", "RepositoryOwner", "ProjectV2Owner", "UniformResourceLocatable", "ProfileOwner"},
	kindBot:       {"Node", "Actor", "Assignee", "RequestedReviewer", "UniformResourceLocatable"},
	"Repository":  {"Node", "RepositoryInfo", "Starrable", "Subscribable", "UniformResourceLocatable", "SearchResultItem"},
	"Commit":      {"Node", "GitObject", "UniformResourceLocatable", "Closer"},
	"CheckRun":    {"Node", "StatusCheckRollupContext", "RequirableByPullRequest", "UniformResourceLocatable"},
	"StatusContext": {
		"Node", "StatusCheckRollupContext", "RequirableByPullRequest",
	},
	"ProjectV2":                           {"Node", "Closable", "Updatable"},
	"ProjectV2Item":                       {"Node"},
	"ProjectV2Field":                      {"Node", "ProjectV2FieldCommon", "ProjectV2FieldConfiguration"},
	"ProjectV2SingleSelectField":          {"Node", "ProjectV2FieldCommon", "ProjectV2FieldConfiguration"},
	"ProjectV2ItemFieldSingleSelectValue": {"Node", "ProjectV2ItemFieldValueCommon", "ProjectV2ItemFieldValue"},
	"ProjectV2ItemFieldTextValue":         {"Node", "ProjectV2ItemFieldValueCommon", "ProjectV2ItemFieldValue"},
	"IssueComment":                        {"Node", "Comment", "Deletable", "Minimizable", "Reactable", "RepositoryNode", "Updatable", "UpdatableComment"},
	"PullRequestReview":                   {"Node", "Comment", "Deletable", "Reactable", "RepositoryNode", "Updatable", "UpdatableComment"},
	"Label":                               {"Node"},
	"Release":                             {"Node", "UniformResourceLocatable"},
	"Ref":                                 {"Node"},
	"ClosedEvent":                         {"Node", "UniformResourceLocatable", "IssueTimelineItems", "PullRequestTimelineItems"},
	"ReopenedEvent":                       {"Node", "IssueTimelineItems", "PullRequestTimelineItems"},
	"BranchProtectionRule":                {"Node"},
}

// The roots.
type queryRoot struct{}
type mutationRoot struct{}

func (queryRoot) gqlType() string    { return "Query" }
func (mutationRoot) gqlType() string { return "Mutation" }

func (*Repo) gqlType() string        { return "Repository" }
func (*Issue) gqlType() string       { return "Issue" }
func (*Pull) gqlType() string        { return "PullRequest" }
func (a *Account) gqlType() string   { return a.Kind }
func (*Label) gqlType() string       { return "Label" }
func (*Comment) gqlType() string     { return "IssueComment" }
func (*Review) gqlType() string      { return "PullRequestReview" }
func (*CheckRun) gqlType() string    { return "CheckRun" }
func (*Status) gqlType() string      { return "StatusContext" }
func (*WorkflowRun) gqlType() string { return "WorkflowRun" }
func (*Workflow) gqlType() string    { return "Workflow" }
func (*Release) gqlType() string     { return "Release" }
func (*Asset) gqlType() string       { return "ReleaseAsset" }
func (*Project) gqlType() string     { return "ProjectV2" }
func (*ProjectItem) gqlType() string { return "ProjectV2Item" }
func (*Protection) gqlType() string  { return "BranchProtectionRule" }

func (f *ProjectField) gqlType() string {
	if f.DataType == "SINGLE_SELECT" {
		return "ProjectV2SingleSelectField"
	}
	return "ProjectV2Field"
}

// commitObj is a commit of a repository.
type commitObj struct {
	repo *Repo
	sha  string
	info *CommitInfo
}

func (*commitObj) gqlType() string { return "Commit" }

// load reads the commit from the origin; without one it is a commit by the
// viewer at the repository's creation.
func (c *commitObj) load(x *gqlExec) CommitInfo {
	if c.info != nil {
		return *c.info
	}
	info := CommitInfo{SHA: c.sha, Author: x.m.Viewer, Email: x.m.Viewer + "@users.noreply.github.com", Committer: "GitHub",
		Authored: c.repo.CreatedAt, Committed: c.repo.CreatedAt, Subject: "commit " + c.sha}
	if x.m.Origins.Has(c.repo) {
		if got, err := x.m.Origins.CommitInfo(c.repo, c.sha); err == nil {
			info = got
		}
	}
	c.info = &info
	return info
}

// refObj is a branch or a tag.
type refObj struct {
	repo   *Repo
	prefix string // refs/heads/ or refs/tags/
	name   string
	sha    string
}

func (*refObj) gqlType() string { return "Ref" }

// rollupObj is a commit's status check rollup.
type rollupObj struct {
	repo *Repo
	sha  string
}

func (*rollupObj) gqlType() string { return "StatusCheckRollup" }

// suiteObj is the check suite of a workflow run.
type suiteObj struct{ run *WorkflowRun }

func (*suiteObj) gqlType() string { return "CheckSuite" }

// fieldValueObj is an item's value of a board field.
type fieldValueObj struct {
	item  *ProjectItem
	value *FieldValue
}

func (v *fieldValueObj) gqlType() string {
	if v.value.Field.DataType == "SINGLE_SELECT" {
		return "ProjectV2ItemFieldSingleSelectValue"
	}
	return "ProjectV2ItemFieldTextValue"
}

// draftObj is a draft item's content.
type draftObj struct{ item *ProjectItem }

func (*draftObj) gqlType() string { return "DraftIssue" }

// closedEvent and reopenedEvent are the timeline items the model keeps.
type closedEvent struct {
	issue *Issue
	pull  *Pull
}

func (*closedEvent) gqlType() string { return "ClosedEvent" }

type reopenedEvent struct{ at time.Time }

func (*reopenedEvent) gqlType() string { return "ReopenedEvent" }

// typeIntro is a type as introspection (__type) names it.
type typeIntro struct{ name string }

func (*typeIntro) gqlType() string { return "__Type" }

// gqlLogin is an account's login in GraphQL: a bot's without its [bot].
func gqlLogin(a *Account) string { return strings.TrimSuffix(a.Login, "[bot]") }

func (x *gqlExec) actor(login string) any {
	if login == "" {
		return nil
	}
	return x.m.Account(login, "")
}

func (x *gqlExec) actors(logins []string) []*Account {
	out := make([]*Account, 0, len(logins))
	for _, l := range logins {
		out = append(out, x.m.Account(l, ""))
	}
	return out
}

func (x *gqlExec) labels(r *Repo, names []string) []*Label {
	out := make([]*Label, 0, len(names))
	for _, n := range names {
		out = append(out, x.m.EnsureLabel(r, n, "", ""))
	}
	return out
}

func (x *gqlExec) ref(r *Repo, prefix, name string) any {
	var sha string
	var ok bool
	if prefix == "refs/tags/" {
		sha, ok = x.m.Tag(r, name)
	} else {
		sha, ok = x.m.Branch(r, name)
	}
	if !ok {
		return nil
	}
	return &refObj{repo: r, prefix: prefix, name: name, sha: sha}
}

func (x *gqlExec) commit(r *Repo, sha string) any {
	if sha == "" {
		return nil
	}
	return &commitObj{repo: r, sha: sha}
}

// association is the author's association with the repository: the
// viewer and the owner are members, anyone else a contributor.
func (x *gqlExec) association(r *Repo, login string) string {
	switch {
	case strings.EqualFold(login, r.Owner):
		return "OWNER"
	case strings.EqualFold(login, x.m.Viewer):
		return "MEMBER"
	}
	return "CONTRIBUTOR"
}

func upperStates(a args, name string) []string {
	out := a.strs(name)
	for i := range out {
		out[i] = strings.ToUpper(out[i])
	}
	return out
}

// orderBy sorts items by a connection's orderBy argument: CREATED_AT or
// UPDATED_AT, ASC or DESC.
func orderBy[T any](items []T, a args, created, updated func(T) time.Time) {
	o := a.obj("orderBy")
	if len(o) == 0 {
		return
	}
	at := created
	if o.str("field") == "UPDATED_AT" {
		at = updated
	}
	desc := o.str("direction") == "DESC"
	slices.SortStableFunc(items, func(p, q T) int {
		c := at(p).Compare(at(q))
		if desc {
			return -c
		}
		return c
	})
}

func init() {
	define("Query", fieldsOf[queryRoot]{
		"viewer": func(x *gqlExec, _ queryRoot, _ args) any { return x.m.Account(x.m.Viewer, "") },
		"rateLimit": func(x *gqlExec, _ queryRoot, _ args) any {
			return &payload{typ: "RateLimit", fields: map[string]any{
				"limit": x.rate.Limit, "remaining": x.rate.Remaining, "used": x.rate.Used,
				"cost": x.rate.Cost, "nodeCount": 0, "resetAt": x.rate.ResetAt,
			}}
		},
		"repository": func(x *gqlExec, _ queryRoot, a args) any {
			full := a.str("owner") + "/" + a.str("name")
			r, ok := x.m.Repo(full)
			if !ok {
				return notFound("Could not resolve to a Repository with the name '%s'.", full)
			}
			return r
		},
		"repositoryOwner": func(x *gqlExec, _ queryRoot, a args) any {
			acc, ok := x.m.LookupAccount(a.str("login"))
			if !ok || acc.Kind == kindBot {
				return nil
			}
			return acc
		},
		"organization": func(x *gqlExec, _ queryRoot, a args) any {
			acc, ok := x.m.LookupAccount(a.str("login"))
			if !ok || acc.Kind != kindOrg {
				return notFound("Could not resolve to an Organization with the login of '%s'.", a.str("login"))
			}
			return acc
		},
		"user": func(x *gqlExec, _ queryRoot, a args) any {
			acc, ok := x.m.LookupAccount(a.str("login"))
			if !ok || acc.Kind != kindUser {
				return notFound("Could not resolve to a User with the login of '%s'.", a.str("login"))
			}
			return acc
		},
		"node": func(x *gqlExec, _ queryRoot, a args) any {
			obj, ok := x.m.Node(a.str("id"))
			if !ok {
				return notFound("Could not resolve to a node with the global id of '%s'", a.str("id"))
			}
			return obj
		},
		"nodes": func(x *gqlExec, _ queryRoot, a args) any {
			var out []any
			for _, id := range a.strs("ids") {
				obj, _ := x.m.Node(id)
				out = append(out, obj)
			}
			return out
		},
		"search": func(x *gqlExec, _ queryRoot, a args) any {
			kind := a.str("type")
			if kind != "ISSUE" && kind != "ISSUE_ADVANCED" {
				return unsupported("GraphQL search of type %s", kind)
			}
			hits, err := x.m.Search(a.str("query"), kind == "ISSUE_ADVANCED")
			if err != nil {
				return err
			}
			c := connect("search", hits, a)
			c.extra = map[string]func() any{"issueCount": func() any { return len(hits) }}
			return c
		},
		"__type": func(_ *gqlExec, _ queryRoot, a args) any {
			if _, ok := schema[a.str("name")]; !ok {
				return nil
			}
			return &typeIntro{name: a.str("name")}
		},
	})

	define("__Type", fieldsOf[*typeIntro]{
		"name": func(_ *gqlExec, t *typeIntro, _ args) any { return t.name },
		"kind": func(_ *gqlExec, _ *typeIntro, _ args) any { return "OBJECT" },
		"fields": func(_ *gqlExec, t *typeIntro, _ args) any {
			names := make([]string, 0, len(schema[t.name]))
			for n := range schema[t.name] {
				names = append(names, n)
			}
			slices.Sort(names)
			out := make([]any, 0, len(names))
			for _, n := range names {
				out = append(out, &payload{typ: "__Field", fields: map[string]any{"name": n}})
			}
			return out
		},
	})

	accountFields := fieldsOf[*Account]{
		"login":      func(_ *gqlExec, a *Account, _ args) any { return gqlLogin(a) },
		"id":         func(_ *gqlExec, a *Account, _ args) any { return a.NodeID },
		"databaseId": func(_ *gqlExec, a *Account, _ args) any { return a.ID },
		"name": func(_ *gqlExec, a *Account, _ args) any {
			if a.Name == "" {
				return nil
			}
			return a.Name
		},
		"url":          func(_ *gqlExec, a *Account, _ args) any { return webHost + "/" + a.Login },
		"resourcePath": func(_ *gqlExec, a *Account, _ args) any { return "/" + a.Login },
		"avatarUrl": func(_ *gqlExec, a *Account, _ args) any {
			return fmt.Sprintf("https://avatars.githubusercontent.com/u/%d?v=4", a.ID)
		},
		"email": func(_ *gqlExec, _ *Account, _ args) any { return "" },
		"projectV2": func(x *gqlExec, a *Account, ar args) any {
			n, _ := ar.num("number")
			p, ok := x.m.Project(a.Login, n)
			if !ok {
				return notFound("Could not resolve to a ProjectV2 with the number %d.", n)
			}
			return p
		},
		"projectsV2": func(x *gqlExec, a *Account, ar args) any {
			var out []*Project
			for _, p := range x.m.Projects() {
				if strings.EqualFold(p.Owner, a.Login) {
					out = append(out, p)
				}
			}
			return connect("projectsV2", out, ar)
		},
		"repository": func(x *gqlExec, a *Account, ar args) any {
			r, ok := x.m.Repo(a.Login + "/" + ar.str("name"))
			if !ok {
				return nil
			}
			return r
		},
		"repositories": func(x *gqlExec, a *Account, ar args) any {
			var out []*Repo
			for _, r := range x.m.Repos() {
				if strings.EqualFold(r.Owner, a.Login) {
					out = append(out, r)
				}
			}
			return connect("repositories", out, ar)
		},
	}
	define(kindUser, accountFields)
	define(kindOrg, accountFields)
	define(kindBot, accountFields)

	define("Repository", fieldsOf[*Repo]{
		"id":            func(_ *gqlExec, r *Repo, _ args) any { return r.NodeID },
		"databaseId":    func(_ *gqlExec, r *Repo, _ args) any { return r.ID },
		"name":          func(_ *gqlExec, r *Repo, _ args) any { return r.Name },
		"nameWithOwner": func(_ *gqlExec, r *Repo, _ args) any { return r.Full() },
		"owner":         func(x *gqlExec, r *Repo, _ args) any { return x.m.Account(r.Owner, "") },
		"url":           func(_ *gqlExec, r *Repo, _ args) any { return r.URL() },
		"sshUrl":        func(_ *gqlExec, r *Repo, _ args) any { return "git@github.com:" + r.Full() + ".git" },
		"description": func(_ *gqlExec, r *Repo, _ args) any {
			if r.Description == "" {
				return nil
			}
			return r.Description
		},
		"isPrivate": func(_ *gqlExec, r *Repo, _ args) any { return r.Private },
		"visibility": func(_ *gqlExec, r *Repo, _ args) any {
			if r.Private {
				return "PRIVATE"
			}
			return "PUBLIC"
		},
		"isArchived":          func(_ *gqlExec, _ *Repo, _ args) any { return false },
		"isFork":              func(_ *gqlExec, _ *Repo, _ args) any { return false },
		"isTemplate":          func(_ *gqlExec, _ *Repo, _ args) any { return false },
		"isEmpty":             func(x *gqlExec, r *Repo, _ args) any { return len(x.m.BranchNames(r)) == 0 },
		"isInOrganization":    func(x *gqlExec, r *Repo, _ args) any { return x.m.Account(r.Owner, "").Kind == kindOrg },
		"parent":              func(_ *gqlExec, _ *Repo, _ args) any { return nil },
		"templateRepository":  func(_ *gqlExec, _ *Repo, _ args) any { return nil },
		"hasIssuesEnabled":    func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"hasProjectsEnabled":  func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"hasWikiEnabled":      func(_ *gqlExec, _ *Repo, _ args) any { return false },
		"mergeCommitAllowed":  func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"squashMergeAllowed":  func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"rebaseMergeAllowed":  func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"autoMergeAllowed":    func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"deleteBranchOnMerge": func(_ *gqlExec, _ *Repo, _ args) any { return false },
		"viewerPermission":    func(_ *gqlExec, _ *Repo, _ args) any { return "ADMIN" },
		"viewerCanAdminister": func(_ *gqlExec, _ *Repo, _ args) any { return true },
		"viewerDefaultMergeMethod": func(_ *gqlExec, _ *Repo, _ args) any {
			return "SQUASH"
		},
		"createdAt": func(_ *gqlExec, r *Repo, _ args) any { return r.CreatedAt },
		"pushedAt":  func(_ *gqlExec, r *Repo, _ args) any { return r.PushedAt },
		"updatedAt": func(_ *gqlExec, r *Repo, _ args) any { return r.PushedAt },
		"defaultBranchRef": func(x *gqlExec, r *Repo, _ args) any {
			return x.ref(r, "refs/heads/", r.DefaultBranch)
		},
		"ref": func(x *gqlExec, r *Repo, a args) any {
			name := a.str("qualifiedName")
			if strings.HasPrefix(name, "refs/tags/") {
				return x.ref(r, "refs/tags/", strings.TrimPrefix(name, "refs/tags/"))
			}
			return x.ref(r, "refs/heads/", strings.TrimPrefix(name, "refs/heads/"))
		},
		"refs": func(x *gqlExec, r *Repo, a args) any {
			prefix := a.str("refPrefix")
			names := x.m.BranchNames(r)
			if prefix == "refs/tags/" {
				names = x.m.TagNames(r)
			}
			var out []any
			for _, n := range names {
				out = append(out, x.ref(r, prefix, n))
			}
			return connect("refs", out, a)
		},
		"object": func(x *gqlExec, r *Repo, a args) any {
			ref := a.str("oid")
			if ref == "" {
				ref = a.str("expression")
			}
			sha, ok := x.m.Commit(r, ref)
			if !ok {
				return nil
			}
			return x.commit(r, sha)
		},
		"issue": func(_ *gqlExec, r *Repo, a args) any {
			n, _ := a.num("number")
			i, ok := r.Issue(n)
			if !ok {
				return notFound("Could not resolve to an Issue with the number of %d.", n)
			}
			return i
		},
		"pullRequest": func(_ *gqlExec, r *Repo, a args) any {
			n, _ := a.num("number")
			p, ok := r.Pull(n)
			if !ok {
				return notFound("Could not resolve to a PullRequest with the number of %d.", n)
			}
			return p
		},
		"issueOrPullRequest": func(_ *gqlExec, r *Repo, a args) any {
			n, _ := a.num("number")
			if i, ok := r.Issue(n); ok {
				return i
			}
			if p, ok := r.Pull(n); ok {
				return p
			}
			return notFound("Could not resolve to an issue or pull request with the number of %d.", n)
		},
		"issues": func(_ *gqlExec, r *Repo, a args) any {
			states, labels := upperStates(a, "states"), a.strs("labels")
			filter := a.obj("filterBy")
			states = append(states, upperStates(filter, "states")...)
			labels = append(labels, filter.strs("labels")...)
			var out []*Issue
			for _, i := range r.Issues {
				if len(states) > 0 && !slices.Contains(states, i.State) {
					continue
				}
				if !hasAll(i.Labels, labels) {
					continue
				}
				if who := filter.str("assignee"); who != "" && !containsFold(i.Assignees, who) {
					continue
				}
				if who := filter.str("createdBy"); who != "" && !strings.EqualFold(i.Author, who) {
					continue
				}
				out = append(out, i)
			}
			orderBy(out, a, func(i *Issue) time.Time { return i.CreatedAt }, func(i *Issue) time.Time { return i.UpdatedAt })
			return connect("issues", out, a)
		},
		"pullRequests": func(_ *gqlExec, r *Repo, a args) any {
			states, labels := upperStates(a, "states"), a.strs("labels")
			var out []*Pull
			for _, p := range r.Pulls {
				if len(states) > 0 && !slices.Contains(states, p.State) {
					continue
				}
				if b := a.str("baseRefName"); b != "" && p.Base != b {
					continue
				}
				if h := a.str("headRefName"); h != "" && p.Head != h {
					continue
				}
				if !hasAll(p.Labels, labels) {
					continue
				}
				out = append(out, p)
			}
			orderBy(out, a, func(p *Pull) time.Time { return p.CreatedAt }, func(p *Pull) time.Time { return p.UpdatedAt })
			return connect("pullRequests", out, a)
		},
		"label": func(_ *gqlExec, r *Repo, a args) any {
			l, ok := r.Label(a.str("name"))
			if !ok {
				return nil
			}
			return l
		},
		"labels": func(_ *gqlExec, r *Repo, a args) any {
			var out []*Label
			for _, l := range r.Labels {
				if q := a.str("query"); q == "" || strings.Contains(strings.ToLower(l.Name), strings.ToLower(q)) {
					out = append(out, l)
				}
			}
			return connect("labels", out, a)
		},
		"release": func(_ *gqlExec, r *Repo, a args) any {
			rel, ok := r.Release(a.str("tagName"))
			if !ok {
				return nil
			}
			return rel
		},
		"latestRelease": func(_ *gqlExec, r *Repo, _ args) any {
			rel := latestRelease(r)
			if rel == nil {
				return nil
			}
			return rel
		},
		"releases": func(_ *gqlExec, r *Repo, a args) any {
			out := releasesNewestFirst(r)
			if o := a.obj("orderBy"); o.str("direction") == "ASC" {
				slices.Reverse(out)
			}
			return connect("releases", out, a)
		},
		"mergeQueue": func(_ *gqlExec, r *Repo, a args) any {
			branch := cmpOr(a.str("branch"), r.DefaultBranch)
			if p, ok := r.Protection(branch); ok && p.MergeQueue {
				return &payload{typ: "MergeQueue", fields: map[string]any{"id": "MQ_" + p.NodeID, "url": r.URL() + "/queue/" + branch}}
			}
			return nil
		},
		"branchProtectionRules": func(_ *gqlExec, r *Repo, a args) any {
			return connect("branchProtectionRules", r.Protections, a)
		},
		"assignableUsers": func(x *gqlExec, r *Repo, a args) any {
			return connect("assignableUsers", []*Account{x.m.Account(x.m.Viewer, "")}, a)
		},
		"projectsV2": func(x *gqlExec, r *Repo, a args) any {
			var out []*Project
			for _, p := range x.m.Projects() {
				if strings.EqualFold(p.Owner, r.Owner) {
					out = append(out, p)
				}
			}
			return connect("projectsV2", out, a)
		},
		"issueTemplates":       func(_ *gqlExec, _ *Repo, _ args) any { return []any{} },
		"pullRequestTemplates": func(_ *gqlExec, _ *Repo, _ args) any { return []any{} },
		"milestones": func(_ *gqlExec, _ *Repo, a args) any {
			return connect("milestones", []any{}, a)
		},
	})

	define("Ref", fieldsOf[*refObj]{
		"id":     func(_ *gqlExec, r *refObj, _ args) any { return "REF_" + r.repo.NodeID + "_" + r.name },
		"name":   func(_ *gqlExec, r *refObj, _ args) any { return r.name },
		"prefix": func(_ *gqlExec, r *refObj, _ args) any { return r.prefix },
		"target": func(x *gqlExec, r *refObj, _ args) any { return x.commit(r.repo, r.sha) },
		"branchProtectionRule": func(_ *gqlExec, r *refObj, _ args) any {
			if p, ok := r.repo.Protection(r.name); ok && r.prefix == "refs/heads/" {
				return p
			}
			return nil
		},
		"associatedPullRequests": func(_ *gqlExec, r *refObj, a args) any {
			states := upperStates(a, "states")
			var out []*Pull
			for _, p := range r.repo.Pulls {
				if p.Head == r.name && (len(states) == 0 || slices.Contains(states, p.State)) {
					out = append(out, p)
				}
			}
			return connect("associatedPullRequests", out, a)
		},
	})

	define("BranchProtectionRule", fieldsOf[*Protection]{
		"id":                           func(_ *gqlExec, p *Protection, _ args) any { return p.NodeID },
		"pattern":                      func(_ *gqlExec, p *Protection, _ args) any { return p.Branch },
		"requiresStrictStatusChecks":   func(_ *gqlExec, p *Protection, _ args) any { return p.Strict },
		"requiresStatusChecks":         func(_ *gqlExec, p *Protection, _ args) any { return len(p.RequiredChecks) > 0 },
		"requiredStatusCheckContexts":  func(_ *gqlExec, p *Protection, _ args) any { return slices.Clone(p.RequiredChecks) },
		"requiresApprovingReviews":     func(_ *gqlExec, p *Protection, _ args) any { return p.RequiredReviews > 0 },
		"requiredApprovingReviewCount": func(_ *gqlExec, p *Protection, _ args) any { return p.RequiredReviews },
	})

	define("Commit", fieldsOf[*commitObj]{
		"id":              func(_ *gqlExec, c *commitObj, _ args) any { return "C_" + c.sha },
		"oid":             func(_ *gqlExec, c *commitObj, _ args) any { return c.sha },
		"abbreviatedOid":  func(_ *gqlExec, c *commitObj, _ args) any { return c.sha[:min(7, len(c.sha))] },
		"url":             func(_ *gqlExec, c *commitObj, _ args) any { return c.repo.URL() + "/commit/" + c.sha },
		"committedDate":   func(x *gqlExec, c *commitObj, _ args) any { return c.load(x).Committed },
		"authoredDate":    func(x *gqlExec, c *commitObj, _ args) any { return c.load(x).Authored },
		"message":         func(x *gqlExec, c *commitObj, _ args) any { return c.load(x).Message() },
		"messageHeadline": func(x *gqlExec, c *commitObj, _ args) any { return c.load(x).Subject },
		"messageBody":     func(x *gqlExec, c *commitObj, _ args) any { return c.load(x).Body },
		"authors": func(x *gqlExec, c *commitObj, a args) any {
			info := c.load(x)
			author := &payload{typ: "GitActor", fields: map[string]any{"name": info.Author, "email": info.Email, "user": nil}}
			return connect("authors", []any{author}, a)
		},
		"author": func(x *gqlExec, c *commitObj, _ args) any {
			info := c.load(x)
			return &payload{typ: "GitActor", fields: map[string]any{"name": info.Author, "email": info.Email, "date": info.Authored, "user": nil}}
		},
		"parents": func(x *gqlExec, c *commitObj, a args) any {
			var out []any
			for _, p := range c.load(x).Parents {
				out = append(out, x.commit(c.repo, p))
			}
			return connect("parents", out, a)
		},
		"statusCheckRollup": func(_ *gqlExec, c *commitObj, _ args) any {
			if c.repo.Rollup(c.sha) == "" {
				return nil
			}
			return &rollupObj{repo: c.repo, sha: c.sha}
		},
		"checkSuites": func(_ *gqlExec, c *commitObj, a args) any {
			var out []any
			for _, run := range c.repo.Runs {
				if run.SHA == c.sha {
					out = append(out, &suiteObj{run: run})
				}
			}
			return connect("checkSuites", out, a)
		},
		"associatedPullRequests": func(x *gqlExec, c *commitObj, a args) any {
			var out []*Pull
			for _, p := range c.repo.Pulls {
				if p.MergeCommit == c.sha || x.m.HeadSHA(p) == c.sha {
					out = append(out, p)
				}
			}
			return connect("associatedPullRequests", out, a)
		},
	})

	define("StatusCheckRollup", fieldsOf[*rollupObj]{
		"id":    func(_ *gqlExec, r *rollupObj, _ args) any { return "SCR_" + r.sha },
		"state": func(_ *gqlExec, r *rollupObj, _ args) any { return r.repo.Rollup(r.sha) },
		"contexts": func(_ *gqlExec, r *rollupObj, a args) any {
			runs, statuses := r.repo.Contexts(r.sha)
			var out []any
			for _, c := range runs {
				out = append(out, c)
			}
			for _, s := range statuses {
				out = append(out, s)
			}
			c := connect("contexts", out, a)
			c.extra = map[string]func() any{
				"checkRunCount":      func() any { return len(runs) },
				"statusContextCount": func() any { return len(statuses) },
				"checkRunCountsByState": func() any {
					return countsByState(len(runs), func(i int) string { return checkRunState(runs[i]) })
				},
				"statusContextCountsByState": func() any {
					return countsByState(len(statuses), func(i int) string { return statuses[i].State })
				},
			}
			return c
		},
	})

	define("CheckRun", fieldsOf[*CheckRun]{
		"id":         func(_ *gqlExec, c *CheckRun, _ args) any { return c.NodeID },
		"databaseId": func(_ *gqlExec, c *CheckRun, _ args) any { return c.ID },
		"name":       func(_ *gqlExec, c *CheckRun, _ args) any { return c.Name },
		"status":     func(_ *gqlExec, c *CheckRun, _ args) any { return c.Status },
		"conclusion": func(_ *gqlExec, c *CheckRun, _ args) any {
			if c.Conclusion == "" {
				return nil
			}
			return c.Conclusion
		},
		"detailsUrl":  func(_ *gqlExec, c *CheckRun, _ args) any { return c.URL() },
		"url":         func(_ *gqlExec, c *CheckRun, _ args) any { return c.URL() },
		"startedAt":   func(_ *gqlExec, c *CheckRun, _ args) any { return c.StartedAt },
		"completedAt": func(_ *gqlExec, c *CheckRun, _ args) any { return c.CompletedAt },
		"checkSuite":  func(_ *gqlExec, c *CheckRun, _ args) any { return &suiteObj{run: c.Run} },
		"isRequired": func(_ *gqlExec, c *CheckRun, _ args) any {
			for _, p := range c.repo.Protections {
				if slices.Contains(p.RequiredChecks, c.Name) {
					return true
				}
			}
			return false
		},
	})

	define("StatusContext", fieldsOf[*Status]{
		"id":      func(_ *gqlExec, s *Status, _ args) any { return s.NodeID },
		"context": func(_ *gqlExec, s *Status, _ args) any { return s.Context },
		"state":   func(_ *gqlExec, s *Status, _ args) any { return s.State },
		"targetUrl": func(_ *gqlExec, s *Status, _ args) any {
			if s.TargetURL == "" {
				return nil
			}
			return s.TargetURL
		},
		"description": func(_ *gqlExec, s *Status, _ args) any { return s.Description },
		"createdAt":   func(_ *gqlExec, s *Status, _ args) any { return s.CreatedAt },
		"isRequired":  func(_ *gqlExec, _ *Status, _ args) any { return false },
	})

	define("CheckSuite", fieldsOf[*suiteObj]{
		"id":         func(_ *gqlExec, s *suiteObj, _ args) any { return fmt.Sprintf("CS_%d", s.run.ID) },
		"databaseId": func(_ *gqlExec, s *suiteObj, _ args) any { return s.run.ID },
		"status": func(_ *gqlExec, s *suiteObj, _ args) any {
			return strings.ToUpper(s.run.Status)
		},
		"conclusion": func(_ *gqlExec, s *suiteObj, _ args) any {
			if s.run.Conclusion == "" {
				return nil
			}
			return strings.ToUpper(s.run.Conclusion)
		},
		"workflowRun": func(_ *gqlExec, s *suiteObj, _ args) any { return s.run },
	})

	define("WorkflowRun", fieldsOf[*WorkflowRun]{
		"id":         func(_ *gqlExec, r *WorkflowRun, _ args) any { return fmt.Sprintf("WFR_%d", r.ID) },
		"databaseId": func(_ *gqlExec, r *WorkflowRun, _ args) any { return r.ID },
		"runNumber":  func(_ *gqlExec, r *WorkflowRun, _ args) any { return r.Number },
		"runAttempt": func(_ *gqlExec, r *WorkflowRun, _ args) any { return r.Attempt },
		"url":        func(_ *gqlExec, r *WorkflowRun, _ args) any { return r.URL() },
		"event":      func(_ *gqlExec, r *WorkflowRun, _ args) any { return r.Event },
		"workflow":   func(_ *gqlExec, r *WorkflowRun, _ args) any { return r.Workflow },
	})

	define("Workflow", fieldsOf[*Workflow]{
		"id":         func(_ *gqlExec, w *Workflow, _ args) any { return fmt.Sprintf("W_%d", w.ID) },
		"databaseId": func(_ *gqlExec, w *Workflow, _ args) any { return w.ID },
		"name":       func(_ *gqlExec, w *Workflow, _ args) any { return w.Name },
	})

	define("Label", fieldsOf[*Label]{
		"id":          func(_ *gqlExec, l *Label, _ args) any { return l.NodeID },
		"name":        func(_ *gqlExec, l *Label, _ args) any { return l.Name },
		"color":       func(_ *gqlExec, l *Label, _ args) any { return l.Color },
		"description": func(_ *gqlExec, l *Label, _ args) any { return l.Description },
	})

	define("IssueComment", fieldsOf[*Comment]{
		"id":         func(_ *gqlExec, c *Comment, _ args) any { return c.NodeID },
		"databaseId": func(_ *gqlExec, c *Comment, _ args) any { return c.ID },
		"author":     func(x *gqlExec, c *Comment, _ args) any { return x.actor(c.Author) },
		"authorAssociation": func(x *gqlExec, c *Comment, _ args) any {
			if c.issue != nil {
				return x.association(c.issue.Repo, c.Author)
			}
			return x.association(c.pull.Repo, c.Author)
		},
		"body":                func(_ *gqlExec, c *Comment, _ args) any { return c.Body },
		"createdAt":           func(_ *gqlExec, c *Comment, _ args) any { return c.CreatedAt },
		"updatedAt":           func(_ *gqlExec, c *Comment, _ args) any { return c.UpdatedAt },
		"url":                 func(_ *gqlExec, c *Comment, _ args) any { return c.URL() },
		"includesCreatedEdit": func(_ *gqlExec, _ *Comment, _ args) any { return false },
		"isMinimized":         func(_ *gqlExec, _ *Comment, _ args) any { return false },
		"minimizedReason":     func(_ *gqlExec, _ *Comment, _ args) any { return "" },
		"reactionGroups":      func(_ *gqlExec, _ *Comment, _ args) any { return []any{} },
		"viewerDidAuthor":     func(x *gqlExec, c *Comment, _ args) any { return strings.EqualFold(c.Author, x.m.Viewer) },
	})

	define("PullRequestReview", fieldsOf[*Review]{
		"id":                  func(_ *gqlExec, r *Review, _ args) any { return r.NodeID },
		"databaseId":          func(_ *gqlExec, r *Review, _ args) any { return r.ID },
		"fullDatabaseId":      func(_ *gqlExec, r *Review, _ args) any { return fmt.Sprint(r.ID) },
		"author":              func(x *gqlExec, r *Review, _ args) any { return x.actor(r.Author) },
		"authorAssociation":   func(x *gqlExec, r *Review, _ args) any { return x.association(r.pull.Repo, r.Author) },
		"state":               func(_ *gqlExec, r *Review, _ args) any { return r.State },
		"body":                func(_ *gqlExec, r *Review, _ args) any { return r.Body },
		"submittedAt":         func(_ *gqlExec, r *Review, _ args) any { return r.SubmittedAt },
		"createdAt":           func(_ *gqlExec, r *Review, _ args) any { return r.SubmittedAt },
		"updatedAt":           func(_ *gqlExec, r *Review, _ args) any { return r.SubmittedAt },
		"url":                 func(_ *gqlExec, r *Review, _ args) any { return r.URL() },
		"commit":              func(x *gqlExec, r *Review, _ args) any { return x.commit(r.pull.Repo, r.Commit) },
		"includesCreatedEdit": func(_ *gqlExec, _ *Review, _ args) any { return false },
		"reactionGroups":      func(_ *gqlExec, _ *Review, _ args) any { return []any{} },
		"comments": func(_ *gqlExec, _ *Review, a args) any {
			return connect("comments", []any{}, a)
		},
	})

	define("Release", fieldsOf[*Release]{
		"id":           func(_ *gqlExec, r *Release, _ args) any { return r.NodeID },
		"databaseId":   func(_ *gqlExec, r *Release, _ args) any { return r.ID },
		"name":         func(_ *gqlExec, r *Release, _ args) any { return r.Name },
		"tagName":      func(_ *gqlExec, r *Release, _ args) any { return r.Tag },
		"description":  func(_ *gqlExec, r *Release, _ args) any { return r.Body },
		"isDraft":      func(_ *gqlExec, r *Release, _ args) any { return r.Draft },
		"isPrerelease": func(_ *gqlExec, r *Release, _ args) any { return r.Prerelease },
		"isLatest":     func(_ *gqlExec, r *Release, _ args) any { return latestRelease(r.repo) == r },
		"createdAt":    func(_ *gqlExec, r *Release, _ args) any { return r.CreatedAt },
		"publishedAt":  func(_ *gqlExec, r *Release, _ args) any { return r.PublishedAt },
		"url":          func(_ *gqlExec, r *Release, _ args) any { return r.URL() },
		"author":       func(x *gqlExec, r *Release, _ args) any { return x.actor(r.Author) },
		"tagCommit": func(x *gqlExec, r *Release, _ args) any {
			sha, _ := x.m.Tag(r.repo, r.Tag)
			return x.commit(r.repo, sha)
		},
		"releaseAssets": func(_ *gqlExec, r *Release, a args) any {
			return connect("releaseAssets", r.Assets, a)
		},
	})

	define("ReleaseAsset", fieldsOf[*Asset]{
		"id":          func(_ *gqlExec, a *Asset, _ args) any { return a.NodeID },
		"name":        func(_ *gqlExec, a *Asset, _ args) any { return a.Name },
		"size":        func(_ *gqlExec, a *Asset, _ args) any { return len(a.Content) },
		"contentType": func(_ *gqlExec, a *Asset, _ args) any { return a.ContentType },
		"createdAt":   func(_ *gqlExec, a *Asset, _ args) any { return a.CreatedAt },
	})

	defineIssues()
	defineProjects()
	defineMutations()
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !containsFold(have, w) {
			return false
		}
	}
	return true
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return strings.EqualFold(e, s) })
}

// checkRunState is a check run's state as the rollup counts it: its
// conclusion once completed, else its status.
func checkRunState(c *CheckRun) string {
	if c.Status == checkCompleted {
		return c.Conclusion
	}
	return c.Status
}

func countsByState(n int, state func(int) string) []any {
	var order []string
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		s := state(i)
		if _, seen := counts[s]; !seen {
			order = append(order, s)
		}
		counts[s]++
	}
	out := make([]any, 0, len(order))
	for _, s := range order {
		out = append(out, &payload{typ: "CountByState", fields: map[string]any{"state": s, "count": counts[s]}})
	}
	return out
}

// releasesNewestFirst are a repository's releases, the newest first.
func releasesNewestFirst(r *Repo) []*Release {
	out := slices.Clone(r.Releases)
	// Of two releases of the same second the one added later is newer.
	slices.Reverse(out)
	slices.SortStableFunc(out, func(a, b *Release) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out
}

// latestRelease is the newest release that is neither a draft nor a
// prerelease.
func latestRelease(r *Repo) *Release {
	for _, rel := range releasesNewestFirst(r) {
		if !rel.Draft && !rel.Prerelease {
			return rel
		}
	}
	return nil
}
