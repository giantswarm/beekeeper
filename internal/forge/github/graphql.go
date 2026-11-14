package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// query runs one GraphQL query (never a mutation: the documents are wire's constants) and decodes
// its data. It returns the request's point cost.
func (c *Client) query(ctx context.Context, op, doc string, vars map[string]any, data any) ([]wire.GraphQLError, int, error) {
	body, err := json.Marshal(wire.GraphQLRequest{Query: doc, OperationName: op, Variables: vars})
	if err != nil {
		return nil, 0, &forge.Error{Kind: forge.Refused, API: forge.APIGraphQL, Operation: op, Err: err}
	}
	a, err := c.do(ctx, call{
		api: forge.APIGraphQL, op: op, method: http.MethodPost, path: wire.GraphQLPath, body: body,
		auth: authInstallation, class: forge.ClassFrom(ctx),
	})
	if err != nil {
		return nil, 0, err
	}
	points := graphQLCost(a.body)
	if len(a.gql.Data) == 0 || string(a.gql.Data) == "null" {
		return a.gql.Errors, points, &forge.Error{Kind: forge.Refused, API: forge.APIGraphQL, Operation: op, Code: a.code,
			Err: gqlErr(a.gql.Errors)}
	}
	if err := json.Unmarshal(a.gql.Data, data); err != nil {
		return a.gql.Errors, points, &forge.Error{Kind: forge.Transient, API: forge.APIGraphQL, Operation: op, Code: a.code,
			Err: fmt.Errorf("decoding GraphQL data: %w", err)}
	}
	return a.gql.Errors, points, nil
}

func gqlErr(errs []wire.GraphQLError) error {
	if len(errs) == 0 {
		return errors.New("no data in the GraphQL answer")
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, strings.TrimSpace(e.Type+" "+e.Message))
	}
	return errors.New(strings.Join(msgs, "; "))
}

func refusedGQL(op string, errs []wire.GraphQLError) error {
	return &forge.Error{Kind: forge.Refused, API: forge.APIGraphQL, Operation: op, Code: http.StatusOK, Err: gqlErr(errs)}
}

// node is a node of an answer's list: the list's place (the index of its path prefix) and the
// node's index in it.
type node struct{ list, index int }

// nodeIn reports the index of the node of the list at prefix that path lies inside of. A path that
// ends at the node itself (the node is null) or above it is not inside one.
func nodeIn(path []any, prefix []string) (int, bool) {
	if len(path) <= len(prefix)+1 {
		return 0, false
	}
	for i, p := range prefix {
		if s, _ := path[i].(string); s != p {
			return 0, false
		}
	}
	f, ok := path[len(prefix)].(float64)
	if !ok || f < 0 {
		return 0, false
	}
	return int(f), true
}

// partial sorts the errors GitHub answered beside a query's data, which would otherwise be dropped.
// An error inside one of the answer's nodes (below the node itself, e.g. a link GitHub couldn't
// resolve) spoils only that node: it comes back in spoiled, keyed by the list (the index of its path
// prefix) and the node's index. Any other error spoils the whole answer: a transient error, so the
// read is retried on the engine's clock. Either way the errors are logged.
func (c *Client) partial(op string, errs []wire.GraphQLError, prefixes ...[]string) (spoiled map[node]bool, err error) {
	if len(errs) == 0 {
		return nil, nil
	}
	var whole []wire.GraphQLError
	for _, e := range errs {
		found := false
		for li, pre := range prefixes {
			if i, ok := nodeIn(e.Path, pre); ok {
				if spoiled == nil {
					spoiled = map[node]bool{}
				}
				spoiled[node{li, i}] = true
				found = true
				break
			}
		}
		if !found {
			whole = append(whole, e)
		}
	}
	c.cfg.Logger.Info("GitHub answered a GraphQL query with errors beside its data", "operation", op,
		"errors", gqlErr(errs).Error(), "spoiledNodes", len(spoiled), "spoiledAnswer", len(whole) > 0)
	if len(whole) > 0 {
		return spoiled, &forge.Error{Kind: forge.Transient, API: forge.APIGraphQL, Operation: op, Code: http.StatusOK,
			Err: fmt.Errorf("errors beside the data: %w", gqlErr(whole))}
	}
	return spoiled, nil
}

