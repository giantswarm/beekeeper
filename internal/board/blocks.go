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
// for an issue its node id, state and recorded blockers. The blockers are
// read as the list, which a mutation's answer and the next read show at
// once; the issue's issueDependenciesSummary follows them seconds later.
type depNode struct {
	Type      string `json:"__typename"`
	ID        string `json:"id"`
	State     string `json:"state"`
	BlockedBy struct {
		TotalCount int      `json:"totalCount"`
		Nodes      []depRef `json:"nodes"`
	} `json:"blockedBy"`
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
	if why := notIssue(it, h.Ref, "a hold is recorded on an issue"); why != "" {
		return nil, &Refusal{why}
	}
	blocker, err := c.depNode(github.ActingOn(ctx, bo+"/"+br), bo, br, bn, "")
	if err != nil {
		return nil, err
	}
	if why := notIssue(blocker, h.By, "a blocker is an issue; a hold on a review or a person is the item's Status Blocked / Waiting plus a decision for that person"); why != "" {
		return nil, &Refusal{why}
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
	err := c.graphql(ctx, &r, `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issueOrPullRequest(number:$n){__typename ...on Issue{`+depFields+` `+extra+`}}}}`,
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

// notIssue says why x, read for ref, is no issue a hold takes: a pull
// request (with rule, what a hold takes instead) or nothing GitHub resolves;
// empty for an issue.
func notIssue(x resolved, ref, rule string) string {
	switch {
	case x.node == nil:
		return ref + " is no issue" + x.partial.reason()
	case x.node.Type == "PullRequest":
		return ref + " is a pull request: " + rule
	case x.node.Type != "Issue" || x.node.ID == "":
		return ref + " is no issue"
	}
	return ""
}
