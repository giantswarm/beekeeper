package board

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
)

// Client reads and moves the items of one board through the GitHub
// GraphQL API; Plans are the plans repositories (owner/repo) an epic's
// plan is found in.
type Client struct {
	GH    github.GH
	Board config.Board
	Plans []string
}

// field is a field of the board: a single-select one with its options, or
// a plain one with its data type (NUMBER for the Priority).
type field struct {
	ID       string
	DataType string
	Options  []struct{ ID, Name string }
}

func (f field) names() []string {
	out := make([]string, len(f.Options))
	for i, o := range f.Options {
		out[i] = o.Name
	}
	return out
}

// option is the id of the option named name, one of names().
func (f field) option(name string) string {
	return f.Options[slices.IndexFunc(f.Options, func(o struct{ ID, Name string }) bool { return o.Name == name })].ID
}

// meta is the board's id and its single-select and plain fields by name.
type meta struct {
	ID     string
	Fields map[string]field
}

// stateOpen is the state of an open issue or pull request.
const stateOpen = "OPEN"

// isOpen is a board query's filter of the open items.
const isOpen = "is:open"

// issueFields is what every read takes of an issue.
const issueFields = `fragment I on Issue{number url title state createdAt updatedAt repository{nameWithOwner}
labels(first:20){nodes{name}} assignees(first:10){nodes{login}} issueDependenciesSummary{blockedBy totalBlockedBy} subIssuesSummary{total completed}
parent{number repository{nameWithOwner}}}`

// boardFields is what every read takes of a board item, the Status with
// when it was set.
const boardFields = `status:fieldValueByName(name:"` + StatusField + `"){...on ProjectV2ItemFieldSingleSelectValue{name updatedAt}}
kind:fieldValueByName(name:"` + KindField + `"){...on ProjectV2ItemFieldSingleSelectValue{name}}`

// blockerFields is what a read of the board's items takes of an issue's
// recorded blockers: when each closed, the last close a blocked item's
// Status is weighed against (Item.HeldByStatus). An issue with more
// blockers has its last close unknown.
const blockerFields = `...on Issue{blockedBy(first:20){nodes{closedAt number state repository{nameWithOwner}}}}`

// itemFields is what a move takes of an item: its id, its board and the
// board fields with the Team.
const itemFields = `id project{id} ` + boardFields + `
team:fieldValueByName(name:"` + TeamField + `"){...on ProjectV2ItemFieldSingleSelectValue{name}}`

type issueJSON struct {
	Number     int       `json:"number"`
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct{ Name string } `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []struct{ Login string } `json:"nodes"`
	} `json:"assignees"`
	Deps struct {
		BlockedBy      int `json:"blockedBy"`
		TotalBlockedBy int `json:"totalBlockedBy"`
	} `json:"issueDependenciesSummary"`
	Subs struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
	} `json:"subIssuesSummary"`
	Parent *struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"parent"`
	// BlockedBy are the recorded blockers, read with blockerFields only.
	BlockedBy struct {
		Nodes []blockerNode `json:"nodes"`
	} `json:"blockedBy"`
}

// blockerNode is a recorded blocker as blockerFields reads it.
type blockerNode struct {
	ClosedAt *time.Time `json:"closedAt"`
	depRef
}

// blockersClosed is when the last of the recorded blockers closed: zero
// while one is open, for none, or when not every one was read.
func (j issueJSON) blockersClosed() time.Time {
	ns := j.BlockedBy.Nodes
	if len(ns) == 0 || len(ns) != j.Deps.TotalBlockedBy {
		return time.Time{}
	}
	var last time.Time
	for _, n := range ns {
		if n.ClosedAt == nil {
			return time.Time{}
		}
		if n.ClosedAt.After(last) {
			last = *n.ClosedAt
		}
	}
	return last
}

// open reports whether j is an open issue; a pull request or a draft has
// no URL.
func (j issueJSON) open() bool { return j.URL != "" && j.State == stateOpen }

func (j issueJSON) item() Item {
	it := Item{
		Ref: fmt.Sprintf("%s#%d", j.Repository.NameWithOwner, j.Number), URL: j.URL, Title: j.Title,
		Created: j.CreatedAt, Updated: j.UpdatedAt, Blockers: j.Deps.TotalBlockedBy, OpenBlockers: j.Deps.BlockedBy, OpenSubIssues: j.Subs.Total - j.Subs.Completed,
		subIssues: j.Subs.Total, BlockersClosed: j.blockersClosed(),
	}
	for _, l := range j.Labels.Nodes {
		it.Labels = append(it.Labels, l.Name)
	}
	for _, a := range j.Assignees.Nodes {
		it.Assignees = append(it.Assignees, a.Login)
	}
	for _, b := range j.BlockedBy.Nodes {
		if b.State == stateOpen {
			it.OpenBlockedBy = append(it.OpenBlockedBy, fmt.Sprintf("%s#%d", b.Repository.NameWithOwner, b.Number))
		}
	}
	if p := j.Parent; p != nil {
		it.Parent = fmt.Sprintf("%s#%d", p.Repository.NameWithOwner, p.Number)
	}
	return it
}