// BoardSchema reads the board's single-select fields and their options.
func (c *Client) BoardSchema(ctx context.Context, org string, project int) (forge.BoardSchema, error) {
	var d wire.BoardSchemaData
	errs, _, err := c.query(ctx, forge.OpBoardSchema, wire.BoardSchemaQuery, map[string]any{varOrg: org, varNumber: project}, &d)
	if err != nil {
		return forge.BoardSchema{}, err
	}
	if d.Organization == nil || d.Organization.ProjectV2 == nil {
		return forge.BoardSchema{}, refusedGQL(forge.OpBoardSchema, errs)
	}
	// An option list GitHub couldn't fully answer would make a valid filter look invalid.
	if _, err := c.partial(forge.OpBoardSchema, errs); err != nil {
		return forge.BoardSchema{}, err
	}
	s := forge.BoardSchema{ProjectID: d.Organization.ProjectV2.ID, Options: map[string][]string{}}
	for _, f := range d.Organization.ProjectV2.Fields.Nodes {
		if f.Name == "" {
			continue // not a single-select field
		}
		names := make([]string, 0, len(f.Options))
		for _, o := range f.Options {
			names = append(names, o.Name)
		}
		s.Options[f.Name] = names
	}
	return s, nil
}

// filterValue quotes an option name for a board filter (names must be exact, emoji included).
func filterValue(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func filterValues(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = filterValue(s)
	}
	return strings.Join(q, ",")
}

// TeamFilters returns the board filters BoardTeamItems sends: the team's open items in the active
// statuses, and the team's open Inbox bugs.
func TeamFilters(q forge.TeamItemsQuery) (active, inbox string) {
	active = fmt.Sprintf("team:%s status:%s is:open", filterValue(q.TeamOption), filterValues(q.Statuses))
	if q.InboxStatus != "" && q.BugKind != "" {
		inbox = fmt.Sprintf("team:%s status:%s kind:%s is:open", filterValue(q.TeamOption), filterValue(q.InboxStatus), filterValue(q.BugKind))
	}
	return active, inbox
}

// Variables several queries take.
const (
	varOrg    = "org"
	varNumber = "number"
)

// Fields of the paths to the node lists of the answers, which GraphQL errors name.
const (
	fieldOrganization = "organization"
	fieldProjectV2    = "projectV2"
	fieldNodes        = "nodes"
)

// Paths of the item lists in a BoardTeamItems answer.
var (
	activeNodes = []string{fieldOrganization, fieldProjectV2, "active", fieldNodes}
	inboxNodes  = []string{fieldOrganization, fieldProjectV2, "inbox", fieldNodes}
)

