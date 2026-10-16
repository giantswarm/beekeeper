package board

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

// PriorityField is the board's number field that holds the priority list:
// an epic's rank, 1 the top. Only the person sets it (beekeeper priority set
// --as <person>); board next applies it.
const PriorityField = "Priority"

// numberType is the data type of a number field.
const numberType = "NUMBER"

// RankedEpic is an epic of the priority list.
type RankedEpic struct {
	Rank   int    `json:"rank"`
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Status string `json:"status,omitempty"`
	// Team is its Team; RankBy and TeamBy the logins that wrote its rank
	// and Team (the field values' creator), RankAt and TeamAt when.
	Team   string    `json:"team,omitempty"`
	RankBy string    `json:"rankBy,omitempty"`
	RankAt time.Time `json:"rankAt,omitzero"`
	TeamBy string    `json:"teamBy,omitempty"`
	TeamAt time.Time `json:"teamAt,omitzero"`
	// Held is its hold (Item.HeldText): its open blockers, "" for none. A
	// held epic keeps its rank; board next skips it and every issue under it.
	Held string `json:"held,omitempty"`
	// item is its item's id on the board.
	item string
}

// Priorities is the priority list, by rank.
type Priorities []RankedEpic

// Ranks is the priority list as each epic's rank by its lower-cased ref.
func (p Priorities) Ranks() Ranks {
	out := Ranks{}
	for _, e := range p {
		out[strings.ToLower(e.Ref)] = e.Rank
	}
	return out
}

// Ranks are the epics' ranks by their lower-cased ref.
type Ranks map[string]int

// maxDepth bounds the walk up the sub-issue relation.
const maxDepth = 10

// Of is the rank ref has, its own or inherited from its nearest ranked
// ancestor through the native sub-issue relation (parent says an issue's
// parent, "" when unknown), and the epic it is the rank of; 0 and "" for an
// issue without a ranked ancestor.
func (r Ranks) Of(ref string, parent func(string) string) (int, string) {
	for range maxDepth {
		if ref == "" {
			break
		}
		if n := r[strings.ToLower(ref)]; n > 0 {
			return n, ref
		}
		ref = parent(ref)
	}
	return 0, ""
}

// Parent is the parent of the issue ref among the items snap read, the
// board items and the sub-issues read with them; "" when snap read none.
func (snap *Snapshot) Parent(ref string) string {
	for _, it := range snap.Items {
		if strings.EqualFold(it.Ref, ref) {
			return it.Parent
		}
		for _, sub := range it.SubIssues {
			if strings.EqualFold(sub.Ref, ref) {
				return cmp.Or(sub.Parent, it.Ref)
			}
		}
	}
	return ""
}

// ByPriority orders cands by the rank each has (Ranks.Of, from its own
// parent and upward through parent) first, unranked after ranked, and keeps
// their order within a rank: the priority list over the configured order.
// It records the rank applied on each candidate.
func ByPriority(cands []Candidate, r Ranks, parent func(string) string) {
	for i := range cands {
		c := &cands[i]
		up := func(ref string) string {
			if strings.EqualFold(ref, c.Ref) {
				return cmp.Or(c.Parent, c.Epic)
			}
			return parent(ref)
		}
		c.Priority, c.PriorityOf = r.Of(c.Ref, up)
	}
	slices.SortStableFunc(cands, func(a, b Candidate) int {
		return cmp.Compare(rankKey(a.Priority), rankKey(b.Priority))
	})
}

// rankKey sorts the unranked (0) after every rank.
func rankKey(n int) int {
	if n <= 0 {
		return int(^uint(0) >> 1)
	}
	return n
}

// priorityFields is what a read of the priority list takes of an item: its
// rank and Team, each with who wrote it and when.
const priorityFields = `id ` + boardFields + `
priority:fieldValueByName(name:"` + PriorityField + `"){...on ProjectV2ItemFieldNumberValue{number updatedAt creator{login}}}
team:fieldValueByName(name:"` + TeamField + `"){...on ProjectV2ItemFieldSingleSelectValue{name updatedAt creator{login}}}`