// vars are a query's variables: a string or an int each.
type vars map[string]any

// GraphQLError is one error GitHub answered beside the data: Path is the
// field it could not resolve (aliases, field names and list indexes), Type
// GitHub's kind of error (NOT_FOUND, RESOURCE_LIMITS_EXCEEDED, …).
type GraphQLError struct {
	Path    []string
	Type    string
	Message string
}

// Partial is an answer with errors beside its data: the data is decoded,
// and every field an error's path names is missing from it, never absent.
type Partial struct{ Errors []GraphQLError }

// partialShown is how many of a Partial's errors its message names.
const partialShown = 3

func (p *Partial) Error() string {
	msgs := make([]string, 0, partialShown+1)
	for i, e := range p.Errors {
		if i == partialShown {
			msgs = append(msgs, fmt.Sprintf("and %d more", len(p.Errors)-partialShown))
			break
		}
		msgs = append(msgs, strings.Join(e.Path, ".")+": "+e.Message)
	}
	return "GitHub answered partially: " + strings.Join(msgs, "; ")
}

// reason is p's messages as the tail of a sentence, "" for nil.
func (p *Partial) reason() string {
	if p == nil {
		return ""
	}
	msgs := make([]string, len(p.Errors))
	for i, e := range p.Errors {
		msgs[i] = e.Message
	}
	return ": " + strings.Join(msgs, "; ")
}

// partialOf is err's Partial, nil for none; ok is false for any other
// error, which the caller returns.
func partialOf(err error) (p *Partial, ok bool) {
	return p, err == nil || errors.As(err, &p)
}