// BoardTeamItems runs the team's two filtered item queries as one request per page; a team's items
// fit in one page (30 active items and 15 Inbox bugs, 2 points), larger teams take more. An error
// GitHub answers about part of an item (a link it couldn't resolve) marks that item Incomplete; any
// other error beside the data fails the read.
func (c *Client) BoardTeamItems(ctx context.Context, q forge.TeamItemsQuery) (forge.TeamItems, error) {
	if q.TeamOption == "" {
		return forge.TeamItems{}, &forge.Error{Kind: forge.Refused, API: forge.APIGraphQL, Operation: forge.OpBoardTeamItems,
			Err: errors.New("no team option")}
	}
	activeQ, inboxQ := TeamFilters(q)
	withActive, withInbox := len(q.Statuses) > 0, inboxQ != ""
	var activeAfter, inboxAfter any
	var out forge.TeamItems
	for page := 0; (withActive || withInbox) && page < maxPages; page++ {
		vars := map[string]any{
			varOrg: q.Org, varNumber: q.Project,
			"activeQuery": activeQ, "activeFirst": wire.ActivePageSize, "activeAfter": activeAfter, "withActive": withActive,
			"inboxQuery": inboxQ, "inboxFirst": wire.InboxPageSize, "inboxAfter": inboxAfter, "withInbox": withInbox,
		}
		var d wire.BoardTeamItemsData
		errs, points, err := c.query(ctx, forge.OpBoardTeamItems, wire.BoardTeamItemsQuery, vars, &d)
		out.Points += points
		if err != nil {
			return forge.TeamItems{}, err
		}
		if d.Organization == nil || d.Organization.ProjectV2 == nil {
			return forge.TeamItems{}, refusedGQL(forge.OpBoardTeamItems, errs)
		}
		spoiled, err := c.partial(forge.OpBoardTeamItems, errs, activeNodes, inboxNodes)
		if err != nil {
			return forge.TeamItems{}, err
		}
		p := d.Organization.ProjectV2
		if withActive {
			if p.Active == nil {
				return forge.TeamItems{}, refusedGQL(forge.OpBoardTeamItems, errs)
			}
			for i, n := range p.Active.Nodes {
				bi := boardItem(n)
				bi.Incomplete = spoiled[node{0, i}]
				out.Active = append(out.Active, bi)
			}
			withActive, activeAfter = p.Active.PageInfo.HasNextPage, p.Active.PageInfo.EndCursor
		}
		if withInbox {
			if p.Inbox == nil {
				return forge.TeamItems{}, refusedGQL(forge.OpBoardTeamItems, errs)
			}
			for i, n := range p.Inbox.Nodes {
				bi := boardItem(n)
				bi.Incomplete = spoiled[node{1, i}]
				out.InboxBugs = append(out.InboxBugs, bi)
			}
			withInbox, inboxAfter = p.Inbox.PageInfo.HasNextPage, p.Inbox.PageInfo.EndCursor
		}
	}
	return out, nil
}

// BoardItem reads one board item by node id.
func (c *Client) BoardItem(ctx context.Context, nodeID string) (forge.BoardItem, bool, error) {
	items, err := c.BoardItemsByNode(ctx, []string{nodeID})
	if err != nil {
		return forge.BoardItem{}, false, err
	}
	it, ok := items[nodeID]
	return it, ok, nil
}

var itemNodes = []string{fieldNodes}

// BoardItemsByNode resolves board item node ids, 100 per request. An id GitHub doesn't find
// (NOT_FOUND on the node) is a deleted item; an error about part of an item marks it Incomplete;
// any other error refuses the read.
func (c *Client) BoardItemsByNode(ctx context.Context, nodeIDs []string) (map[string]forge.BoardItem, error) {
	out := map[string]forge.BoardItem{}
	for start := 0; start < len(nodeIDs); start += 100 {
		end := min(start+100, len(nodeIDs))
		var d wire.BoardItemsByNodeData
		errs, _, err := c.query(ctx, forge.OpBoardItemsByNode, wire.BoardItemsByNodeQuery, map[string]any{"ids": nodeIDs[start:end]}, &d)
		if err != nil {
			return nil, err
		}
		var rest []wire.GraphQLError
		for _, e := range errs {
			if _, inside := nodeIn(e.Path, itemNodes); inside {
				rest = append(rest, e)
				continue
			}
			if e.Type != wire.ErrorTypeNotFound {
				return nil, refusedGQL(forge.OpBoardItemsByNode, errs)
			}
		}
		spoiled, err := c.partial(forge.OpBoardItemsByNode, rest, itemNodes)
		if err != nil {
			return nil, err
		}
		for i, n := range d.Nodes {
			if n == nil || n.ID == "" || (n.Typename != "" && n.Typename != "ProjectV2Item") {
				continue
			}
			bi := boardItem(*n)
			bi.Incomplete = spoiled[node{0, i}]
			out[n.ID] = bi
		}
	}
	return out, nil
}

// ProbeFilter returns the board filter BoardProbe sends.
func ProbeFilter(team string, since time.Time) string {
	return fmt.Sprintf("team:%s updated:>=%s", filterValue(team), since.UTC().Format(time.DateOnly))
}

// maxProbePages bounds one probe: 1,000 items updated since the bound, 10 points.
const maxProbePages = 10

var probeNodes = []string{fieldOrganization, fieldProjectV2, "items", fieldNodes}

