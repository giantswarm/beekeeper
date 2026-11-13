//go:build scenario

package fakegithub

import (
	"errors"
	"slices"
	"strings"
)

// unprocessable is a mutation GitHub refuses.
func unprocessable(format string, args ...any) *gqlError {
	e := notFound(format, args...)
	e.Type = "UNPROCESSABLE"
	return e
}

// asGQL turns a model's refusal into a mutation's error.
func asGQL(err error) *gqlError {
	var api *apiError
	if errors.As(err, &api) {
		msg := api.Message
		if len(api.Errors) > 0 {
			msg = strings.Join(api.Errors, ", ")
		}
		return &gqlError{Type: "UNPROCESSABLE", Message: msg}
	}
	return &gqlError{Type: "UNPROCESSABLE", Message: err.Error()}
}

// nodeOf is the node of an id as a T.
func nodeOf[T any](x *gqlExec, id, what string) (T, *gqlError) {
	var zero T
	obj, ok := x.m.Node(id)
	if !ok {
		return zero, notFound("Could not resolve to a node with the global id of '%s'", id)
	}
	t, ok := obj.(T)
	if !ok {
		return zero, notFound("Could not resolve to %s node with the global id of '%s'", what, id)
	}
	return t, nil
}

// subjectNode is the issue or pull request of an id.
func subjectNode(x *gqlExec, id string) (*Issue, *Pull, *gqlError) {
	obj, ok := x.m.Node(id)
	if !ok {
		return nil, nil, notFound("Could not resolve to a node with the global id of '%s'", id)
	}
	switch t := obj.(type) {
	case *Issue:
		return t, nil, nil
	case *Pull:
		return nil, t, nil
	}
	return nil, nil, notFound("Could not resolve to an issue or pull request with the global id of '%s'", id)
}

func subjectValue(i *Issue, p *Pull) any {
	if i != nil {
		return i
	}
	return p
}

func result(typ string, fields map[string]any) *payload {
	fields["clientMutationId"] = nil
	return &payload{typ: typ, fields: fields}
}

// positionPayload is updateProjectV2ItemPosition's payload: its items are
// the board's, in their new order.
type positionPayload struct{ project *Project }

func (*positionPayload) gqlType() string { return "UpdateProjectV2ItemPositionPayload" }

// mutation is one mutation: its input, its payload.
type mutation func(x *gqlExec, in args) any

