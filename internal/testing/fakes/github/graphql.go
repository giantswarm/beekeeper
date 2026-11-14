package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// Keys of a GraphQL answer, the type of a query operation, and the typename of an issue.
const (
	answerData    = "data"
	answerErrors  = "errors"
	opQuery       = "query"
	typenameIssue = "Issue"
)

// gqlRequest is a parsed GraphQL request.
type gqlRequest struct {
	opName string
	opType string
	vars   map[string]any
	doc    *gqlDoc
	op     *gqlOp
	err    error
}

func parseGQLRequest(body []byte) *gqlRequest {
	var req wire.GraphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return &gqlRequest{err: fmt.Errorf("problems parsing JSON: %v", err)}
	}
	doc, err := parseGraphQL(req.Query)
	if err != nil {
		return &gqlRequest{opName: req.OperationName, err: fmt.Errorf("parse error: %v", err)}
	}
	op, err := doc.operation(req.OperationName)
	if err != nil {
		return &gqlRequest{opName: req.OperationName, err: err}
	}
	vars := req.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	return &gqlRequest{opName: op.name, opType: op.typ, vars: vars, doc: doc, op: op}
}

func gqlErrors(errs ...wire.GraphQLError) reply {
	return jsonReply(http.StatusOK, map[string]any{answerErrors: errs})
}

// graphql answers a GraphQL request by operation name. s.mu is held.
func (s *Server) graphql(c *Call, gq *gqlRequest, rl *rateLimit, now time.Time) reply {
	if gq.err != nil {
		return gqlErrors(wire.GraphQLError{Message: gq.err.Error()})
	}
	if gq.opType != opQuery {
		return gqlErrors(wire.GraphQLError{Type: wire.ErrorTypeForbidden, Message: "the fake GitHub answers no mutations"})
	}
	cost := gq.doc.cost(gq.op, gq.vars)
	if cost > rl.remaining {
		c.RateLimited = true
		return jsonReply(http.StatusOK, map[string]any{
			answerData:   nil,
			answerErrors: []wire.GraphQLError{{Type: wire.ErrorTypeRateLimited, Message: "API rate limit exceeded"}},
		})
	}
	rl.remaining -= cost
	c.Points = cost
	rate := &wire.RateLimit{Cost: cost, Limit: rl.limit, Remaining: rl.remaining, Used: rl.limit - rl.remaining, ResetAt: rl.resetAt}

	var data any
	var errs []wire.GraphQLError
	switch gq.opName {
	case forge.OpBoardSchema:
		data, errs = s.gqlBoardSchema(gq.vars, rate)
	case forge.OpBoardTeamItems:
		data, errs = s.gqlTeamItems(gq.vars, rate, now)
	case forge.OpBoardItemsByNode:
		data, errs = s.gqlItemsByNode(gq.vars, rate)
	case forge.OpBoardProbe:
		data, errs = s.gqlProbe(gq.vars, rate, now)
	case forge.OpPullClosingIssues:
		data, errs = s.gqlPullClosingIssues(gq.vars, rate)
	default:
		return gqlErrors(wire.GraphQLError{Message: fmt.Sprintf("the fake GitHub doesn't know the operation %q", gq.opName)})
	}
	resp := map[string]any{answerData: data}
	if len(errs) > 0 {
		resp[answerErrors] = errs
	}
	return jsonReply(http.StatusOK, resp)
}

func varString(vars map[string]any, name string) string { s, _ := vars[name].(string); return s }
func varInt(vars map[string]any, name string) int       { return int(number(vars[name])) }
func varBool(vars map[string]any, name string) bool     { b, _ := vars[name].(bool); return b }

// project checks the org and number of a board query.
func (s *Server) project(vars map[string]any) []wire.GraphQLError {
	org, number := varString(vars, "org"), varInt(vars, "number")
	if org != s.st.org {
		return []wire.GraphQLError{{Type: wire.ErrorTypeNotFound, Message: fmt.Sprintf("Could not resolve to an Organization with the login of '%s'.", org)}}
	}
	if number != s.st.project {
		return []wire.GraphQLError{{Type: wire.ErrorTypeNotFound, Message: fmt.Sprintf("Could not resolve to a ProjectV2 with the number %d.", number)}}
	}
	return nil
}