// BoardProbe asks for the team's items updated since q.UpdatedSince and returns them newest first.
// GitHub orders a board's items by position only, so the probe can't ask for the newest items:
// it filters by the day of the bound (the filter's granularity), pages through every match (100 per
// page, 1 point each, at most maxProbePages) and drops the matches older than the bound. q.First is
// not used: a page of 100 costs the same 1 point as a smaller one.
func (c *Client) BoardProbe(ctx context.Context, q forge.ProbeQuery) (forge.Probe, error) {
	since := q.UpdatedSince
	if since.IsZero() {
		since = c.cfg.Clock.Now().Add(-24 * time.Hour)
	}
	vars := map[string]any{varOrg: q.Org, varNumber: q.Project, "query": ProbeFilter(q.TeamOption, since),
		"first": wire.ProbePageSize, "after": nil}
	var p forge.Probe
	for page := 0; ; page++ {
		if page == maxProbePages {
			p.Truncated = true
			break
		}
		var d wire.BoardProbeData
		errs, _, err := c.query(ctx, forge.OpBoardProbe, wire.BoardProbeQuery, vars, &d)
		if err != nil {
			return forge.Probe{}, err
		}
		if d.Organization == nil || d.Organization.ProjectV2 == nil {
			return forge.Probe{}, refusedGQL(forge.OpBoardProbe, errs)
		}
		// An error inside an item leaves its id and update time, which is all the probe judges.
		if _, err := c.partial(forge.OpBoardProbe, errs, probeNodes); err != nil {
			return forge.Probe{}, err
		}
		items := d.Organization.ProjectV2.Items
		for _, n := range items.Nodes {
			if n.ID == "" || n.UpdatedAt.Before(since) {
				continue
			}
			pi := forge.ProbeItem{NodeID: n.ID, UpdatedAt: n.UpdatedAt}
			if n.Content != nil && n.Content.Typename == "Issue" && n.Content.Repository != nil {
				pi.IsIssue = true
				pi.IssueRef = n.Content.Repository.NameWithOwner + "#" + strconv.Itoa(n.Content.Number)
			}
			p.Items = append(p.Items, pi)
		}
		if !items.PageInfo.HasNextPage || items.PageInfo.EndCursor == "" {
			break
		}
		vars["after"] = items.PageInfo.EndCursor
	}
	sort.SliceStable(p.Items, func(i, j int) bool { return p.Items[i].UpdatedAt.After(p.Items[j].UpdatedAt) })
	return p, nil
}

func lower(s string) string { return strings.ToLower(s) }

func issueRef(r wire.IssueRef) string {
	return r.Repository.NameWithOwner + "#" + strconv.Itoa(r.Number)
}

