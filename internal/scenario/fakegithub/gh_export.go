//go:build scenario

package fakegithub

import (
	"fmt"
	"slices"
	"strings"
)

// The --json fields of gh pr and gh issue: the GraphQL each selects (as
// the CLI's api.IssueGraphQL writes it) and the JSON gh exports of the
// answer (api.ExportData and the types' own encoding).

// sharedJSONFields are the fields of issues and pull requests both.
var sharedJSONFields = []string{
	"assignees", "author", "body", "closed", "comments", "createdAt", "closedAt", "id", "labels", "milestone", "number",
	"projectCards", "projectItems", "reactionGroups", "state", "title", "updatedAt", "url",
}

// issueOnlyJSONFields are the fields of issues alone.
var issueOnlyJSONFields = []string{
	"isPinned", "stateReason", "closedByPullRequestsReferences", "issueType", "parent", "subIssues", "subIssuesSummary", "blockedBy", "blocking",
}

var issueJSONFields = append(slices.Clone(sharedJSONFields), issueOnlyJSONFields...)

var pullJSONFields = append(slices.Clone(sharedJSONFields),
	"additions", "autoMergeRequest", "baseRefName", "baseRefOid", "changedFiles", "closingIssuesReferences", "commits", "deletions",
	"files", "fullDatabaseId", "headRefName", "headRefOid", "headRepository", "headRepositoryOwner", "isCrossRepository", "isDraft",
	"latestReviews", "maintainerCanModify", "mergeable", "mergeCommit", "mergedAt", "mergedBy", "mergeStateStatus",
	"potentialMergeCommit", "reviewDecision", "reviewRequests", "reviews", "statusCheckRollup",
)

const (
	gqlComments = `comments(first: 100) {nodes {id,author{login,...on User{id,name}},authorAssociation,body,createdAt,includesCreatedEdit,isMinimized,minimizedReason,reactionGroups{content,users{totalCount}},url,viewerDidAuthor},pageInfo{hasNextPage,endCursor},totalCount}`
	gqlReviews  = `reviews(first: 100) {nodes {id,author{login},authorAssociation,submittedAt,body,state,commit{oid},reactionGroups{content,users{totalCount}}}pageInfo{hasNextPage,endCursor}totalCount}`
	gqlLatest   = `latestReviews(first: 100) {nodes {author{login},authorAssociation,submittedAt,body,state}}`
	gqlFiles    = `files(first: 100) {nodes {additions,deletions,path,changeType}}`
	gqlCommits  = `commits(first: 100) {nodes {commit {authors(first:100) {nodes {name,email,user{id,login}}},messageHeadline,messageBody,oid,committedDate,authoredDate}}}`
	gqlClosing  = `closingIssuesReferences(first: 100) {nodes {id,number,url,repository {id,name,owner {id,login}}}pageInfo{hasNextPage,endCursor}}`
	gqlClosedBy = `closedByPullRequestsReferences(first: 100) {nodes {id,number,url,repository {id,name,owner {id,login}}}pageInfo{hasNextPage,endCursor}}`
	gqlRequests = `reviewRequests(first: 100) {nodes {requestedReviewer {__typename,...on User{login,name},...on Bot{login},...on Team{organization{login}name,slug}}}}`
	gqlRollup   = `statusCheckRollup: commits(last: 1) {nodes {commit {statusCheckRollup {contexts(first:100) {nodes {__typename ...on StatusContext {context,state,targetUrl,createdAt,description},...on CheckRun {name,checkSuite{workflowRun{workflow{name}}},status,conclusion,startedAt,completedAt,detailsUrl}},pageInfo{hasNextPage,endCursor}}}}}}`
	gqlAuto     = `autoMergeRequest {authorEmail,commitBody,commitHeadline,mergeMethod,enabledAt,enabledBy{login,...on User{id,name}}}`
	gqlLinked   = `{nodes{id,number,title,url,state,repository{nameWithOwner}},totalCount}`
)

