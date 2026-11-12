//go:build scenario

package fakegithub

import (
	"fmt"
	"slices"
	"strings"
)

func (x *gqlExec) fieldValue(it *ProjectItem, name string) any {
	if strings.EqualFold(name, "Title") {
		return &payload{typ: "ProjectV2ItemFieldTextValue", fields: map[string]any{"text": itemTitle(it), "updatedAt": it.UpdatedAt}}
	}
	v, ok := it.Value(name)
	if !ok {
		return nil
	}
	return &fieldValueObj{item: it, value: v}
}

func itemTitle(it *ProjectItem) string {
	switch {
	case it.Issue != nil:
		return it.Issue.Title
	case it.Pull != nil:
		return it.Pull.Title
	}
	return it.Draft
}

func defineProjects() {
	define("ProjectV2", fieldsOf[*Project]{
		"id":           func(_ *gqlExec, p *Project, _ args) any { return p.NodeID },
		"databaseId":   func(_ *gqlExec, p *Project, _ args) any { return p.ID },
		"number":       func(_ *gqlExec, p *Project, _ args) any { return p.Number },
		"title":        func(_ *gqlExec, p *Project, _ args) any { return p.Title },
		"closed":       func(_ *gqlExec, p *Project, _ args) any { return p.Closed },
		"public":       func(_ *gqlExec, _ *Project, _ args) any { return false },
		"owner":        func(x *gqlExec, p *Project, _ args) any { return x.m.Account(p.Owner, "") },
		"url":          func(x *gqlExec, p *Project, _ args) any { return projectURL(x.m, p) },
		"resourcePath": func(x *gqlExec, p *Project, _ args) any { return strings.TrimPrefix(projectURL(x.m, p), webHost) },
		"fields": func(_ *gqlExec, p *Project, a args) any {
			return connect("fields", p.Fields, a)
		},
		"field": func(_ *gqlExec, p *Project, a args) any {
			f := p.Field(a.str("name"))
			if f == nil {
				return nil
			}
			return f
		},
		"items": func(x *gqlExec, p *Project, a args) any {
			query := a.str("query")
			var out []*ProjectItem
			for _, it := range p.Items {
				ok, err := itemMatches(it, query)
				if err != nil {
					return err
				}
				if ok {
					out = append(out, it)
				}
			}
			return connect("items", out, a)
		},
	})

	define("ProjectV2Item", fieldsOf[*ProjectItem]{
		"id":         func(_ *gqlExec, it *ProjectItem, _ args) any { return it.NodeID },
		"databaseId": func(_ *gqlExec, it *ProjectItem, _ args) any { return it.ID },
		"isArchived": func(_ *gqlExec, it *ProjectItem, _ args) any { return it.Archived },
		"type": func(_ *gqlExec, it *ProjectItem, _ args) any {
			switch {
			case it.Issue != nil:
				return "ISSUE"
			case it.Pull != nil:
				return "PULL_REQUEST"
			}
			return "DRAFT_ISSUE"
		},
		"project":   func(_ *gqlExec, it *ProjectItem, _ args) any { return it.Project },
		"createdAt": func(_ *gqlExec, it *ProjectItem, _ args) any { return it.CreatedAt },
		"updatedAt": func(_ *gqlExec, it *ProjectItem, _ args) any { return it.UpdatedAt },
		"content": func(_ *gqlExec, it *ProjectItem, _ args) any {
			switch {
			case it.Issue != nil:
				return it.Issue
			case it.Pull != nil:
				return it.Pull
			}
			return &draftObj{item: it}
		},
		"fieldValueByName": func(x *gqlExec, it *ProjectItem, a args) any { return x.fieldValue(it, a.str("name")) },
		"fieldValues": func(x *gqlExec, it *ProjectItem, a args) any {
			var out []any
			for _, f := range it.Project.Fields {
				if v := x.fieldValue(it, f.Name); v != nil {
					out = append(out, v)
				}
			}
			return connect("fieldValues", out, a)
		},
	})

	define("DraftIssue", fieldsOf[*draftObj]{
		"id":    func(_ *gqlExec, d *draftObj, _ args) any { return "DI_" + d.item.NodeID },
		"title": func(_ *gqlExec, d *draftObj, _ args) any { return d.item.Draft },
		"body":  func(_ *gqlExec, _ *draftObj, _ args) any { return "" },
	})

	value := fieldsOf[*fieldValueObj]{
		"id": func(_ *gqlExec, v *fieldValueObj, _ args) any {
			return "PVTFV_" + v.item.NodeID + "_" + v.value.Field.NodeID
		},
		"name":      func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.value.Option },
		"optionId":  func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.value.OptionID },
		"text":      func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.value.Text },
		"updatedAt": func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.value.UpdatedAt },
		"createdAt": func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.value.UpdatedAt },
		"field":     func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.value.Field },
		"item":      func(_ *gqlExec, v *fieldValueObj, _ args) any { return v.item },
	}
	define("ProjectV2ItemFieldSingleSelectValue", value)
	define("ProjectV2ItemFieldTextValue", value)

	field := fieldsOf[*ProjectField]{
		"id":       func(_ *gqlExec, f *ProjectField, _ args) any { return f.NodeID },
		"name":     func(_ *gqlExec, f *ProjectField, _ args) any { return f.Name },
		"dataType": func(_ *gqlExec, f *ProjectField, _ args) any { return f.DataType },
		"options": func(_ *gqlExec, f *ProjectField, _ args) any {
			out := make([]any, 0, len(f.Options))
			for _, o := range f.Options {
				out = append(out, &payload{typ: "ProjectV2SingleSelectFieldOption", fields: map[string]any{
					"id": o.ID, "name": o.Name, "nameHTML": o.Name, "color": "GRAY", "description": "",
				}})
			}
			return out
		},
	}
	define("ProjectV2SingleSelectField", field)
	define("ProjectV2Field", field)
}