func defineMutations() {
	define("UpdateProjectV2ItemPositionPayload", fieldsOf[*positionPayload]{
		"clientMutationId": func(_ *gqlExec, _ *positionPayload, _ args) any { return nil },
		"items": func(_ *gqlExec, p *positionPayload, a args) any {
			return connect("items", p.project.Items, a)
		},
	})

	mutations := map[string]mutation{
		"addProjectV2ItemById": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Project](x, in.str("projectId"), "a ProjectV2")
			if gerr != nil {
				return gerr
			}
			i, pr, gerr := subjectNode(x, in.str("contentId"))
			if gerr != nil {
				return gerr
			}
			return result("AddProjectV2ItemByIdPayload", map[string]any{"item": x.m.AddItem(p, i, pr)})
		},
		"updateProjectV2ItemFieldValue": func(x *gqlExec, in args) any {
			it, gerr := nodeOf[*ProjectItem](x, in.str("itemId"), "a ProjectV2Item")
			if gerr != nil {
				return gerr
			}
			f, gerr := nodeOf[*ProjectField](x, in.str("fieldId"), "a ProjectV2Field")
			if gerr != nil {
				return gerr
			}
			value := in.obj("value")
			switch {
			case value.has("singleSelectOptionId"):
				var option string
				for _, o := range f.Options {
					if o.ID == value.str("singleSelectOptionId") {
						option = o.Name
					}
				}
				if option == "" {
					return unprocessable("The single select option Id does not belong to the field")
				}
				if err := x.m.SetItemOption(it, f.Name, option); err != nil {
					return asGQL(err)
				}
			case value.has("text"):
				it.Values[key(f.Name)] = &FieldValue{Field: f, Text: value.str("text"), UpdatedAt: x.m.now()}
				it.UpdatedAt = x.m.now()
			default:
				return unsupported("updateProjectV2ItemFieldValue with a value other than singleSelectOptionId or text")
			}
			return result("UpdateProjectV2ItemFieldValuePayload", map[string]any{"projectV2Item": it})
		},
		"clearProjectV2ItemFieldValue": func(x *gqlExec, in args) any {
			it, gerr := nodeOf[*ProjectItem](x, in.str("itemId"), "a ProjectV2Item")
			if gerr != nil {
				return gerr
			}
			f, gerr := nodeOf[*ProjectField](x, in.str("fieldId"), "a ProjectV2Field")
			if gerr != nil {
				return gerr
			}
			delete(it.Values, key(f.Name))
			it.UpdatedAt = x.m.now()
			return result("ClearProjectV2ItemFieldValuePayload", map[string]any{"projectV2Item": it})
		},
		"updateProjectV2ItemPosition": func(x *gqlExec, in args) any {
			it, gerr := nodeOf[*ProjectItem](x, in.str("itemId"), "a ProjectV2Item")
			if gerr != nil {
				return gerr
			}
			if it.Archived {
				return unprocessable("The item is archived and cannot be updated")
			}
			p := it.Project
			items := slices.DeleteFunc(slices.Clone(p.Items), func(o *ProjectItem) bool { return o == it })
			at := 0
			if after := in.str("afterId"); after != "" {
				prev, gerr := nodeOf[*ProjectItem](x, after, "a ProjectV2Item")
				if gerr != nil {
					return gerr
				}
				if prev.Archived {
					return unprocessable("Archived items cannot be used as a position")
				}
				at = slices.Index(items, prev) + 1
			}
			p.Items = slices.Insert(items, at, it)
			return &positionPayload{project: p}
		},
		"archiveProjectV2Item": func(x *gqlExec, in args) any {
			it, gerr := nodeOf[*ProjectItem](x, in.str("itemId"), "a ProjectV2Item")
			if gerr != nil {
				return gerr
			}
			it.Archived, it.UpdatedAt = true, x.m.now()
			return result("ArchiveProjectV2ItemPayload", map[string]any{"item": it})
		},
		"unarchiveProjectV2Item": func(x *gqlExec, in args) any {
			it, gerr := nodeOf[*ProjectItem](x, in.str("itemId"), "a ProjectV2Item")
			if gerr != nil {
				return gerr
			}
			it.Archived, it.UpdatedAt = false, x.m.now()
			return result("UnarchiveProjectV2ItemPayload", map[string]any{"item": it})
		},
		"deleteProjectV2Item": func(x *gqlExec, in args) any {
			it, gerr := nodeOf[*ProjectItem](x, in.str("itemId"), "a ProjectV2Item")
			if gerr != nil {
				return gerr
			}
			it.Project.Items = slices.DeleteFunc(it.Project.Items, func(o *ProjectItem) bool { return o == it })
			return result("DeleteProjectV2ItemPayload", map[string]any{"deletedItemId": it.NodeID})
		},
		"addComment": func(x *gqlExec, in args) any {
			i, pr, gerr := subjectNode(x, in.str("subjectId"))
			if gerr != nil {
				return gerr
			}
			c := x.m.AddComment(i, pr, "", in.str("body"))
			edge := &payload{typ: "IssueCommentEdge", fields: map[string]any{"node": c, "cursor": cursorOf(0)}}
			return result("AddCommentPayload", map[string]any{"commentEdge": edge, "subject": subjectValue(i, pr)})
		},
		"addBlockedBy": func(x *gqlExec, in args) any {
			i, blocker, gerr := issuePair(x, in.str("issueId"), in.str("blockingIssueId"))
			if gerr != nil {
				return gerr
			}
			if !slices.Contains(i.BlockedBy, blocker) {
				i.BlockedBy = append(i.BlockedBy, blocker)
			}
			return result("AddBlockedByPayload", map[string]any{"issue": i, "blockingIssue": blocker})
		},
		"removeBlockedBy": func(x *gqlExec, in args) any {
			i, blocker, gerr := issuePair(x, in.str("issueId"), in.str("blockingIssueId"))
			if gerr != nil {
				return gerr
			}
			i.BlockedBy = slices.DeleteFunc(i.BlockedBy, func(o *Issue) bool { return o == blocker })
			return result("RemoveBlockedByPayload", map[string]any{"issue": i, "blockingIssue": blocker})
		},
		"addSubIssue": func(x *gqlExec, in args) any {
			i, sub, gerr := issuePair(x, in.str("issueId"), in.str("subIssueId"))
			if gerr != nil {
				return gerr
			}
			if sub.Parent != nil && sub.Parent != i && !in.flag("replaceParent") {
				return unprocessable("Sub issue may only have one parent")
			}
			x.m.SetParent(sub, i)
			return result("AddSubIssuePayload", map[string]any{"issue": i, "subIssue": sub})
		},
		"removeSubIssue": func(x *gqlExec, in args) any {
			i, sub, gerr := issuePair(x, in.str("issueId"), in.str("subIssueId"))
			if gerr != nil {
				return gerr
			}
			if sub.Parent == i {
				x.m.SetParent(sub, nil)
			}
			return result("RemoveSubIssuePayload", map[string]any{"issue": i, "subIssue": sub})
		},
		"createIssue": func(x *gqlExec, in args) any {
			r, gerr := nodeOf[*Repo](x, in.str("repositoryId"), "a Repository")
			if gerr != nil {
				return gerr
			}
			i, err := x.m.AddIssue(r, IssueSpec{Title: in.str("title"), Body: in.str("body")})
			if err != nil {
				return asGQL(err)
			}
			if gerr := x.setLabels(r, &i.Labels, in.strs("labelIds"), true); gerr != nil {
				return gerr
			}
			if gerr := x.setAssignees(&i.Assignees, in.strs("assigneeIds"), true); gerr != nil {
				return gerr
			}
			return result("CreateIssuePayload", map[string]any{"issue": i})
		},
		"updateIssue": func(x *gqlExec, in args) any {
			i, gerr := nodeOf[*Issue](x, in.str("id"), "an Issue")
			if gerr != nil {
				return gerr
			}
			if in.has("title") {
				i.Title = in.str("title")
			}
			if in.has("body") {
				i.Body = in.str("body")
			}
			if in.has("labelIds") {
				i.Labels = nil
				if gerr := x.setLabels(i.Repo, &i.Labels, in.strs("labelIds"), true); gerr != nil {
					return gerr
				}
			}
			if in.has("assigneeIds") {
				i.Assignees = nil
				if gerr := x.setAssignees(&i.Assignees, in.strs("assigneeIds"), true); gerr != nil {
					return gerr
				}
			}
			switch in.str("state") {
			case stateClosed:
				x.m.CloseIssue(i, "")
			case stateOpen:
				x.m.ReopenIssue(i)
			}
			i.UpdatedAt = x.m.now()
			return result("UpdateIssuePayload", map[string]any{"issue": i})
		},
		"closeIssue": func(x *gqlExec, in args) any {
			i, gerr := nodeOf[*Issue](x, in.str("issueId"), "an Issue")
			if gerr != nil {
				return gerr
			}
			x.m.CloseIssue(i, in.str("stateReason"))
			return result("CloseIssuePayload", map[string]any{"issue": i})
		},
		"reopenIssue": func(x *gqlExec, in args) any {
			i, gerr := nodeOf[*Issue](x, in.str("issueId"), "an Issue")
			if gerr != nil {
				return gerr
			}
			x.m.ReopenIssue(i)
			return result("ReopenIssuePayload", map[string]any{"issue": i})
		},
		"addLabelsToLabelable": func(x *gqlExec, in args) any {
			i, pr, gerr := subjectNode(x, in.str("labelableId"))
			if gerr != nil {
				return gerr
			}
			repo, labels := labelsOf(i, pr)
			if gerr := x.setLabels(repo, labels, in.strs("labelIds"), true); gerr != nil {
				return gerr
			}
			return result("AddLabelsToLabelablePayload", map[string]any{"labelable": subjectValue(i, pr)})
		},
		"removeLabelsFromLabelable": func(x *gqlExec, in args) any {
			i, pr, gerr := subjectNode(x, in.str("labelableId"))
			if gerr != nil {
				return gerr
			}
			repo, labels := labelsOf(i, pr)
			if gerr := x.setLabels(repo, labels, in.strs("labelIds"), false); gerr != nil {
				return gerr
			}
			return result("RemoveLabelsFromLabelablePayload", map[string]any{"labelable": subjectValue(i, pr)})
		},
		"addAssigneesToAssignable": func(x *gqlExec, in args) any {
			i, pr, gerr := subjectNode(x, in.str("assignableId"))
			if gerr != nil {
				return gerr
			}
			if gerr := x.setAssignees(assigneesOf(i, pr), in.strs("assigneeIds"), true); gerr != nil {
				return gerr
			}
			return result("AddAssigneesToAssignablePayload", map[string]any{"assignable": subjectValue(i, pr)})
		},
		"removeAssigneesFromAssignable": func(x *gqlExec, in args) any {
			i, pr, gerr := subjectNode(x, in.str("assignableId"))
			if gerr != nil {
				return gerr
			}
			if gerr := x.setAssignees(assigneesOf(i, pr), in.strs("assigneeIds"), false); gerr != nil {
				return gerr
			}
			return result("RemoveAssigneesFromAssignablePayload", map[string]any{"assignable": subjectValue(i, pr)})
		},
		"createPullRequest": func(x *gqlExec, in args) any {
			r, gerr := nodeOf[*Repo](x, in.str("repositoryId"), "a Repository")
			if gerr != nil {
				return gerr
			}
			head := in.str("headRefName")
			if _, branch, ok := strings.Cut(head, ":"); ok {
				head = branch
			}
			if _, ok := x.m.Branch(r, head); !ok {
				return unprocessable("Head sha can't be blank, Base sha can't be blank, No commits between %s and %s, Head ref must be a branch", in.str("baseRefName"), head)
			}
			if _, ok := x.m.Branch(r, in.str("baseRefName")); !ok {
				return unprocessable("Base ref must be a branch")
			}
			p, err := x.m.AddPull(r, PullSpec{Title: in.str("title"), Body: in.str("body"), Base: in.str("baseRefName"), Head: head, Draft: in.flag("draft")})
			if err != nil {
				return asGQL(err)
			}
			return result("CreatePullRequestPayload", map[string]any{"pullRequest": p})
		},
		"updatePullRequest": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			if in.has("title") {
				p.Title = in.str("title")
			}
			if in.has("body") {
				p.Body = in.str("body")
			}
			if in.has("baseRefName") {
				p.Base = in.str("baseRefName")
			}
			if in.has("labelIds") {
				p.Labels = nil
				if gerr := x.setLabels(p.Repo, &p.Labels, in.strs("labelIds"), true); gerr != nil {
					return gerr
				}
			}
			switch in.str("state") {
			case stateClosed:
				x.m.ClosePull(p)
			case stateOpen:
				if err := x.m.ReopenPull(p); err != nil {
					return asGQL(err)
				}
			}
			p.UpdatedAt = x.m.now()
			return result("UpdatePullRequestPayload", map[string]any{"pullRequest": p})
		},
		"closePullRequest": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			x.m.ClosePull(p)
			return result("ClosePullRequestPayload", map[string]any{"pullRequest": p})
		},
		"reopenPullRequest": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			if err := x.m.ReopenPull(p); err != nil {
				return asGQL(err)
			}
			return result("ReopenPullRequestPayload", map[string]any{"pullRequest": p})
		},
		"mergePullRequest": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			err := x.m.Merge(p, MergeSpec{Method: in.str("mergeMethod"), Title: in.str("commitHeadline"), Body: in.str("commitBody"),
				SHA: in.str("expectedHeadOid"), Admin: true})
			if err != nil {
				return asGQL(err)
			}
			return result("MergePullRequestPayload", map[string]any{"pullRequest": p})
		},
		"markPullRequestReadyForReview": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			p.Draft, p.UpdatedAt = false, x.m.now()
			return result("MarkPullRequestReadyForReviewPayload", map[string]any{"pullRequest": p})
		},
		"convertPullRequestToDraft": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			p.Draft, p.UpdatedAt = true, x.m.now()
			return result("ConvertPullRequestToDraftPayload", map[string]any{"pullRequest": p})
		},
		"addPullRequestReview": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			state, ok := reviewEvents[in.str("event")]
			if !ok {
				return unprocessable("Review event %q is not one of APPROVE, REQUEST_CHANGES, COMMENT", in.str("event"))
			}
			if state != reviewCommented && strings.EqualFold(p.Author, x.m.Viewer) {
				word := "approve"
				if state == reviewChanges {
					word = "request changes on"
				}
				return unprocessable("Review Can not %s your own pull request", word)
			}
			rv := x.m.AddReview(p, "", state, in.str("body"), in.str("commitOID"))
			return result("AddPullRequestReviewPayload", map[string]any{"pullRequestReview": rv})
		},
		"enablePullRequestAutoMerge": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			p.AutoMerge = cmpOr(in.str("mergeMethod"), "MERGE")
			return result("EnablePullRequestAutoMergePayload", map[string]any{"pullRequest": p})
		},
		"disablePullRequestAutoMerge": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			p.AutoMerge = ""
			return result("DisablePullRequestAutoMergePayload", map[string]any{"pullRequest": p})
		},
		"updatePullRequestBranch": func(x *gqlExec, in args) any {
			p, gerr := nodeOf[*Pull](x, in.str("pullRequestId"), "a PullRequest")
			if gerr != nil {
				return gerr
			}
			if err := x.m.UpdateBranch(p, in.str("expectedHeadOid"), in.str("updateMethod") == "REBASE"); err != nil {
				return asGQL(err)
			}
			return result("UpdatePullRequestBranchPayload", map[string]any{"pullRequest": p})
		},
	}

	fields := fieldsOf[mutationRoot]{}
	for name, run := range mutations {
		fields[name] = func(x *gqlExec, _ mutationRoot, a args) any {
			if !a.has("input") {
				return unsupported("the mutation %s without an input", name)
			}
			return run(x, a.obj("input"))
		}
	}
	define("Mutation", fields)
}

