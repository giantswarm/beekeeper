package board

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Held is a hold recorded on an issue, or removed from it: the issue's
// GitHub blocked-by dependency on another issue, which board next reads.
type Held struct {
	Ref string `json:"ref"`
	By  string `json:"by"`
	// ByOpen says the blocker is open: the hold keeps the item from board
	// next until it closes.
	ByOpen bool `json:"byOpen"`
	// Changed says the dependency was added or removed; false when it was
	// recorded already (block) or not recorded (unblock).
	Changed bool `json:"changed"`
	// Blockers and OpenBlockers are the issue's recorded blockers after the
	// change, all and the open ones.
	Blockers     int `json:"blockers"`
	OpenBlockers int `json:"openBlockers"`
}

// Block records the issue by as a blocker of the issue ref, a GitHub
// blocked-by dependency (addBlockedBy): board next skips ref while one of
// its blockers is open and offers it again once they closed, with no session
// acting. A dependency recorded already is left as it is.
func (c *Client) Block(ctx context.Context, ref, by string) (*Held, error) {
	return c.hold(ctx, ref, by, true)
}

// Unblock removes the issue by from the blockers of the issue ref
// (removeBlockedBy). A dependency not recorded is left as it is.
func (c *Client) Unblock(ctx context.Context, ref, by string) (*Held, error) {
	return c.hold(ctx, ref, by, false)
}