// graphql runs one query with its variables into v. An answer with errors
// beside its data is decoded into v and returned as a *Partial: a caller
// reads it with partialOf and either fails or knows which fields it lacks.
func (c *Client) graphql(ctx context.Context, v any, query string, vs vars) error {
	args := []string{"api", "graphql", "-f", "query=" + query}
	for k, x := range vs {
		if n, ok := x.(int); ok {
			args = append(args, "-F", fmt.Sprintf("%s=%d", k, n))
		} else {
			args = append(args, "-f", fmt.Sprintf("%s=%s", k, x))
		}
	}
	out, err := c.GH(ctx, args...)
	var doc struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Path    []any  `json:"path"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	jerr := json.Unmarshal(out, &doc)
	p := &Partial{}
	for _, e := range doc.Errors {
		path := make([]string, len(e.Path))
		for i, x := range e.Path {
			path[i] = fmt.Sprint(x)
		}
		p.Errors = append(p.Errors, GraphQLError{Path: path, Type: e.Type, Message: e.Message})
	}
	if jerr != nil || len(doc.Data) == 0 || string(doc.Data) == "null" {
		if len(p.Errors) == 0 {
			return cmp.Or(err, errors.New("gh api graphql: no data"))
		}
		return cmp.Or(err, errors.New("gh api graphql"+p.reason()))
	}
	if err := json.Unmarshal(doc.Data, v); err != nil {
		return err
	}
	if len(p.Errors) > 0 {
		return p
	}
	return nil
}

func (c *Client) owner() (string, error) {
	if c.Board.Owner == "" || c.Board.Project == 0 {
		return "", errors.New("no board configured: set board.owner and board.project")
	}
	return c.Board.Owner, nil
}

func (c *Client) meta(ctx context.Context) (*meta, error) {
	owner, err := c.owner()
	if err != nil {
		return nil, err
	}
	var r struct {
		Owner *struct {
			Project *struct {
				ID     string `json:"id"`
				Fields struct {
					Nodes []struct {
						ID       string `json:"id"`
						Name     string `json:"name"`
						DataType string `json:"dataType"`
						Options  []struct{ ID, Name string }
					} `json:"nodes"`
				} `json:"fields"`
			} `json:"projectV2"`
		} `json:"repositoryOwner"`
	}
	err = c.graphql(ctx, &r, `query($o:String!,$n:Int!){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){id
fields(first:50){nodes{...on ProjectV2SingleSelectField{id name options{id name}} ...on ProjectV2Field{id name dataType}}}}}}}`,
		vars{"o": owner, "n": c.Board.Project})
	if err != nil {
		return nil, err
	}
	if r.Owner == nil || r.Owner.Project == nil {
		return nil, fmt.Errorf("board %s/%d not found", owner, c.Board.Project)
	}
	m := &meta{ID: r.Owner.Project.ID, Fields: map[string]field{}}
	for _, f := range r.Owner.Project.Fields.Nodes {
		if f.ID != "" {
			m.Fields[f.Name] = field{ID: f.ID, DataType: f.DataType, Options: f.Options}
		}
	}
	if _, ok := m.Fields[StatusField]; !ok {
		return nil, fmt.Errorf("board %s/%d has no %s field", owner, c.Board.Project, StatusField)
	}
	return m, nil
}

// filter is the board query of the order's items: open, the team's, and
// in the statuses its steps name and extra when every board step names
// some.
func filter(b config.Board, order []config.BoardStep, extra ...string) string {
	who := ""
	if b.Team != "" {
		who = fmt.Sprintf("%s:%q", strings.ToLower(TeamField), b.Team)
	}
	return filterOf(who, order, extra...)
}

// filterOf is filter with who, the query part that picks the team's items
// ("" for every item).
func filterOf(who string, order []config.BoardStep, extra ...string) string {
	parts := []string{isOpen}
	if who != "" {
		parts = append(parts, who)
	}
	var statuses []string
	for _, st := range order {
		if st.Search != "" {
			continue
		}
		if len(st.Status) == 0 {
			return strings.Join(parts, " ")
		}
		for _, s := range st.Status {
			if !slices.Contains(statuses, s) {
				statuses = append(statuses, s)
			}
		}
	}
	for _, s := range extra {
		if !slices.Contains(statuses, s) {
			statuses = append(statuses, s)
		}
	}
	if len(statuses) > 0 {
		q := make([]string, len(statuses))
		for i, s := range statuses {
			q[i] = strconv.Quote(s)
		}
		parts = append(parts, strings.ToLower(StatusField)+":"+strings.Join(q, ","))
	}
	return strings.Join(parts, " ")
}

// rankedOffTeam adds to snap.Items the epics of the priority list its read
// of the team's items missed: ranked epics that carry the team label while
// their Team field says another team, read by the label (one request per
// 100 items) and kept only when ranked.
func (c *Client) rankedOffTeam(ctx context.Context, snap *Snapshot, order []config.BoardStep, gate Gate) error {
	l := c.TeamLabel()
	if c.Board.Team == "" || l == "" {
		return nil
	}
	have := map[string]bool{}
	for _, it := range snap.Items {
		have[strings.ToLower(it.Ref)] = true
	}
	missing := map[string]bool{}
	for _, e := range snap.Priorities {
		if !have[strings.ToLower(e.Ref)] {
			missing[strings.ToLower(e.Ref)] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	items, err := c.items(ctx, filterOf(fmt.Sprintf("label:%q", l), order, gate.statuses()...))
	if err != nil {
		return err
	}
	for _, it := range items {
		if missing[strings.ToLower(it.Ref)] {
			snap.Items = append(snap.Items, it)
		}
	}
	return nil
}

// Read reads the board items the order can offer and the epic gate holds,
// the open sub-issues of the items a SubIssues step matches, the search
// steps' issues, the priority list (Priorities, kept as snap.Priorities)
// with the ranked epics a Team field moved off the team but its label keeps
// (rankedOffTeam), and the plans of the epics the gate watches: one request
// for the fields, one per 100 items, one per extrasBudget sub-issues for the
// sub-issues and searches, one per plansBatch epics.
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	if len(c.Board.Order) == 0 {
		return nil, errors.New("board.order is empty: configure the picking order")
	}
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	order, err := ResolveOrder(c.Board.Order, m.Fields[StatusField].names(), m.Fields[KindField].names())
	if err != nil {
		return nil, err
	}
	gate, err := ResolveGate(c.Board.Epics, m.Fields[StatusField].names(), m.Fields[KindField].names())
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Order: order, Statuses: m.Fields[StatusField].names(), Search: map[int][]Item{}, Gate: gate}
	if snap.Items, err = c.items(ctx, filter(c.Board, order, gate.statuses()...)); err != nil {
		return nil, err
	}
	if snap.Priorities, err = c.priorities(ctx, m); err != nil {
		return nil, err
	}
	if err := c.rankedOffTeam(ctx, snap, order, gate); err != nil {
		return nil, err
	}
	if err := c.extras(ctx, snap, m.ID, now); err != nil {
		return nil, err
	}
	snap.Plans, err = c.plansOf(ctx, snap)
	return snap, err
}

func (c *Client) items(ctx context.Context, q string) ([]Item, error) {
	var out []Item
	for cursor := ""; ; {
		var r struct {
			Owner struct {
				Project struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							projectItem
							// Content is empty for a pull request or a draft.
							Content issueJSON `json:"content"`
						} `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"repositoryOwner"`
		}
		vs := vars{"o": c.Board.Owner, "n": c.Board.Project, "q": q}
		if cursor != "" {
			vs["c"] = cursor
		}
		err := c.graphql(ctx, &r, `query($o:String!,$n:Int!,$q:String!,$c:String){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){
items(first:100,after:$c,query:$q){pageInfo{hasNextPage endCursor} nodes{`+boardFields+`
content{...I `+blockerFields+`}}}}}}}`+issueFields, vs)
		if err != nil {
			return nil, err
		}
		items := r.Owner.Project.Items
		for _, n := range items.Nodes {
			if !n.Content.open() {
				continue
			}
			it := n.Content.item()
			n.onto(&it)
			out = append(out, it)
		}
		if !items.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = items.PageInfo.EndCursor
	}
}

// The sub-issues one request of extras reads: GitHub answers a request of
// 304 sub-issues with their projectItems in full and one of 396 with
// RESOURCE_LIMITS_EXCEEDED on every projectItems, so a request stays at
// half the largest one measured whole.
const (
	extrasBudget  = 150
	subIssuesRead = 50
)

// extrasPart is one aliased field of an extras request, the sub-issues it
// reads and the repository (owner/repo) it reads them of, "" for a search.
type extrasPart struct {
	field string
	subs  int
	repo  string
}

// extrasBatches splits parts into requests of at most extrasBudget
// sub-issues each, in their order; a part over the budget is a request of
// its own.
func extrasBatches(parts []extrasPart) [][]extrasPart {
	var out [][]extrasPart
	var cur []extrasPart
	n := 0
	for _, p := range parts {
		if len(cur) > 0 && n+p.subs > extrasBudget {
			out = append(out, cur)
			cur, n = nil, 0
		}
		cur = append(cur, p)
		n += p.subs
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// extras reads the open sub-issues of the items with any a SubIssues step
// matches, with their board fields when they are items of the board
// project, whatever their Team or Status, and the search steps' issues,
// extrasBudget sub-issues per request. What GitHub answers with an error is
// named in the Missing of the item it belongs to, else of snap; the rest of
// the answer is kept.
func (c *Client) extras(ctx context.Context, snap *Snapshot, project string, now time.Time) error {
	var parts []extrasPart
	var epics []int
	for _, st := range snap.Order {
		if !st.SubIssues {
			continue
		}
		for i, it := range snap.Items {
			if it.OpenSubIssues > 0 && !slices.Contains(epics, i) && Matches(st, it, now) {
				epics = append(epics, i)
				owner, repo, n, err := Ref(it.Ref)
				if err != nil {
					return err
				}
				parts = append(parts, extrasPart{
					field: fmt.Sprintf("e%d:repository(owner:%s,name:%s){issue(number:%d){subIssues(first:%d){nodes{...I projectItems(first:20){nodes{project{id} "+boardFields+"}}}}}}", i, quote(owner), quote(repo), n, subIssuesRead),
					subs:  min(it.subIssues, subIssuesRead),
					repo:  owner + "/" + repo,
				})
			}
		}
	}
	for i, st := range snap.Order {
		if st.Search != "" {
			parts = append(parts, extrasPart{field: fmt.Sprintf("s%d:search(query:%s,type:ISSUE,first:50){nodes{...I}}", i, quote(searchQuery(st.Search)))})
		}
	}
	r := map[string]json.RawMessage{}
	var missing []GraphQLError
	// Each request reads the epics of one owner, acting on their repository
	// (github.ActingOn), so a personal account's epic resolves too; the
	// searches go with the board owner's epics.
	var batches [][]extrasPart
	for _, group := range github.ByOwner(parts, func(p extrasPart) string { return cmp.Or(p.repo, c.Board.Owner+"/") }) {
		batches = append(batches, extrasBatches(group)...)
	}
	for _, batch := range batches {
		var q strings.Builder
		for _, p := range batch {
			q.WriteString(p.field)
		}
		var part map[string]json.RawMessage
		err := c.graphql(github.ActingOn(ctx, batch[0].repo), &part, "query{"+q.String()+"}"+issueFields, nil)
		p, ok := partialOf(err)
		if !ok {
			return err
		}
		if p != nil {
			missing = append(missing, p.Errors...)
		}
		maps.Copy(r, part)
	}
	type nodes struct {
		Nodes []issueJSON `json:"nodes"`
	}
	open := func(ns []issueJSON) []Item {
		var out []Item
		for _, n := range ns {
			if n.open() {
				out = append(out, n.item())
			}
		}
		return out
	}
	// Each error is named on what it belongs to: a sub-issue (by its epic's
	// alias and its index among the nodes read), the epic, else snap.
	epicOf := map[string]int{}
	for _, i := range epics {
		epicOf[fmt.Sprintf("e%d", i)] = i
	}
	subMissing := map[string][]string{}
	for _, e := range missing {
		alias := at(e.Path, 0)
		i, epic := epicOf[alias]
		switch {
		case epic && at(e.Path, 3) == "nodes" && len(e.Path) > 5:
			key := alias + "/" + e.Path[4]
			subMissing[key] = noted(subMissing[key], e.Path[5]+": "+e.Message)
		case epic:
			snap.Items[i].Missing = noted(snap.Items[i].Missing, strings.Join(e.Path[1:], ".")+": "+e.Message)
		default:
			name := strings.Join(e.Path, ".")
			if s, ok := strings.CutPrefix(alias, "s"); ok {
				if n, err := strconv.Atoi(s); err == nil && n < len(snap.Order) {
					name = fmt.Sprintf("board.order %q", snap.Order[n].Name)
				}
			}
			snap.Missing = noted(snap.Missing, name+": "+e.Message)
		}
	}
	for _, i := range epics {
		var e struct {
			Issue struct {
				SubIssues struct {
					Nodes []struct {
						issueJSON
						ProjectItems struct {
							Nodes []projectItem `json:"nodes"`
						} `json:"projectItems"`
					} `json:"nodes"`
				} `json:"subIssues"`
			} `json:"issue"`
		}
		raw, ok := r[fmt.Sprintf("e%d", i)]
		if !ok || json.Unmarshal(raw, &e) != nil {
			continue
		}
		for k, n := range e.Issue.SubIssues.Nodes {
			if !n.open() {
				continue
			}
			it := n.item()
			if j := slices.IndexFunc(n.ProjectItems.Nodes, func(p projectItem) bool { return p.Project.ID == project }); j >= 0 {
				n.ProjectItems.Nodes[j].onto(&it)
			}
			it.Missing = subMissing[fmt.Sprintf("e%d/%d", i, k)]
			snap.Items[i].SubIssues = append(snap.Items[i].SubIssues, it)
		}
	}
	for i := range snap.Order {
		var s nodes
		if raw, ok := r[fmt.Sprintf("s%d", i)]; ok {
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("board.order %q: %w", snap.Order[i].Name, err)
			}
			snap.Search[i] = open(s.Nodes)
		}
	}
	return nil
}

// at is path's element i, "" past its end.
func at(path []string, i int) string {
	if i < len(path) {
		return path[i]
	}
	return ""
}

// noted is list with s, which it holds once.
func noted(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// searchQuery is a search step's query for open issues, oldest first.
func searchQuery(s string) string {
	for _, q := range []string{"is:issue", "is:open"} {
		if !strings.Contains(s, q) {
			s += " " + q
		}
	}
	if !strings.Contains(s, "sort:") {
		s += " sort:created-asc"
	}
	return s
}

// Open reports whether the issue or pull request ref is open, acting on
// its repository (github.ActingOn).
func (c *Client) Open(ctx context.Context, ref string) (bool, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return false, err
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	var r struct {
		Repository struct {
			Item *struct{ State string } `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	err = c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issueOrPullRequest(number:$n){...on Issue{state} ...on PullRequest{state}}}}`,
		vars{"o": owner, "r": repo, "n": n})
	p, ok := partialOf(err)
	if !ok {
		return false, err
	}
	if r.Repository.Item == nil {
		return false, &Refusal{fmt.Sprintf("%s/%s#%d is no issue%s", owner, repo, n, p.reason())}
	}
	if p != nil {
		return false, err
	}
	return r.Repository.Item.State == stateOpen, nil
}

// closesBatch is how many refs one request of Closes asks about: the query
// stays far under an argument's size limit, and a request's cost bounded.
const closesBatch = 50

// Closes reads the issues each pull request among refs closes (its closing
// references, the Closes and Fixes keywords resolved across repositories),
// keyed by the lower-cased ref as given, closesBatch refs of one owner per
// request, acting on their repository (github.ActingOn). An issue among
// refs, or a ref that is no issue, closes nothing; nil when nothing was
// asked.
func (c *Client) Closes(ctx context.Context, refs []string) (map[string][]string, error) {
	var out map[string][]string
	var batches [][]string
	for _, group := range github.ByOwner(refs, refRepo) {
		batches = slices.AppendSeq(batches, slices.Chunk(group, closesBatch))
	}
	for _, batch := range batches {
		part, err := c.closes(github.ActingOn(ctx, refRepo(batch[0])), batch)
		if err != nil {
			return nil, err
		}
		if part == nil {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		maps.Copy(out, part)
	}
	return out, nil
}

// closes is one request of Closes.
func (c *Client) closes(ctx context.Context, refs []string) (map[string][]string, error) {
	var q strings.Builder
	var asked []string
	for _, ref := range refs {
		owner, repo, n, err := Ref(ref)
		if err != nil {
			continue
		}
		fmt.Fprintf(&q, "r%d:repository(owner:%s,name:%s){issueOrPullRequest(number:%d){...on PullRequest{closingIssuesReferences(first:20){nodes{number repository{nameWithOwner}}}}}}", len(asked), quote(owner), quote(repo), n)
		asked = append(asked, strings.ToLower(ref))
	}
	if len(asked) == 0 {
		return nil, nil
	}
	var r map[string]*struct {
		Item *struct {
			Closing struct {
				Nodes []struct {
					Number     int `json:"number"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"nodes"`
			} `json:"closingIssuesReferences"`
		} `json:"issueOrPullRequest"`
	}
	// A ref GitHub does not resolve closes nothing; an error on a pull
	// request it resolved leaves what it closes unknown.
	err := c.graphql(ctx, &r, "query{"+q.String()+"}", nil)
	p, ok := partialOf(err)
	if p != nil {
		ok = !slices.ContainsFunc(p.Errors, func(e GraphQLError) bool {
			x := r[at(e.Path, 0)]
			return x != nil && x.Item != nil
		})
	}
	if !ok {
		return nil, fmt.Errorf("the issues the served pull requests close: %w", err)
	}
	out := map[string][]string{}
	for i, ref := range asked {
		x := r[fmt.Sprintf("r%d", i)]
		if x == nil || x.Item == nil {
			continue
		}
		for _, n := range x.Item.Closing.Nodes {
			out[ref] = append(out[ref], strings.ToLower(fmt.Sprintf("%s#%d", n.Repository.NameWithOwner, n.Number)))
		}
	}
	return out, nil
}

// Moved is a move of an item to a status.
type Moved struct {
	Ref  string `json:"ref"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// Added says the issue had no item on the board: one was added before
	// the move. Team is the Team the item was given, board.team, "" when
	// it had one or the board names none.
	Added bool   `json:"added,omitempty"`
	Team  string `json:"team,omitempty"`
	// Go is the go the move is, an epic moved to the epic gate's go
	// status; nil for any other move.
	Go *Go `json:"go,omitempty"`
	// Board is the project the item moved on when it is not the configured
	// board: the project of the issue's repository's own owner
	// (owner/number); "" for the configured board.
	Board string `json:"board,omitempty"`
	// Kept says a release left the item as it is: its Status was changed
	// since the hold set it (Release).
	Kept bool `json:"kept,omitempty"`
}