// reviewEvents are a review's events and the states they give.
var reviewEvents = map[string]string{"APPROVE": reviewApproved, "REQUEST_CHANGES": reviewChanges, "COMMENT": reviewCommented}

func issuePair(x *gqlExec, id, otherID string) (*Issue, *Issue, *gqlError) {
	i, gerr := nodeOf[*Issue](x, id, "an Issue")
	if gerr != nil {
		return nil, nil, gerr
	}
	other, gerr := nodeOf[*Issue](x, otherID, "an Issue")
	if gerr != nil {
		return nil, nil, gerr
	}
	if i == other {
		return nil, nil, unprocessable("An issue cannot be linked to itself")
	}
	return i, other, nil
}

func labelsOf(i *Issue, p *Pull) (*Repo, *[]string) {
	if i != nil {
		return i.Repo, &i.Labels
	}
	return p.Repo, &p.Labels
}

func assigneesOf(i *Issue, p *Pull) *[]string {
	if i != nil {
		return &i.Assignees
	}
	return &p.Assignees
}

// setLabels adds the labels of ids to names, or removes them.
func (x *gqlExec) setLabels(r *Repo, names *[]string, ids []string, add bool) *gqlError {
	for _, id := range ids {
		l, gerr := nodeOf[*Label](x, id, "a Label")
		if gerr != nil {
			return gerr
		}
		if _, ok := r.Label(l.Name); !ok {
			return unprocessable("Label %s does not belong to %s", l.Name, r.Full())
		}
		has := containsFold(*names, l.Name)
		switch {
		case add && !has:
			*names = append(*names, l.Name)
		case !add && has:
			*names = slices.DeleteFunc(*names, func(n string) bool { return strings.EqualFold(n, l.Name) })
		}
	}
	return nil
}

// setAssignees adds the accounts of ids to logins, or removes them.
func (x *gqlExec) setAssignees(logins *[]string, ids []string, add bool) *gqlError {
	for _, id := range ids {
		a, gerr := nodeOf[*Account](x, id, "a User")
		if gerr != nil {
			return gerr
		}
		has := containsFold(*logins, a.Login)
		switch {
		case add && !has:
			*logins = append(*logins, a.Login)
		case !add && has:
			*logins = slices.DeleteFunc(*logins, func(n string) bool { return strings.EqualFold(n, a.Login) })
		}
	}
	return nil
}
