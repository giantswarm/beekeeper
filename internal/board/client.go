package board

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
)

// Client reads and moves the items of one board through the GitHub
// GraphQL API.
type Client struct {
	GH    github.GH
	Board config.Board
}

// field is a single-select field of the board.
type field struct {
	ID      string
	Options []struct{ ID, Name string }
}

func (f field) names() []string {
	out := make([]string, len(f.Options))
	for i, o := range f.Options {
		out[i] = o.Name
	}
	return out
}

// meta is the board's id and single-select fields by name.
type meta struct {
	ID     string
	Fields map[string]field
}

// issueFields is what every read takes of an issue.
const issueFields = `fragment I on Issue{number url title state createdAt updatedAt repository{nameWithOwner}
labels(first:20){nodes{name}} assignees(first:10){nodes{login}} issueDependenciesSummary{blockedBy totalBlockedBy} subIssuesSummary{total completed}}`

// boardFields is what every read takes of a board item.
const boardFields = `status:fieldValueByName(name:"` + StatusField + `"){...on ProjectV2ItemFieldSingleSelectValue{name}}
kind:fieldValueByName(name:"` + KindField + `"){...on ProjectV2ItemFieldSingleSelectValue{name}}`

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
}

// open reports whether j is an open issue; a pull request or a draft has
// no URL.
func (j issueJSON) open() bool { return j.URL != "" && j.State == "OPEN" }

func (j issueJSON) item() Item {
	it := Item{
		Ref: fmt.Sprintf("%s#%d", j.Repository.NameWithOwner, j.Number), URL: j.URL, Title: j.Title,
		Created: j.CreatedAt, Updated: j.UpdatedAt, Blockers: j.Deps.TotalBlockedBy, OpenBlockers: j.Deps.BlockedBy, OpenSubIssues: j.Subs.Total - j.Subs.Completed,
	}
	for _, l := range j.Labels.Nodes {
		it.Labels = append(it.Labels, l.Name)
	}
	for _, a := range j.Assignees.Nodes {
		it.Assignees = append(it.Assignees, a.Login)
	}
	return it
}

// vars are a query's variables: a string or an int each.
type vars map[string]any

// graphql runs one query with its variables into v.
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
			Message string `json:"message"`
		} `json:"errors"`
	}
	if jerr := json.Unmarshal(out, &doc); jerr != nil || len(doc.Data) == 0 || string(doc.Data) == "null" {
		msgs := make([]string, len(doc.Errors))
		for i, e := range doc.Errors {
			msgs[i] = e.Message
		}
		return cmp.Or(err, errors.New("gh api graphql: "+cmp.Or(strings.Join(msgs, "; "), "no data")))
	}
	return json.Unmarshal(doc.Data, v)
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
						ID      string `json:"id"`
						Name    string `json:"name"`
						Options []struct{ ID, Name string }
					} `json:"nodes"`
				} `json:"fields"`
			} `json:"projectV2"`
		} `json:"repositoryOwner"`
	}
	err = c.graphql(ctx, &r, `query($o:String!,$n:Int!){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){id
fields(first:50){nodes{...on ProjectV2SingleSelectField{id name options{id name}}}}}}}}`,
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
			m.Fields[f.Name] = field{ID: f.ID, Options: f.Options}
		}
	}
	if _, ok := m.Fields[StatusField]; !ok {
		return nil, fmt.Errorf("board %s/%d has no %s field", owner, c.Board.Project, StatusField)
	}
	return m, nil
}