// Move sets the Status of the board item of the issue ref to the status in
// stands for (see Resolve): the item the issue's projectItems name, else
// the one addProjectV2ItemById answers, which is the issue's existing item
// or a new one, with board.team as its Team when it has none; no read of
// the board's items. Issues from any accessible repository use the configured
// board, subject to GitHub permissions. Every call acts on the
// issue's repository (github.ActingOn), so a gh link choosing its login by
// the owner reaches the issue. An unknown or ambiguous status, and a
// board.team that is none of the Team field's values, are refused with the
// board's values before anything is written; an issue GitHub does not
// resolve is refused as no issue, with GitHub's reason. A move of an epic
// to the epic gate's go status is the go: Moved.Go names its plan and the
// private thread to record it on (RecordGo), and an epic with a plan but
// no private thread is refused before anything is written.
func (c *Client) Move(ctx context.Context, ref, in string) (*Moved, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	mv := &Moved{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n)}
	id, found, _, err := c.lookup(ctx, m, mv.Ref, owner, repo, n)
	if err != nil {
		return nil, err
	}
	sf := m.Fields[StatusField]
	to, err := Resolve(StatusField, sf.names(), in)
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	mv.To = to
	team, err := c.team(m)
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	item, err := c.ensure(ctx, m, mv, id, found)
	if err != nil {
		return nil, err
	}
	if item.Status != nil {
		mv.From = item.Status.Name
	}
	gate, err := ResolveGate(c.Board.Epics, sf.names(), m.Fields[KindField].names())
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	var it Item
	item.onto(&it)
	if mv.From != to && gate.Go != "" && to == gate.Go && gate.Epic(it) {
		if mv.Go, err = c.goOf(ctx, mv.Ref); err != nil {
			return nil, err
		}
	}
	if item.Team == nil && team != nil {
		if err := c.set(ctx, m.ID, item.ID, team.field, team.id); err != nil {
			return nil, err
		}
		mv.Team = team.name
	}
	if mv.From == to {
		return mv, nil
	}
	if err := c.set(ctx, m.ID, item.ID, sf.ID, sf.option(to)); err != nil {
		return nil, err
	}
	return mv, nil
}