func (s *Server) gqlBoardSchema(vars map[string]any, rate *wire.RateLimit) (any, []wire.GraphQLError) {
	d := wire.BoardSchemaData{RateLimit: rate}
	if errs := s.project(vars); errs != nil {
		if varString(vars, "org") == s.st.org {
			d.Organization = &wire.SchemaOrg{}
		}
		return d, errs
	}
	p := &wire.SchemaProject{ID: s.st.projectNodeID()}
	for _, f := range []string{"Title", "Assignees", fieldStatus, "Labels", "Linked pull requests", "Repository", fieldTeam, fieldKind, "Parent issue", fieldPriority} {
		opts := s.st.options(f)
		if opts == nil {
			p.Fields.Nodes = append(p.Fields.Nodes, wire.SchemaField{}) // not a single-select field
			continue
		}
		sf := wire.SchemaField{ID: s.st.fieldNodeID(f), Name: f}
		for _, o := range opts {
			sf.Options = append(sf.Options, wire.SelectOption{ID: optionID(f, o), Name: o})
		}
		p.Fields.Nodes = append(p.Fields.Nodes, sf)
	}
	d.Organization = &wire.SchemaOrg{ProjectV2: p}
	return d, nil
}

// connection filters the board's items and pages them. Like GitHub, it answers in board position
// order (ProjectV2ItemOrder knows only POSITION): an item takes its place when it is added to the
// board, at the bottom, and keeps it through edits, archiving and restoring. Nothing orders the items
// by their update, so a reader that wants the recently updated ones must filter (updated:) and page.
func (s *Server) connection(query string, first int, after string, now time.Time, full bool) (*wire.ItemConnection, error) {
	terms, err := parseFilter(query)
	if err != nil {
		return nil, err
	}
	if first < 1 || first > 100 {
		return nil, fmt.Errorf("first must be between 1 and 100, got %d", first)
	}
	var matched []*boardItem
	for _, it := range s.st.items {
		if !it.archived && s.st.matches(it, terms, now) {
			matched = append(matched, it)
		}
	}
	start := 0
	if after != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(after, "cursor:"))
		if err != nil {
			return nil, fmt.Errorf("invalid cursor %q", after)
		}
		start = n + 1
	}
	conn := &wire.ItemConnection{TotalCount: len(matched), Nodes: []wire.ProjectItem{}}
	end := min(start+first, len(matched))
	for i := start; i < end; i++ {
		if full {
			conn.Nodes = append(conn.Nodes, s.projectItem(matched[i]))
		} else {
			conn.Nodes = append(conn.Nodes, s.probeItem(matched[i]))
		}
	}
	if end > start {
		conn.PageInfo.EndCursor = "cursor:" + strconv.Itoa(end-1)
	}
	conn.PageInfo.HasNextPage = end < len(matched)
	return conn, nil
}

func (s *Server) gqlTeamItems(vars map[string]any, rate *wire.RateLimit, now time.Time) (any, []wire.GraphQLError) {
	d := wire.BoardTeamItemsData{RateLimit: rate}
	if errs := s.project(vars); errs != nil {
		return d, errs
	}
	p := &wire.TeamItemsProject{ID: s.st.projectNodeID()}
	var err error
	if varBool(vars, "withActive") {
		if p.Active, err = s.connection(varString(vars, "activeQuery"), varInt(vars, "activeFirst"), varString(vars, "activeAfter"), now, true); err != nil {
			return nil, []wire.GraphQLError{{Message: err.Error()}}
		}
	}
	if varBool(vars, "withInbox") {
		if p.Inbox, err = s.connection(varString(vars, "inboxQuery"), varInt(vars, "inboxFirst"), varString(vars, "inboxAfter"), now, true); err != nil {
			return nil, []wire.GraphQLError{{Message: err.Error()}}
		}
	}
	d.Organization = &wire.TeamItemsOrg{ProjectV2: p}
	return d, nil
}

func (s *Server) gqlProbe(vars map[string]any, rate *wire.RateLimit, now time.Time) (any, []wire.GraphQLError) {
	d := wire.BoardProbeData{RateLimit: rate}
	if errs := s.project(vars); errs != nil {
		return d, errs
	}
	conn, err := s.connection(varString(vars, "query"), varInt(vars, "first"), varString(vars, "after"), now, false)
	if err != nil {
		return nil, []wire.GraphQLError{{Message: err.Error()}}
	}
	d.Organization = &wire.ProbeOrg{ProjectV2: &wire.ProbeProject{Items: *conn}}
	return d, nil
}