// writtenBy is who wrote a field value and when.
type writtenBy struct {
	UpdatedAt time.Time               `json:"updatedAt"`
	Creator   *struct{ Login string } `json:"creator"`
}

func (w writtenBy) login() string {
	if w.Creator == nil {
		return ""
	}
	return w.Creator.Login
}

// rankedNode is an item as a read of the priority list takes it; its Team
// with the writer shadows projectItem's.
type rankedNode struct {
	projectItem
	Priority *struct {
		Number float64 `json:"number"`
		writtenBy
	} `json:"priority"`
	Team *struct {
		Name string `json:"name"`
		writtenBy
	} `json:"team"`
}

// onto fills e's rank and Team with their writers.
func (n rankedNode) onto(e *RankedEpic) {
	if n.Priority != nil {
		e.Rank, e.RankBy, e.RankAt = int(n.Priority.Number), n.Priority.login(), n.Priority.UpdatedAt
	}
	if n.Team != nil {
		e.Team, e.TeamBy, e.TeamAt = n.Team.Name, n.Team.login(), n.Team.UpdatedAt
	}
}

// priorityFilters are the board queries of the epics the priority list can
// hold: open, of the epic kinds, the team's by its Team field and, apart,
// by its label (TeamLabel), which a label sync that moves the Team leaves
// on the epic. A board without board.team has one query of every Team.
func (c *Client) priorityFilters(kinds []string) []string {
	kind := ""
	if len(kinds) > 0 {
		q := make([]string, len(kinds))
		for i, k := range kinds {
			q[i] = fmt.Sprintf("%q", k)
		}
		kind = " " + strings.ToLower(KindField) + ":" + strings.Join(q, ",")
	}
	if c.Board.Team == "" {
		return []string{isOpen + kind}
	}
	out := []string{fmt.Sprintf("%s %s:%q%s", isOpen, strings.ToLower(TeamField), c.Board.Team, kind)}
	if l := c.TeamLabel(); l != "" {
		out = append(out, fmt.Sprintf("%s label:%q%s", isOpen, l, kind))
	}
	return out
}

// TeamLabel is board.teamLabel, else team/ and board.team's first word
// lower-cased; "" without board.team.
func (c *Client) TeamLabel() string {
	if c.Board.TeamLabel != "" || c.Board.Team == "" {
		return c.Board.TeamLabel
	}
	w, _, _ := strings.Cut(words(c.Board.Team), " ")
	if w == "" {
		return ""
	}
	return "team/" + w
}

// Priorities reads the priority list: the open epics (the epic gate's
// kinds) of board.team, by their Team field or their team label, whose
// Priority field holds a rank, by rank and, within
// one, in the board's order. A board without a Priority field has an empty
// list. It is the one read every consumer of the priorities calls (board
// next; lease, lanes and capacity next), with Ranks and Ranks.Of on top.
func (c *Client) Priorities(ctx context.Context) (Priorities, error) {
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	return c.priorities(ctx, m)
}