// subjectGraphQL is the selection of the fields of an issue or a pull
// request.
func subjectGraphQL(fields []string) string {
	var q []string
	for _, field := range fields {
		switch field {
		case "author":
			q = append(q, `author{login,...on User{id,name}}`)
		case "mergedBy":
			q = append(q, `mergedBy{login,...on User{id,name}}`)
		case "headRepositoryOwner":
			q = append(q, `headRepositoryOwner{id,login,...on User{name}}`)
		case "headRepository":
			q = append(q, `headRepository{id,name,nameWithOwner}`)
		case "assignees":
			q = append(q, `assignees(first:100){nodes{id,login,name,databaseId},totalCount}`)
		case "labels":
			q = append(q, `labels(first:100){nodes{id,name,description,color},totalCount}`)
		case "projectCards":
			q = append(q, `projectCards(first:100){nodes{project{name}column{name}},totalCount}`)
		case "projectItems":
			q = append(q, `projectItems(first:100){nodes{id, project{id,title}, status:fieldValueByName(name: "Status") { ... on ProjectV2ItemFieldSingleSelectValue{optionId,name}}},totalCount}`)
		case "milestone":
			q = append(q, `milestone{number,title,description,dueOn}`)
		case "reactionGroups":
			q = append(q, `reactionGroups{content,users{totalCount}}`)
		case "mergeCommit":
			q = append(q, `mergeCommit{oid}`)
		case "potentialMergeCommit":
			q = append(q, `potentialMergeCommit{oid}`)
		case "autoMergeRequest":
			q = append(q, gqlAuto)
		case "comments":
			q = append(q, gqlComments)
		case "reviewRequests":
			q = append(q, gqlRequests)
		case "reviews":
			q = append(q, gqlReviews)
		case "latestReviews":
			q = append(q, gqlLatest)
		case "files":
			q = append(q, gqlFiles)
		case "commits":
			q = append(q, gqlCommits)
		case "statusCheckRollup":
			q = append(q, gqlRollup)
		case "closingIssuesReferences":
			q = append(q, gqlClosing)
		case "closedByPullRequestsReferences":
			q = append(q, gqlClosedBy)
		case "issueType":
			q = append(q, `issueType{id,name,description,color}`)
		case "parent":
			q = append(q, `parent{id,number,title,url,state,repository{nameWithOwner}}`)
		case "subIssues":
			q = append(q, `subIssues(first:100)`+gqlLinked)
		case "subIssuesSummary":
			q = append(q, `subIssuesSummary{total,completed,percentCompleted}`)
		case "blockedBy":
			q = append(q, `blockedBy(first:50)`+gqlLinked)
		case "blocking":
			q = append(q, `blocking(first:50)`+gqlLinked)
		default:
			q = append(q, field)
		}
	}
	return strings.Join(q, ",")
}

// node is a decoded GraphQL object.
type node = map[string]any

func child(n node, key string) node {
	c, _ := n[key].(map[string]any)
	return c
}