// boardItem converts a board item of the GraphQL answer.
func boardItem(n wire.ProjectItem) forge.BoardItem {
	bi := forge.BoardItem{NodeID: n.ID, Archived: n.IsArchived, UpdatedAt: n.UpdatedAt}
	if n.Status != nil {
		bi.Status = n.Status.Name
		if n.Status.UpdatedAt != nil {
			bi.StatusAt = *n.Status.UpdatedAt
		}
	}
	if n.Team != nil {
		bi.Team = n.Team.Name
	}
	if n.Kind != nil {
		bi.Kind = n.Kind.Name
	}
	if n.Priority != nil {
		p := int(n.Priority.Number)
		bi.Priority = &p
	}
	ct := n.Content
	if ct == nil || ct.Typename != "Issue" || ct.Repository == nil {
		return bi
	}
	bi.IsIssue = true
	is := forge.Issue{
		Ref:         ct.Repository.NameWithOwner + "#" + strconv.Itoa(ct.Number),
		NodeID:      ct.ID,
		Title:       ct.Title,
		Body:        ct.Body,
		State:       lower(ct.State),
		StateReason: lower(ct.StateReason),
		Private:     ct.Repository.IsPrivate,
	}
	if ct.CreatedAt != nil {
		is.CreatedAt = *ct.CreatedAt
	}
	if ct.UpdatedAt != nil {
		is.UpdatedAt = *ct.UpdatedAt
	}
	if ct.Assignees != nil {
		for _, a := range ct.Assignees.Nodes {
			is.Assignees = append(is.Assignees, a.Login)
		}
	}
	if ct.Labels != nil {
		for _, l := range ct.Labels.Nodes {
			is.Labels = append(is.Labels, l.Name)
		}
	}
	if ct.Parent != nil {
		is.Parent = issueRef(*ct.Parent)
	}
	if ct.SubIssues != nil {
		for _, s := range ct.SubIssues.Nodes {
			if s.Repository.NameWithOwner == "" {
				continue // a sub-issue GitHub couldn't resolve (the answer carries its error)
			}
			is.SubIssues = append(is.SubIssues, forge.LinkedIssue{Ref: issueRef(s), State: lower(s.State), StateReason: lower(s.StateReason)})
		}
	}
	if ct.BlockedBy != nil {
		for _, s := range ct.BlockedBy.Nodes {
			if s.Repository.NameWithOwner == "" {
				continue
			}
			is.BlockedBy = append(is.BlockedBy, forge.LinkedIssue{Ref: issueRef(s), State: lower(s.State), StateReason: lower(s.StateReason)})
		}
	}
	// The totals count every link, also those the lists leave out (truncated, or unresolvable).
	is.SubIssuesTotal, is.BlockedByTotal = len(is.SubIssues), len(is.BlockedBy)
	if ct.SubIssuesSummary != nil {
		is.SubIssuesTotal = max(ct.SubIssuesSummary.Total, is.SubIssuesTotal)
	}
	if ct.DependenciesSummary != nil {
		is.BlockedByTotal = max(ct.DependenciesSummary.TotalBlockedBy, is.BlockedByTotal)
	}
	if ct.ClosedBy != nil {
		for _, p := range ct.ClosedBy.Nodes {
			is.ClosedBy = append(is.ClosedBy, forge.LinkedPR{
				Ref:    p.Repository.NameWithOwner + "#" + strconv.Itoa(p.Number),
				State:  lower(p.State),
				Draft:  p.IsDraft,
				Merged: p.Merged || p.State == wire.StateMerged,
			})
		}
	}
	bi.Issue = is
	return bi
}

// maxClosingPages bounds PullClosingIssues: 500 closing issues.
const maxClosingPages = 10

var closingNodes = []string{"repository", "pullRequest", "closingIssuesReferences", fieldNodes}

// PullClosingIssues reads the issues a PR closes (closingIssuesReferences: closing keywords and
// manual Development links), with their state, in one 1-point query per 50 issues. Status is left
// empty: the PR's side doesn't know which board the engine observes. A repo the installation can't
// read, or a PR that doesn't exist, is refused.
func (c *Client) PullClosingIssues(ctx context.Context, ref string) ([]forge.LinkedIssue, error) {
	repo, n, err := splitRef(ref)
	if err != nil {
		return nil, &forge.Error{Kind: forge.Refused, API: forge.APIGraphQL, Operation: forge.OpPullClosingIssues, Err: err}
	}
	owner, name, _ := splitRepo(repo)
	vars := map[string]any{"owner": owner, "name": name, varNumber: n, "first": wire.ClosingIssuesPageSize, "after": nil}
	var out []forge.LinkedIssue
	for page := 0; page < maxClosingPages; page++ {
		var d wire.PullClosingIssuesData
		errs, _, err := c.query(ctx, forge.OpPullClosingIssues, wire.PullClosingIssuesQuery, vars, &d)
		if err != nil {
			return nil, err
		}
		if d.Repository == nil || d.Repository.PullRequest == nil {
			return nil, refusedGQL(forge.OpPullClosingIssues, errs)
		}
		spoiled, err := c.partial(forge.OpPullClosingIssues, errs, closingNodes)
		if err != nil {
			return nil, err
		}
		conn := d.Repository.PullRequest.ClosingIssues
		for i, is := range conn.Nodes {
			if is.Repository.NameWithOwner == "" || is.Number == 0 {
				continue
			}
			li := forge.LinkedIssue{Ref: issueRef(is), State: lower(is.State), StateReason: lower(is.StateReason)}
			if spoiled[node{0, i}] {
				li.State, li.StateReason = "", "" // GitHub couldn't say: unknown, not open
			}
			out = append(out, li)
		}
		if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
			break
		}
		vars["after"] = conn.PageInfo.EndCursor
	}
	return out, nil
}