func projectURL(m *Model, p *Project) string {
	kind := "orgs"
	if a, ok := m.LookupAccount(p.Owner); ok && a.Kind == kindUser {
		kind = "users"
	}
	return fmt.Sprintf("%s/%s/%s/projects/%d", webHost, kind, p.Owner, p.Number)
}

// filterTerm is one term of a filter or search query: a qualifier with
// its values (label:"a b",c), or free text.
type filterTerm struct {
	Negated bool
	Key     string // "" for free text
	Values  []string
}

// splitFilter splits a filter into its terms: spaces separate terms,
// double quotes keep a value together, commas separate a qualifier's
// values and a leading dash negates.
func splitFilter(q string) []filterTerm {
	var terms []filterTerm
	var words []string
	var cur strings.Builder
	quoted, started := false, false
	flush := func() {
		if started {
			words = append(words, cur.String())
		}
		cur.Reset()
		started = false
	}
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			started = true
			cur.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !quoted:
			flush()
		default:
			started = true
			cur.WriteRune(r)
		}
	}
	flush()
	for _, w := range words {
		t := filterTerm{}
		if strings.HasPrefix(w, "-") && len(w) > 1 {
			t.Negated, w = true, w[1:]
		}
		key, rest, isQualifier := strings.Cut(w, ":")
		if !isQualifier || key == "" || strings.ContainsAny(key, `"/`) || strings.HasPrefix(rest, "//") {
			t.Values = []string{strings.Trim(w, `"`)}
			terms = append(terms, t)
			continue
		}
		t.Key = strings.ToLower(key)
		t.Values = splitValues(rest)
		terms = append(terms, t)
	}
	return terms
}

// splitValues splits a qualifier's values at the commas outside quotes.
func splitValues(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}

// itemMatches applies a board's filter (items(query:)) to an item. An
// empty filter holds every item, archived ones too; any other holds an
// archived item only with is:archived.
func itemMatches(it *ProjectItem, query string) (bool, error) {
	terms := splitFilter(query)
	if len(terms) == 0 {
		return true, nil
	}
	wantsArchived := false
	for _, t := range terms {
		ok, err := itemTerm(it, t, &wantsArchived)
		if err != nil {
			return false, err
		}
		if ok == t.Negated {
			return false, nil
		}
	}
	return it.Archived == wantsArchived, nil
}

func itemTerm(it *ProjectItem, t filterTerm, wantsArchived *bool) (bool, error) {
	state, labels, assignees, repo := "", []string(nil), []string(nil), ""
	switch {
	case it.Issue != nil:
		state, labels, assignees, repo = it.Issue.State, it.Issue.Labels, it.Issue.Assignees, it.Issue.Repo.Full()
	case it.Pull != nil:
		state, labels, assignees, repo = it.Pull.State, it.Pull.Labels, it.Pull.Assignees, it.Pull.Repo.Full()
	}
	anyOf := func(match func(v string) bool) bool { return slices.ContainsFunc(t.Values, match) }
	switch t.Key {
	case "":
		return strings.Contains(strings.ToLower(itemTitle(it)), strings.ToLower(t.Values[0])), nil
	case "is":
		for _, v := range t.Values {
			switch strings.ToLower(v) {
			case "open":
				// A draft has no state: it is open.
				if state == stateOpen || state == "" {
					return true, nil
				}
			case "closed":
				if state == stateClosed || state == stateMerged {
					return true, nil
				}
			case "merged":
				if state == stateMerged {
					return true, nil
				}
			case "issue":
				if it.Issue != nil {
					return true, nil
				}
			case "pr":
				if it.Pull != nil {
					return true, nil
				}
			case "draft":
				if it.Issue == nil && it.Pull == nil {
					return true, nil
				}
			case "archived":
				*wantsArchived = true
				return true, nil
			default:
				return false, unsupported("the board filter is:%s", v)
			}
		}
		return false, nil
	case "repo":
		return anyOf(func(v string) bool { return strings.EqualFold(v, repo) }), nil
	case "label":
		return anyOf(func(v string) bool { return containsFold(labels, v) }), nil
	case "assignee":
		return anyOf(func(v string) bool { return containsFold(assignees, v) }), nil
	case "no", "has":
		has := false
		for _, v := range t.Values {
			switch strings.ToLower(v) {
			case "label":
				has = has || len(labels) > 0
			case "assignee":
				has = has || len(assignees) > 0
			default:
				f := it.Project.Field(v)
				if f == nil {
					return false, unsupported("the board filter %s:%s (the board has no such field)", t.Key, v)
				}
				_, set := it.Value(f.Name)
				has = has || set
			}
		}
		return has == (t.Key == "has"), nil
	}
	f := it.Project.Field(t.Key)
	if f == nil {
		return false, unsupported("the board filter %s: (the board has no such field)", t.Key)
	}
	v, set := it.Value(f.Name)
	if !set {
		return false, nil
	}
	return anyOf(func(want string) bool { return strings.EqualFold(want, v.Option) || strings.EqualFold(want, v.Text) }), nil
}