func (c *Client) priorities(ctx context.Context, m *meta) (Priorities, error) {
	if _, ok := m.Fields[PriorityField]; !ok {
		return nil, nil
	}
	gate, err := ResolveGate(c.Board.Epics, m.Fields[StatusField].names(), m.Fields[KindField].names())
	if err != nil {
		return nil, err
	}
	var out Priorities
	seen := map[string]bool{}
	for _, q := range c.priorityFilters(gate.Kind) {
		if err := c.rankedPages(ctx, q, seen, &out); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(out, func(a, b RankedEpic) int { return cmp.Compare(a.Rank, b.Rank) })
	return out, nil
}

// rankedPages appends the ranked open epics of the board query q to out,
// each item once (seen by its id).
func (c *Client) rankedPages(ctx context.Context, q string, seen map[string]bool, out *Priorities) error {
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
							rankedNode
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
items(first:100,after:$c,query:$q){pageInfo{hasNextPage endCursor} nodes{`+priorityFields+`
content{...I `+blockerFields+`}}}}}}}`+issueFields, vs)
		if err != nil {
			return fmt.Errorf("the priority list: %w", err)
		}
		items := r.Owner.Project.Items
		for _, n := range items.Nodes {
			if !n.Content.open() || n.Priority == nil || n.Priority.Number < 1 || seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			it := n.Content.item()
			n.projectItem.onto(&it)
			e := RankedEpic{Ref: it.Ref, URL: it.URL, Title: it.Title, Status: it.Status, Held: it.HeldText(), item: n.ID}
			n.rankedNode.onto(&e)
			*out = append(*out, e)
		}
		if !items.PageInfo.HasNextPage {
			return nil
		}
		cursor = items.PageInfo.EndCursor
	}
}

// Prioritized is a change of the priority list: the epic, its rank before
// and after (0 for none), and the list as it is now.
type Prioritized struct {
	Ref  string     `json:"ref"`
	From int        `json:"from,omitempty"`
	To   int        `json:"to,omitempty"`
	List Priorities `json:"list"`
	// Added says the epic had no item on the board, Team the Team it was
	// given; as in Moved.
	Added bool   `json:"added,omitempty"`
	Team  string `json:"team,omitempty"`
}

// SetPriority puts the epic ref at rank in the priority list, 1 the top:
// the epics at and below the rank move down one, and the list stays ranked
// 1..n (a rank past its end is its end). rank 0 takes the epic off the
// list, the epics below it moving up one. Only the items whose rank changes
// are written, each by its id; the list is read with Priorities, the epic's
// item from its projectItems (added to the board as Move adds one). The
// caller is the one to check that the person asked for it. Refused before
// anything is written: a board without a number field Priority, an issue
// that does not exist, is closed or is no epic of the epic gate's kinds.
func (c *Client) SetPriority(ctx context.Context, ref string, rank int) (*Prioritized, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	if rank < 0 {
		return nil, &Refusal{fmt.Sprintf("rank %d: a rank is 1 or more, 1 the top", rank)}
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	pf, ok := m.Fields[PriorityField]
	if !ok || pf.DataType != numberType {
		return nil, &Refusal{fmt.Sprintf("the board %s/%d has no number field %s: gh project field-create %d --owner %s --name %s --data-type NUMBER",
			c.Board.Owner, c.Board.Project, PriorityField, c.Board.Project, c.Board.Owner, PriorityField)}
	}
	gate, err := ResolveGate(c.Board.Epics, m.Fields[StatusField].names(), m.Fields[KindField].names())
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	list, err := c.priorities(ctx, m)
	if err != nil {
		return nil, err
	}
	p := &Prioritized{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n), To: rank}
	at := slices.IndexFunc(list, func(e RankedEpic) bool { return strings.EqualFold(e.Ref, p.Ref) })
	var epic RankedEpic
	if at >= 0 {
		epic, p.From = list[at], list[at].Rank
		list = slices.Delete(list, at, at+1)
	}
	if rank > 0 && at < 0 {
		if epic, err = c.epicItem(ctx, m, gate, p, owner, repo, n); err != nil {
			return nil, err
		}
	}
	// The ranks before the change, by item: what is written is what differs.
	was := map[string]int{}
	for _, e := range list {
		was[e.item] = e.Rank
	}
	if rank > 0 {
		pos := min(rank-1, len(list))
		list = slices.Insert(list, pos, epic)
		p.To = pos + 1
	}
	for i := range list {
		list[i].Rank = i + 1
	}
	if rank == 0 && at >= 0 {
		if err := c.clear(ctx, m.ID, epic.item, pf.ID); err != nil {
			return nil, err
		}
	}
	for _, e := range list {
		if r, ok := was[e.item]; ok && r == e.Rank {
			continue
		}
		if e.item == epic.item && p.From == e.Rank {
			continue
		}
		if err := c.setNumber(ctx, m.ID, e.item, pf.ID, e.Rank); err != nil {
			return nil, fmt.Errorf("ranking %s %d: %w", e.Ref, e.Rank, err)
		}
	}
	p.List = list
	return p, nil
}

// epicItem is the item of the epic owner/repo#n on the board, added as Move
// adds one; an issue that is closed or no epic of the gate's kinds is
// refused.
func (c *Client) epicItem(ctx context.Context, m *meta, gate Gate, p *Prioritized, owner, repo string, n int) (RankedEpic, error) {
	var r struct {
		Repository struct {
			Issue *issueJSON `json:"issue"`
		} `json:"repository"`
	}
	err := c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){...I}}}`+issueFields,
		vars{"o": owner, "r": repo, "n": n})
	pe, ok := partialOf(err)
	if !ok {
		return RankedEpic{}, err
	}
	if r.Repository.Issue == nil {
		return RankedEpic{}, &Refusal{p.Ref + " is no issue" + pe.reason()}
	}
	if pe != nil {
		return RankedEpic{}, err
	}
	if !r.Repository.Issue.open() {
		return RankedEpic{}, &Refusal{p.Ref + " is closed: the priority list ranks open epics"}
	}
	mv := &Moved{Ref: p.Ref}
	item, err := c.item(ctx, m, mv, owner, repo, n)
	if err != nil {
		return RankedEpic{}, err
	}
	var it Item
	item.onto(&it)
	if len(gate.Kind) > 0 && !gate.Epic(it) {
		return RankedEpic{}, &Refusal{fmt.Sprintf("%s is %s, no epic (%s): the priority list ranks epics", p.Ref, cmp.Or(it.Kind, "without a Kind"), strings.Join(gate.Kind, ", "))}
	}
	team, err := c.team(m)
	if err != nil {
		return RankedEpic{}, &Refusal{err.Error()}
	}
	p.Added = mv.Added
	if item.Team == nil && team != nil {
		if err := c.set(ctx, m.ID, item.ID, team.field, team.id); err != nil {
			return RankedEpic{}, err
		}
		p.Team = team.name
	}
	e := RankedEpic{Ref: p.Ref, URL: r.Repository.Issue.URL, Title: r.Repository.Issue.Title, Status: it.Status, Team: p.Team, item: item.ID}
	if item.Team != nil {
		e.Team = item.Team.Name
	}
	return e, nil
}