// depNode is an issue or a pull request as a hold reads it: its type, and
// for an issue its node id, state and recorded blockers. A pull request that
// closes an issue resolves to that issue. The blockers are read as the list,
// which a mutation's answer and the next read show at once; the issue's
// issueDependenciesSummary follows them seconds later.
type depNode struct {
	Type       string `json:"__typename"`
	ID         string `json:"id"`
	State      string `json:"state"`
	Number     int    `json:"number"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	BlockedBy struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []depRef `json:"nodes"`
	} `json:"blockedBy"`
	Closing struct {
		Nodes []*depNode `json:"nodes"`
	} `json:"closingIssuesReferences"`
}

// depRef is a recorded blocker of an issue.
type depRef struct {
	Number     int    `json:"number"`
	State      string `json:"state"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// counts are the recorded blockers of x, all and the open ones.
func (x *depNode) counts() (all, open int) {
	for _, b := range x.BlockedBy.Nodes {
		if b.State == stateOpen {
			open++
		}
	}
	return x.BlockedBy.TotalCount, open
}

// depFields is what a hold reads of an issue; blockersField its blockers.
const (
	depFields     = `id state`
	blockersField = `blockedBy(first:100){totalCount nodes{number state repository{nameWithOwner}}}`
)

// hold adds (block) or removes the dependency of ref on by. Each issue is
// read on its own repository (github.ActingOn), the mutation acts on ref's.
// A ref or blocker that is a pull request or no issue GitHub resolves, and
// an issue held by itself, are refused naming why before anything is
// written.
func (c *Client) hold(ctx context.Context, ref, by string, block bool) (*Held, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	bo, br, bn, err := Ref(by)
	if err != nil {
		return nil, err
	}
	h := &Held{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n), By: fmt.Sprintf("%s/%s#%d", bo, br, bn)}
	if strings.EqualFold(h.Ref, h.By) {
		return nil, &Refusal{h.Ref + " cannot be blocked by itself"}
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	it, err := c.depNode(ctx, owner, repo, n, blockersField)
	if err != nil {
		return nil, err
	}
	it, h.Ref = closingIssue(it, h.Ref)
	if why := notIssue(it, h.Ref, "a hold is recorded on an issue"); why != "" {
		return nil, &Refusal{why}
	}
	blocker, err := c.depNode(github.ActingOn(ctx, bo+"/"+br), bo, br, bn, "")
	if err != nil {
		return nil, err
	}
	blocker, h.By = closingIssue(blocker, h.By)
	if why := notIssue(blocker, h.By, "a blocker is an issue; a hold on a review or a person is the item's Status Blocked / Waiting plus a decision for that person"); why != "" {
		return nil, &Refusal{why}
	}
	if strings.EqualFold(h.Ref, h.By) {
		return nil, &Refusal{h.Ref + " cannot be blocked by itself"}
	}
	h.ByOpen = blocker.node.State == stateOpen
	recorded := slices.ContainsFunc(it.node.BlockedBy.Nodes, func(x depRef) bool {
		return strings.EqualFold(fmt.Sprintf("%s#%d", x.Repository.NameWithOwner, x.Number), h.By)
	})
	h.Blockers, h.OpenBlockers = it.node.counts()
	if recorded == block {
		return h, nil
	}
	mutation := "removeBlockedBy"
	if block {
		mutation = "addBlockedBy"
	}
	var r map[string]*struct {
		Issue *depNode `json:"issue"`
	}
	ctx = github.ActingOn(ctx, refRepo(h.Ref))
	err = c.graphql(ctx, &r, `mutation($i:ID!,$b:ID!){`+mutation+`(input:{issueId:$i,blockingIssueId:$b}){issue{`+depFields+` `+blockersField+`}}}`,
		vars{"i": it.node.ID, "b": blocker.node.ID})
	p, ok := partialOf(err)
	if !ok {
		return nil, err
	}
	if r[mutation] == nil || r[mutation].Issue == nil {
		return nil, fmt.Errorf("%s %s on %s%s", mutation, h.By, h.Ref, p.reason())
	}
	if p != nil {
		return nil, fmt.Errorf("%s %s on %s, its blockers unread: %w", mutation, h.By, h.Ref, err)
	}
	h.Changed = true
	h.Blockers, h.OpenBlockers = r[mutation].Issue.counts()
	return h, nil
}

// resolved is an issue or pull request a hold read, with the messages
// GitHub answered beside it.
type resolved struct {
	node    *depNode
	partial *Partial
}

// depNode reads the issue or pull request owner/repo#n, with extra of an
// issue; a node GitHub does not resolve is nil, with its reason.
func (c *Client) depNode(ctx context.Context, owner, repo string, n int, extra string) (resolved, error) {
	var r struct {
		Repository *struct {
			Item *depNode `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	err := c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issueOrPullRequest(number:$n){__typename ...on Issue{`+depFields+` `+extra+`} ...on PullRequest{closingIssuesReferences(first:1){nodes{__typename `+depFields+` number repository{nameWithOwner} `+extra+`}}}}}}`,
		vars{"o": owner, "r": repo, "n": n})
	p, ok := partialOf(err)
	if !ok {
		return resolved{}, err
	}
	out := resolved{partial: p}
	if r.Repository != nil {
		out.node = r.Repository.Item
	}
	if out.node != nil && p != nil {
		// A node GitHub resolved with fields missing is never read whole.
		return resolved{}, fmt.Errorf("%s/%s#%d: %w", owner, repo, n, err)
	}
	return out, nil
}

// closingIssue resolves a pull request to the first issue it closes, when it
// closes one; a pull request that closes none stays a pull request so notIssue
// refuses it.
func closingIssue(x resolved, ref string) (resolved, string) {
	if x.node == nil || x.node.Type != github.RefPullRequest || len(x.node.Closing.Nodes) == 0 || x.node.Closing.Nodes[0] == nil {
		return x, ref
	}
	c := *x.node.Closing.Nodes[0]
	if c.Type == "" {
		c.Type = github.RefIssue
	}
	x.node = &c
	if c.Repository.NameWithOwner != "" && c.Number > 0 {
		ref = fmt.Sprintf("%s#%d", c.Repository.NameWithOwner, c.Number)
	}
	return x, ref
}

// notIssue says why x, read for ref, is no issue a hold takes: a pull
// request (with rule, what a hold takes instead) or nothing GitHub resolves;
// empty for an issue.
func notIssue(x resolved, ref, rule string) string {
	switch {
	case x.node == nil:
		return ref + " is no issue" + x.partial.reason()
	case x.node.Type == github.RefPullRequest:
		return ref + " is a pull request: " + rule
	case x.node.Type != github.RefIssue || x.node.ID == "":
		return ref + " is no issue"
	}
	return ""
}

// MoveHeld is Move guarded for a move to the blocked Status, the one
// HeldStatus resolves to: the board says what holds every blocked item.
// With by such a move records by, an open issue, as the item's blocker first
// (Block); without by it is refused unless the item has an open recorded
// blocker already. A move to any other Status with by is refused, and a
// board without a blocked Status moves as Move does. Every refusal comes
// before anything is written. Held is the item's blockers read or recorded
// for a move to the blocked Status, nil for any other.
func (c *Client) MoveHeld(ctx context.Context, ref, in, by string) (*Moved, *Held, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, nil, err
	}
	m, err := c.meta(github.ActingOn(ctx, owner+"/"+repo))
	if err != nil {
		return nil, nil, err
	}
	names := m.Fields[StatusField].names()
	to, err := Resolve(StatusField, names, in)
	if err != nil {
		return nil, nil, &Refusal{err.Error()}
	}
	if blocked, berr := Resolve(StatusField, names, HeldStatus); berr != nil || to != blocked {
		if by != "" {
			return nil, nil, &Refusal{fmt.Sprintf("--by records what holds an item moved to the blocked Status; %s is none", to)}
		}
		mv, err := c.Move(ctx, ref, in)
		return mv, nil, err
	}
	var h *Held
	if by == "" {
		if h, err = c.blockers(ctx, owner, repo, n); err != nil {
			return nil, nil, err
		}
		if h.OpenBlockers == 0 {
			return nil, h, &Refusal{fmt.Sprintf("%s has no open recorded blocker: a move to %s names what holds it, --by <owner/repo#n> (an open issue, recorded as with board block). "+
				"A wait on a pull request, a review, a hold or the GitHub gate is a park (beekeeper agents park --on <owner/repo#n|hold:<target>>) with the item left in its Status; "+
				"a person's decision that holds it is note add --holds; a defect is a problem report (beekeeper agents problem)", h.Ref, to)}
		}
	} else {
		bo, br, bn, err := Ref(by)
		if err != nil {
			return nil, nil, err
		}
		b, err := c.depNode(github.ActingOn(ctx, bo+"/"+br), bo, br, bn, "")
		if err != nil {
			return nil, nil, err
		}
		b, blockerRef := closingIssue(b, fmt.Sprintf("%s/%s#%d", bo, br, bn))
		if b.node != nil && b.node.Type == github.RefIssue && b.node.State != stateOpen {
			return nil, nil, &Refusal{fmt.Sprintf("%s is closed: it holds nothing, and %s is not blocked by it", blockerRef, ref)}
		}
		if h, err = c.Block(ctx, ref, by); err != nil {
			return nil, nil, err
		}
	}
	mv, err := c.Move(ctx, ref, in)
	return mv, h, err
}

// blockers reads the recorded blockers of the issue owner/repo#n.
func (c *Client) blockers(ctx context.Context, owner, repo string, n int) (*Held, error) {
	h := &Held{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n)}
	it, err := c.depNode(github.ActingOn(ctx, owner+"/"+repo), owner, repo, n, blockersField)
	if err != nil {
		return nil, err
	}
	if why := notIssue(it, h.Ref, "a hold is recorded on an issue"); why != "" {
		return nil, &Refusal{why}
	}
	h.Blockers, h.OpenBlockers = it.node.counts()
	return h, nil
}