// item is the issue's item on the board, read from the issue's
// projectItems, else answered by addProjectV2ItemById: the issue's
// existing item, or a new one, which marks mv added. An issue GitHub does
// not resolve is refused as no issue. Rank and priority add an issue of any
// accessible repository as Move does, whatever its owner: GitHub decides
// whether it may be added. An item added outside beekeeper that the issue's
// projectItems do not list (GitHub lists an item of another owner's project
// under none) is the one the add answers.
func (c *Client) item(ctx context.Context, m *meta, mv *Moved, owner, repo string, n int) (projectItem, error) {
	id, found, _, err := c.lookup(ctx, m, mv.Ref, owner, repo, n)
	if err != nil {
		return projectItem{}, err
	}
	return c.ensure(ctx, m, mv, id, found)
}

// ensure is the item found, else the issue id's item addProjectV2ItemById
// answers, as item reads it. GitHub decides whether the issue may be added.
func (c *Client) ensure(ctx context.Context, m *meta, mv *Moved, id string, found *projectItem) (projectItem, error) {
	if found != nil {
		return *found, nil
	}
	var a struct {
		Added struct {
			Item *projectItem `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	err := c.graphql(ctx, &a, `mutation($p:ID!,$c:ID!){addProjectV2ItemById(input:{projectId:$p,contentId:$c}){item{`+itemFields+`}}}`,
		vars{"p": m.ID, "c": id})
	p, ok := partialOf(err)
	if !ok {
		return projectItem{}, err
	}
	if a.Added.Item == nil || a.Added.Item.ID == "" {
		return projectItem{}, fmt.Errorf("adding %s to the board %s/%d%s", mv.Ref, c.Board.Owner, c.Board.Project, cmp.Or(p.reason(), ": no item answered"))
	}
	if p != nil {
		return projectItem{}, fmt.Errorf("adding %s to the board %s/%d: %w", mv.Ref, c.Board.Owner, c.Board.Project, err)
	}
	it := *a.Added.Item
	// An item that existed carries its fields; a new one none.
	mv.Added = it.Status == nil && it.Team == nil
	return it, nil
}

// lookup reads the issue's node id and its item on the board from the
// issue's projectItems, nil when they name none, and every item they name. An issue
// GitHub does not resolve is refused as no issue, ref naming it.
func (c *Client) lookup(ctx context.Context, m *meta, ref, owner, repo string, n int) (string, *projectItem, []projectItem, error) {
	var r struct {
		Repository struct {
			Issue *struct {
				ID           string `json:"id"`
				ProjectItems struct {
					Nodes []projectItem `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	err := c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){id projectItems(first:50){nodes{
`+itemFields+`}}}}}`,
		vars{"o": owner, "r": repo, "n": n})
	p, ok := partialOf(err)
	if !ok {
		return "", nil, nil, err
	}
	is := r.Repository.Issue
	if is == nil {
		return "", nil, nil, &Refusal{ref + " is no issue" + p.reason()}
	}
	if p != nil {
		// Its items unknown, the issue is never taken for one off the board.
		return "", nil, nil, fmt.Errorf("the board items of %s: %w", ref, err)
	}
	items := is.ProjectItems.Nodes
	if i := slices.IndexFunc(items, func(x projectItem) bool { return x.Project.ID == m.ID }); i >= 0 {
		return is.ID, &items[i], items, nil
	}
	return is.ID, nil, items, nil
}

// Place is where Rank puts an item: at the top of the board, or right
// before or right after the item of another issue, one of the three.
type Place struct {
	Top           bool
	Before, After string
}

// Ranked is a move of an item to a place on the board.
type Ranked struct {
	Ref string `json:"ref"`
	// Above and Below are the item's new neighbours that the move itself
	// names: the item it went after or before, and for the top the item
	// now below it; "" for one no request read. A neighbour that is no
	// issue is "a draft".
	Above string `json:"above,omitempty"`
	Below string `json:"below,omitempty"`
	Top   bool   `json:"top,omitempty"`
	// Added and Team are as in Moved: the issue had no item on the board.
	Added bool   `json:"added,omitempty"`
	Team  string `json:"team,omitempty"`
}

// Rank moves the board item of the issue ref to the place at with
// updateProjectV2ItemPosition, the board's position being the order
// board next reads its items in. An issue without an item is added first,
// as Move adds it, with board.team as its Team. The item of a Before or
// After issue is found by that issue's projectItems and must be on the
// board already; Before is two moves, the item after the other one and the
// other one after the item, so nothing reads the board's items to find the
// other one's predecessor. Each call acts on the repository of the issue it
// asks about. No issue, a reference without an item on the board and an
// item placed against itself are refused before anything is written.
func (c *Client) Rank(ctx context.Context, ref string, at Place) (*Ranked, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	other := cmp.Or(at.Before, at.After)
	if at.Top == (other != "") || (at.Before != "" && at.After != "") {
		return nil, errors.New("rank takes one of top, before and after")
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	team, err := c.team(m)
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	rk := &Ranked{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n), Top: at.Top}
	var otherID string
	if other != "" {
		oo, or, on, err := Ref(other)
		if err != nil {
			return nil, err
		}
		oref := fmt.Sprintf("%s/%s#%d", oo, or, on)
		if strings.EqualFold(oref, rk.Ref) {
			return nil, &Refusal{rk.Ref + " cannot be placed against itself"}
		}
		_, found, _, err := c.lookup(github.ActingOn(ctx, oo+"/"+or), m, oref, oo, or, on)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, &Refusal{fmt.Sprintf("%s is not on the board %s/%d", oref, c.Board.Owner, c.Board.Project)}
		}
		otherID = found.ID
		if at.Before != "" {
			rk.Below = oref
		} else {
			rk.Above = oref
		}
	}
	mv := &Moved{Ref: rk.Ref}
	item, err := c.item(ctx, m, mv, owner, repo, n)
	if err != nil {
		return nil, err
	}
	rk.Added = mv.Added
	if item.Team == nil && team != nil {
		if err := c.set(ctx, m.ID, item.ID, team.field, team.id); err != nil {
			return nil, err
		}
		rk.Team = team.name
	}
	below, err := c.position(ctx, m.ID, item.ID, otherID) // after the other, or to the top
	if err != nil {
		return nil, err
	}
	switch {
	case at.Top:
		rk.Below = below
	case at.Before != "":
		if _, err := c.position(ctx, m.ID, otherID, item.ID); err != nil {
			return nil, err
		}
	}
	return rk, nil
}

// position moves the item of the project right after the item after, to
// the top for "", and answers the issue (or "a draft") then right below
// it when it went to the top, "" otherwise.
func (c *Client) position(ctx context.Context, project, item, after string) (string, error) {
	vs := vars{"p": project, "i": item}
	head, arg, read := "", "", ""
	if after != "" {
		head, arg, vs["a"] = ",$a:ID!", ",afterId:$a", after
	} else {
		read = `items(first:2){nodes{id content{...on Issue{number repository{nameWithOwner}} ...on PullRequest{number repository{nameWithOwner}}}}}`
	}
	var r struct {
		Moved *struct {
			Items struct {
				Nodes []struct {
					ID      string `json:"id"`
					Content struct {
						Number     int `json:"number"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"content"`
				} `json:"nodes"`
			} `json:"items"`
		} `json:"updateProjectV2ItemPosition"`
	}
	err := c.graphql(ctx, &r, `mutation($p:ID!,$i:ID!`+head+`){updateProjectV2ItemPosition(input:{projectId:$p,itemId:$i`+arg+`}){clientMutationId `+read+`}}`, vs)
	p, ok := partialOf(err)
	if !ok {
		return "", err
	}
	if r.Moved == nil {
		return "", fmt.Errorf("moving the item %s on the board %s/%d%s", item, c.Board.Owner, c.Board.Project, cmp.Or(p.reason(), ": no answer"))
	}
	if p != nil {
		return "", fmt.Errorf("moved the item %s on the board %s/%d, its neighbours unread: %w", item, c.Board.Owner, c.Board.Project, err)
	}
	for _, x := range r.Moved.Items.Nodes {
		if x.ID == item {
			continue
		}
		if x.Content.Number == 0 {
			return "a draft", nil
		}
		return fmt.Sprintf("%s#%d", x.Content.Repository.NameWithOwner, x.Content.Number), nil
	}
	return "", nil
}

// option is one option of a single-select field: the field's id, the
// option's id and its name.
type option struct{ field, id, name string }

// team is the Team option board.team names; nil when board.team is empty
// or the board has no Team field. A board.team that is none of the field's
// values is an error naming them.
func (c *Client) team(m *meta) (*option, error) {
	tf, ok := m.Fields[TeamField]
	if !ok || c.Board.Team == "" {
		return nil, nil
	}
	name, err := Resolve(TeamField, tf.names(), c.Board.Team)
	if err != nil {
		return nil, fmt.Errorf("board.team: %w", err)
	}
	return &option{field: tf.ID, id: tf.option(name), name: name}, nil
}

// set sets the single-select field f of the item of the project to the
// option o.
func (c *Client) set(ctx context.Context, project, item, f, o string) error {
	var done json.RawMessage
	return c.graphql(ctx, &done, `mutation($p:ID!,$i:ID!,$f:ID!,$v:String!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{singleSelectOptionId:$v}}){projectV2Item{id}}}`,
		vars{"p": project, "i": item, "f": f, "v": o})
}

// projectItem is an issue's item on a board.
type projectItem struct {
	ID      string              `json:"id"`
	Project struct{ ID string } `json:"project"`
	Status  *struct {
		Name      string
		UpdatedAt time.Time `json:"updatedAt"`
	} `json:"status"`
	Kind *struct{ Name string } `json:"kind"`
	Team *struct{ Name string } `json:"team"`
}

// onto marks it a board item with the board fields of p.
func (p projectItem) onto(it *Item) {
	it.OnBoard = true
	if p.Status != nil {
		it.Status, it.StatusSet = p.Status.Name, p.Status.UpdatedAt
	}
	if p.Kind != nil {
		it.Kind = p.Kind.Name
	}
}

// Refusal is a move the board's values or items do not allow.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// quote is s as a GraphQL string literal, whose escapes are JSON's.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