// setNumber sets the number field f of the item of the project to v.
func (c *Client) setNumber(ctx context.Context, project, item, f string, v int) error {
	var done json.RawMessage
	return c.graphql(ctx, &done, `mutation($p:ID!,$i:ID!,$f:ID!,$v:Float!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{number:$v}}){projectV2Item{id}}}`,
		vars{"p": project, "i": item, "f": f, "v": v})
}

// clear empties the field f of the item of the project.
func (c *Client) clear(ctx context.Context, project, item, f string) error {
	var done json.RawMessage
	return c.graphql(ctx, &done, `mutation($p:ID!,$i:ID!,$f:ID!){clearProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f}){projectV2Item{id}}}`,
		vars{"p": project, "i": item, "f": f})
}

// RankedNow is the epic ref as its own item on the board says now, read
// from the issue's projectItems, never the board's: its rank and Team with
// their writers. nil for an issue that is closed or has no item on the
// board.
func (c *Client) RankedNow(ctx context.Context, ref string) (*RankedEpic, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	var r struct {
		Repository struct {
			Issue *struct {
				issueJSON
				ProjectItems struct {
					Nodes []rankedNode `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	}
	err = c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issue(number:$n){...I `+blockerFields+` projectItems(first:50){nodes{project{id} `+priorityFields+`}}}}}`+issueFields,
		vars{"o": owner, "r": repo, "n": n})
	if err != nil {
		return nil, fmt.Errorf("the board item of %s: %w", ref, err)
	}
	is := r.Repository.Issue
	if is == nil || !is.open() {
		return nil, nil
	}
	i := slices.IndexFunc(is.ProjectItems.Nodes, func(x rankedNode) bool { return x.Project.ID == m.ID })
	if i < 0 {
		return nil, nil
	}
	it := is.item()
	node := is.ProjectItems.Nodes[i]
	node.projectItem.onto(&it)
	e := &RankedEpic{Ref: it.Ref, URL: it.URL, Title: it.Title, Status: it.Status, Held: it.HeldText(), item: node.ID}
	node.onto(e)
	return e, nil
}