func (s *Server) gqlItemsByNode(vars map[string]any, rate *wire.RateLimit) (any, []wire.GraphQLError) {
	ids, _ := vars["ids"].([]any)
	if len(ids) > 100 {
		return nil, []wire.GraphQLError{{Message: "You may not request more than 100 node ids"}}
	}
	nodes := make([]any, len(ids))
	var errs []wire.GraphQLError
	for i, v := range ids {
		id, _ := v.(string)
		if it := s.st.itemByNode(id); it != nil {
			pi := s.projectItem(it)
			nodes[i] = pi
			continue
		}
		if strings.HasPrefix(id, "I_") {
			if _, ok := s.st.issues[strings.TrimPrefix(id, "I_")]; ok {
				nodes[i] = map[string]any{"__typename": typenameIssue}
				continue
			}
		}
		if strings.HasPrefix(id, "PR_") {
			if _, ok := s.st.pulls[strings.TrimPrefix(id, "PR_")]; ok {
				nodes[i] = map[string]any{"__typename": "PullRequest"}
				continue
			}
		}
		errs = append(errs, wire.GraphQLError{Type: wire.ErrorTypeNotFound, Message: fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id), Path: []any{"nodes", i}})
	}
	return map[string]any{"rateLimit": rate, "nodes": nodes}, errs
}

// gqlPullClosingIssues answers a PR's closingIssuesReferences from its closes, in their order. Like
// the board's links, closing issues in repos the installation can't read are listed too.
func (s *Server) gqlPullClosingIssues(vars map[string]any, rate *wire.RateLimit) (any, []wire.GraphQLError) {
	d := wire.PullClosingIssuesData{RateLimit: rate}
	repoName := varString(vars, "owner") + "/" + varString(vars, "name")
	if !s.st.visible(repoName) {
		return d, []wire.GraphQLError{{Type: wire.ErrorTypeNotFound, Path: []any{"repository"},
			Message: fmt.Sprintf("Could not resolve to a Repository with the name '%s'.", repoName)}}
	}
	d.Repository = &wire.ClosingRepo{}
	number := varInt(vars, "number")
	p, ok := s.st.pulls[repoName+"#"+strconv.Itoa(number)]
	if !ok {
		return d, []wire.GraphQLError{{Type: wire.ErrorTypeNotFound, Path: []any{"repository", "pullRequest"},
			Message: fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", number)}}
	}
	first := varInt(vars, "first")
	if first < 1 || first > 100 {
		return nil, []wire.GraphQLError{{Message: fmt.Sprintf("first must be between 1 and 100, got %d", first)}}
	}
	all := s.issueRefs(p.closes, -1).Nodes
	start := 0
	if after := varString(vars, "after"); after != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(after, "cursor:"))
		if err != nil {
			return nil, []wire.GraphQLError{{Message: fmt.Sprintf("invalid cursor %q", after)}}
		}
		start = n + 1
	}
	end := min(start+first, len(all))
	conn := wire.ClosingIssueConn{TotalCount: len(all), Nodes: []wire.IssueRef{}}
	if start < end {
		conn.Nodes = append(conn.Nodes, all[start:end]...)
		conn.PageInfo.EndCursor = "cursor:" + strconv.Itoa(end-1)
	}
	conn.PageInfo.HasNextPage = end < len(all)
	d.Repository.PullRequest = &wire.ClosingPull{ClosingIssues: conn}
	return d, nil
}

func gqlState(state string) string {
	if state == stateClosed {
		return wire.StateClosed
	}
	return wire.StateOpen
}

func gqlStateReason(reason string) string {
	switch reason {
	case reasonCompleted:
		return wire.StateReasonCompleted
	case "not_planned":
		return wire.StateReasonNotPlanned
	case "reopened":
		return wire.StateReasonReopened
	}
	return ""
}

func (s *Server) selectValue(field, name string, at *time.Time) *wire.SingleSelectValue {
	if name == "" {
		return nil
	}
	return &wire.SingleSelectValue{Name: name, OptionID: optionID(field, name), UpdatedAt: at}
}

// issueRefs lists linked issues. Links into repos the installation can't read are listed too (the
// engine learns their refs and has its reads of them refused).
func (s *Server) issueRefs(refs []string, limit int) *wire.IssueConn {
	conn := &wire.IssueConn{Nodes: []wire.IssueRef{}}
	for _, ref := range refs {
		is, ok := s.st.issues[ref]
		if !ok {
			continue
		}
		if len(conn.Nodes) == limit {
			break
		}
		conn.Nodes = append(conn.Nodes, wire.IssueRef{
			Number: is.number, State: gqlState(is.state), StateReason: gqlStateReason(is.stateReason),
			Repository: wire.RepoRef{NameWithOwner: is.repo},
		})
	}
	return conn
}