// filter is the board query of the order's items: open, the team's, and
// in the statuses its steps name when every board step names some.
func filter(b config.Board, order []config.BoardStep) string {
	parts := []string{"is:open"}
	if b.Team != "" {
		parts = append(parts, fmt.Sprintf("%s:%q", strings.ToLower(TeamField), b.Team))
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
	if len(statuses) > 0 {
		q := make([]string, len(statuses))
		for i, s := range statuses {
			q[i] = strconv.Quote(s)
		}
		parts = append(parts, strings.ToLower(StatusField)+":"+strings.Join(q, ","))
	}
	return strings.Join(parts, " ")
}

// Read reads the board items the order can offer, the open sub-issues of
// the items a SubIssues step matches and the search steps' issues: one
// request for the fields, one per 100 items, one for the rest.
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
	snap := &Snapshot{Order: order, Statuses: m.Fields[StatusField].names(), Search: map[int][]Item{}}
	if snap.Items, err = c.items(ctx, filter(c.Board, order)); err != nil {
		return nil, err
	}
	return snap, c.extras(ctx, snap, m.ID, now)
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
content{...I}}}}}}}`+issueFields, vs)
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

// extras reads in one request the open sub-issues of the items with any a
// SubIssues step matches, with their board fields when they are items of
// the board project, whatever their Team or Status, and the search steps'
// issues.
func (c *Client) extras(ctx context.Context, snap *Snapshot, project string, now time.Time) error {
	var q strings.Builder
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
				fmt.Fprintf(&q, "e%d:repository(owner:%s,name:%s){issue(number:%d){subIssues(first:50){nodes{...I projectItems(first:20){nodes{project{id} "+boardFields+"}}}}}}", i, quote(owner), quote(repo), n)
			}
		}
	}
	for i, st := range snap.Order {
		if st.Search != "" {
			fmt.Fprintf(&q, "s%d:search(query:%s,type:ISSUE,first:50){nodes{...I}}", i, quote(searchQuery(st.Search)))
		}
	}
	if q.Len() == 0 {
		return nil
	}
	var r map[string]json.RawMessage
	if err := c.graphql(ctx, &r, "query{"+q.String()+"}"+issueFields, nil); err != nil {
		return err
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
		for _, n := range e.Issue.SubIssues.Nodes {
			if !n.open() {
				continue
			}
			it := n.item()
			if j := slices.IndexFunc(n.ProjectItems.Nodes, func(p projectItem) bool { return p.Project.ID == project }); j >= 0 {
				n.ProjectItems.Nodes[j].onto(&it)
			}
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

// Moved is a move of an item to a status.
type Moved struct {
	Ref  string `json:"ref"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// Move sets the Status of the board item of the issue ref to the status in
// stands for (see Resolve). An unknown or ambiguous status is refused with
// the board's values before anything is written.
func (c *Client) Move(ctx context.Context, ref, in string) (*Moved, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	sf := m.Fields[StatusField]
	to, err := Resolve(StatusField, sf.names(), in)
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	var r struct {
		Repository struct {
			Issue *struct {
				ProjectItems struct {
					Nodes []projectItem `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	err = c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){projectItems(first:50){nodes{
id project{id} `+boardFields+`}}}}}`,
		vars{"o": owner, "r": repo, "n": n})
	if err != nil {
		return nil, err
	}
	mv := &Moved{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n), To: to}
	if r.Repository.Issue == nil {
		return nil, &Refusal{mv.Ref + " is no issue"}
	}
	i := slices.IndexFunc(r.Repository.Issue.ProjectItems.Nodes, func(x projectItem) bool { return x.Project.ID == m.ID })
	if i < 0 {
		return nil, &Refusal{fmt.Sprintf("%s is not on the board %s/%d", mv.Ref, c.Board.Owner, c.Board.Project)}
	}
	item := r.Repository.Issue.ProjectItems.Nodes[i]
	if item.Status != nil {
		mv.From = item.Status.Name
	}
	if mv.From == to {
		return mv, nil
	}
	opt := sf.Options[slices.IndexFunc(sf.Options, func(o struct{ ID, Name string }) bool { return o.Name == to })].ID
	var done json.RawMessage
	err = c.graphql(ctx, &done, `mutation($p:ID!,$i:ID!,$f:ID!,$v:String!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{singleSelectOptionId:$v}}){projectV2Item{id}}}`,
		vars{"p": m.ID, "i": item.ID, "f": sf.ID, "v": opt})
	if err != nil {
		return nil, err
	}
	return mv, nil
}

// projectItem is an issue's item on a board.
type projectItem struct {
	ID      string                 `json:"id"`
	Project struct{ ID string }    `json:"project"`
	Status  *struct{ Name string } `json:"status"`
	Kind    *struct{ Name string } `json:"kind"`
}

// onto marks it a board item with the board fields of p.
func (p projectItem) onto(it *Item) {
	it.OnBoard = true
	if p.Status != nil {
		it.Status = p.Status.Name
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