func nodesOf(n node, key string) []node {
	list, _ := child(n, key)["nodes"].([]any)
	out := make([]node, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// text is a string field; a null one is empty, as Go decodes it.
func text(n node, key string) string {
	s, _ := n[key].(string)
	return s
}

// zeroTime is how gh writes a time GitHub answered with null.
const zeroTime = "0001-01-01T00:00:00Z"

func timeOr(n node, key string) string { return cmpOr(text(n, key), zeroTime) }

// authorJSON is gh's Author: a bot is an account without a node id.
func authorJSON(a node) any {
	if text(a, "id") == "" {
		return node{"is_bot": true, "login": "app/" + text(a, "login")}
	}
	return node{"is_bot": false, "login": text(a, "login"), "id": text(a, "id"), "name": text(a, "name")}
}

func reactions(n node) []any {
	out := []any{}
	groups, _ := n["reactionGroups"].([]any)
	for _, g := range groups {
		m, _ := g.(map[string]any)
		if count, _ := child(m, "users")["totalCount"].(float64); count > 0 {
			out = append(out, node{"content": text(m, "content"), "users": node{"totalCount": int(count)}})
		}
	}
	return out
}

func linkedIssues(n node, key string) node {
	items := []any{}
	for _, l := range nodesOf(n, key) {
		items = append(items, node{"id": text(l, "id"), "number": l["number"], "title": text(l, "title"), "url": text(l, "url"), "state": text(l, "state")})
	}
	total, _ := child(n, key)["totalCount"].(float64)
	return node{"nodes": items, "totalCount": int(total)}
}

func references(n node, key string) []any {
	items := []any{}
	for _, r := range nodesOf(n, key) {
		repo := child(r, "repository")
		items = append(items, node{
			"id": text(r, "id"), "number": r["number"], "url": text(r, "url"),
			"repository": node{"id": text(repo, "id"), "name": text(repo, "name"),
				"owner": node{"id": text(child(repo, "owner"), "id"), "login": text(child(repo, "owner"), "login")}},
		})
	}
	return items
}

// stringFields are the fields gh holds as strings: null is "".
var stringFields = []string{"id", "title", "state", "url", "body", "baseRefName", "baseRefOid", "headRefName", "headRefOid",
	"mergeable", "mergeStateStatus", "reviewDecision", "fullDatabaseId", "stateReason"}

// exportSubject is gh's JSON of an issue or a pull request for fields.
func exportSubject(n node, fields []string) node {
	data := node{}
	for _, f := range fields {
		switch f {
		case "author":
			data[f] = authorJSON(child(n, f))
		case "mergedBy":
			if n[f] == nil {
				data[f] = nil
			} else {
				data[f] = authorJSON(child(n, f))
			}
		case "assignees":
			out := []any{}
			for _, a := range nodesOf(n, f) {
				id, _ := a["databaseId"].(float64)
				out = append(out, node{"id": text(a, "id"), "login": text(a, "login"), "name": text(a, "name"), "databaseId": int(id)})
			}
			data[f] = out
		case "labels":
			out := []any{}
			for _, l := range nodesOf(n, f) {
				out = append(out, node{"id": text(l, "id"), "name": text(l, "name"), "description": text(l, "description"), "color": text(l, "color")})
			}
			data[f] = out
		case "comments":
			out := []any{}
			for _, c := range nodesOf(n, f) {
				entry := node{
					"id": text(c, "id"), "author": node{"login": text(child(c, "author"), "login")}, "authorAssociation": text(c, "authorAssociation"),
					"body": text(c, "body"), "createdAt": timeOr(c, "createdAt"), "includesCreatedEdit": c["includesCreatedEdit"] == true,
					"isMinimized": c["isMinimized"] == true, "minimizedReason": text(c, "minimizedReason"), "reactionGroups": reactions(c),
					"viewerDidAuthor": c["viewerDidAuthor"] == true,
				}
				if u := text(c, "url"); u != "" {
					entry["url"] = u
				}
				out = append(out, entry)
			}
			data[f] = out
		case "reviews", "latestReviews":
			out := []any{}
			for _, r := range nodesOf(n, f) {
				out = append(out, node{
					"id": text(r, "id"), "author": node{"login": text(child(r, "author"), "login")}, "authorAssociation": text(r, "authorAssociation"),
					"body": text(r, "body"), "submittedAt": r["submittedAt"], "includesCreatedEdit": false, "reactionGroups": reactions(r),
					"state": text(r, "state"), "commit": node{"oid": text(child(r, "commit"), "oid")},
				})
			}
			data[f] = out
		case "files":
			out := []any{}
			for _, file := range nodesOf(n, f) {
				out = append(out, node{"path": text(file, "path"), "additions": file["additions"], "deletions": file["deletions"], "changeType": text(file, "changeType")})
			}
			data[f] = out
		case "commits":
			out := []any{}
			for _, c := range nodesOf(n, f) {
				commit := child(c, "commit")
				authors := []any{}
				for _, a := range nodesOf(commit, "authors") {
					authors = append(authors, node{"name": text(a, "name"), "email": text(a, "email"), "id": text(child(a, "user"), "id"), "login": text(child(a, "user"), "login")})
				}
				out = append(out, node{
					"oid": text(commit, "oid"), "messageHeadline": text(commit, "messageHeadline"), "messageBody": text(commit, "messageBody"),
					"committedDate": timeOr(commit, "committedDate"), "authoredDate": timeOr(commit, "authoredDate"), "authors": authors,
				})
			}
			data[f] = out
		case "statusCheckRollup":
			data[f] = exportRollup(n)
		case "closingIssuesReferences", "closedByPullRequestsReferences":
			data[f] = references(n, f)
		case "projectCards":
			data[f] = []any{}
		case "projectItems":
			out := []any{}
			for _, it := range nodesOf(n, f) {
				status := child(it, "status")
				out = append(out, node{"status": node{"optionId": text(status, "optionId"), "name": text(status, "name")}, "title": text(child(it, "project"), "title")})
			}
			data[f] = out
		case "reactionGroups":
			data[f] = reactions(n)
		case "reviewRequests":
			data[f] = []any{}
		case "mergeCommit", "potentialMergeCommit":
			if n[f] == nil {
				data[f] = nil
			} else {
				data[f] = node{"oid": text(child(n, f), "oid")}
			}
		case "headRepository":
			if n[f] == nil {
				data[f] = nil
			} else {
				r := child(n, f)
				data[f] = node{"id": text(r, "id"), "name": text(r, "name"), "nameWithOwner": text(r, "nameWithOwner")}
			}
		case "headRepositoryOwner":
			o := child(n, f)
			owner := node{"login": text(o, "login")}
			if id := text(o, "id"); id != "" {
				owner["id"] = id
			}
			if name := text(o, "name"); name != "" {
				owner["name"] = name
			}
			data[f] = owner
		case "autoMergeRequest":
			if n[f] == nil {
				data[f] = nil
			} else {
				a := child(n, f)
				data[f] = node{"authorEmail": a["authorEmail"], "commitBody": a["commitBody"], "commitHeadline": a["commitHeadline"],
					"mergeMethod": text(a, "mergeMethod"), "enabledAt": timeOr(a, "enabledAt"), "enabledBy": authorJSON(child(a, "enabledBy"))}
			}
		case "parent":
			if n[f] == nil {
				data[f] = nil
			} else {
				p := child(n, f)
				data[f] = node{"id": text(p, "id"), "number": p["number"], "title": text(p, "title"), "url": text(p, "url"), "state": text(p, "state")}
			}
		case "subIssues", "blockedBy", "blocking":
			data[f] = linkedIssues(n, f)
		case "subIssuesSummary":
			s := child(n, f)
			data[f] = node{"total": s["total"], "completed": s["completed"], "percentCompleted": s["percentCompleted"]}
		default:
			if slices.Contains(stringFields, f) {
				data[f] = text(n, f)
			} else {
				data[f] = n[f]
			}
		}
	}
	return data
}

// exportRollup is gh's statusCheckRollup: the contexts of the last commit.
func exportRollup(n node) any {
	commits := nodesOf(n, "statusCheckRollup")
	if len(commits) == 0 {
		return nil
	}
	rollup := child(child(commits[0], "commit"), "statusCheckRollup")
	checks := []any{}
	for _, c := range nodesOf(rollup, "contexts") {
		if text(c, "__typename") == "CheckRun" {
			workflow := child(child(child(c, "checkSuite"), "workflowRun"), "workflow")
			checks = append(checks, node{
				"__typename": "CheckRun", "name": text(c, "name"), "workflowName": text(workflow, "name"), "status": text(c, "status"),
				"conclusion": text(c, "conclusion"), "startedAt": timeOr(c, "startedAt"), "completedAt": timeOr(c, "completedAt"),
				"detailsUrl": text(c, "detailsUrl"),
			})
			continue
		}
		checks = append(checks, node{
			"__typename": text(c, "__typename"), "context": text(c, "context"), "state": text(c, "state"),
			"targetUrl": text(c, "targetUrl"), "startedAt": timeOr(c, "createdAt"),
		})
	}
	return checks
}

// stateWithDraft is a pull request's state as gh lists it: DRAFT for an
// open draft.
func stateWithDraft(n node) string {
	if text(n, "state") == stateOpen && n["isDraft"] == true {
		return "DRAFT"
	}
	return text(n, "state")
}

// names joins the names of a connection's nodes.
func names(n node, key, field string) string {
	var out []string
	for _, e := range nodesOf(n, key) {
		out = append(out, text(e, field))
	}
	return strings.Join(out, ", ")
}

func linkedRefs(list []node) string {
	var out []string
	for _, l := range list {
		out = append(out, fmt.Sprintf("%s#%v", text(child(l, "repository"), "nameWithOwner"), l["number"]))
	}
	return strings.Join(out, ", ")
}