// subIssuesSummary counts an issue's sub-issues, also those a list leaves out.
func (s *Server) subIssuesSummary(is *issue) *wire.SubIssuesSummary {
	sum := &wire.SubIssuesSummary{}
	for _, ref := range is.subIssues {
		if sub, ok := s.st.issues[ref]; ok {
			sum.Total++
			if sub.state == stateClosed {
				sum.Completed++
			}
		}
	}
	return sum
}

// dependenciesSummary counts an issue's blockers (open ones in BlockedBy), also those a list leaves out.
func (s *Server) dependenciesSummary(is *issue) *wire.DependenciesSummary {
	sum := &wire.DependenciesSummary{}
	for _, ref := range is.blockedBy {
		if b, ok := s.st.issues[ref]; ok {
			sum.TotalBlockedBy++
			if b.state == stateOpen {
				sum.BlockedBy++
			}
		}
	}
	return sum
}

// probeItem is an item as BoardProbe selects it.
func (s *Server) probeItem(it *boardItem) wire.ProjectItem {
	pi := wire.ProjectItem{Typename: "ProjectV2Item", ID: it.nodeID, Type: wire.ItemTypeIssue, IsArchived: it.archived, UpdatedAt: it.updatedAt}
	is := s.st.issues[it.issue]
	if is == nil || !s.st.visible(is.repo) {
		pi.Type = wire.ItemTypeRedacted
		return pi
	}
	pi.Content = &wire.ItemContent{Typename: typenameIssue, Number: is.number, Repository: &wire.RepoRef{NameWithOwner: is.repo}}
	return pi
}

// projectItem is an item as BoardItemFields selects it.
func (s *Server) projectItem(it *boardItem) wire.ProjectItem {
	st := s.st
	statusAt := it.statusAt
	pi := wire.ProjectItem{
		Typename:   "ProjectV2Item",
		ID:         it.nodeID,
		Type:       wire.ItemTypeIssue,
		IsArchived: it.archived,
		UpdatedAt:  it.updatedAt,
		Project:    &wire.ProjectRef{Number: st.project},
		Status:     s.selectValue(fieldStatus, it.status, &statusAt),
		Team:       s.selectValue(fieldTeam, it.team, nil),
		Kind:       s.selectValue(fieldKind, it.kind, nil),
	}
	if it.priority != nil {
		pi.Priority = &wire.NumberValue{Number: *it.priority}
	}
	is := st.issues[it.issue]
	if is == nil || !st.visible(is.repo) {
		// Items whose repo the installation can't read come back redacted.
		pi.Type = wire.ItemTypeRedacted
		return pi
	}
	created, updated := is.createdAt, is.updatedAt
	ct := &wire.ItemContent{
		Typename:            typenameIssue,
		ID:                  is.nodeID,
		Number:              is.number,
		Title:               is.title,
		Body:                is.body,
		State:               gqlState(is.state),
		StateReason:         gqlStateReason(is.stateReason),
		CreatedAt:           &created,
		UpdatedAt:           &updated,
		Repository:          &wire.RepoRef{NameWithOwner: is.repo, IsPrivate: st.repos[is.repo].private},
		Assignees:           &wire.UserConn{Nodes: []wire.UserNode{}},
		Labels:              &wire.LabelConn{Nodes: []wire.LabelNode{}},
		SubIssues:           s.issueRefs(is.subIssues, 20),
		BlockedBy:           s.issueRefs(is.blockedBy, 10),
		ClosedBy:            &wire.PullConn{Nodes: []wire.PullRef{}},
		SubIssuesSummary:    s.subIssuesSummary(is),
		DependenciesSummary: s.dependenciesSummary(is),
	}
	for i, a := range is.assignees {
		if i == 10 {
			break
		}
		ct.Assignees.Nodes = append(ct.Assignees.Nodes, wire.UserNode{Login: a})
	}
	for i, l := range is.labels {
		if i == 20 {
			break
		}
		ct.Labels.Nodes = append(ct.Labels.Nodes, wire.LabelNode{Name: l})
	}
	if is.parent != "" {
		if par, ok := st.issues[is.parent]; ok {
			ct.Parent = &wire.IssueRef{Number: par.number, Repository: wire.RepoRef{NameWithOwner: par.repo}}
		}
	}
	for _, p := range st.closingPulls(is.ref) {
		if len(ct.ClosedBy.Nodes) == 10 {
			break
		}
		state := gqlState(p.state)
		if p.merged {
			state = wire.StateMerged
		}
		ct.ClosedBy.Nodes = append(ct.ClosedBy.Nodes, wire.PullRef{
			Number: p.number, State: state, IsDraft: p.draft, Merged: p.merged, Repository: wire.RepoRef{NameWithOwner: p.repo},
		})
	}
	pi.Content = ct
	return pi
}
