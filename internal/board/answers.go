package board

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// ancestorsMax is how many parents up an answer's issue its private thread
// is looked for: the epic above an epic above a slice, and some.
const ancestorsMax = 5

// Thread is where an answer about an issue or pull request is recorded:
// Ref itself in a private repository; else the first private plan pull
// request naming it or an epic above it, then the nearest epic above it in
// a private repository. Via says why Ref is not About, "" when it is.
type Thread struct {
	About string `json:"about"`
	Ref   string `json:"thread"`
	Via   string `json:"via,omitempty"`
	id    string
}

// threadNode is an issue or pull request with its repository's visibility and,
// for an issue, its parent.
type threadNode struct {
	Typename   string `json:"__typename"`
	ID         string `json:"id"`
	Number     int    `json:"number"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
		IsPrivate     bool   `json:"isPrivate"`
	} `json:"repository"`
	Parent *threadNode `json:"parent"`
}

func (n *threadNode) ref() string { return fmt.Sprintf("%s#%d", n.Repository.NameWithOwner, n.Number) }

// threadQuery reads an issue or pull request with ancestorsMax parents.
var threadQuery = func() string {
	const fields = `__typename id number repository{nameWithOwner isPrivate}`
	parents := strings.Repeat(" parent{"+fields, ancestorsMax) + strings.Repeat("}", ancestorsMax)
	return `query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){issueOrPullRequest(number:$n){...on Issue{` + fields + parents + `} ...on PullRequest{` + fields + `}}}}`
}()

// AnswerThread is the thread an answer about ref is recorded on: one read of
// ref and its parents acting on ref's repository, and one search for the
// plans naming it or its parents when its repository is public. A ref that
// does not exist, or a public one with no private thread, is refused.
func (c *Client) AnswerThread(ctx context.Context, ref string) (*Thread, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	var r struct {
		Repository *struct {
			Item *threadNode `json:"issueOrPullRequest"`
		} `json:"repository"`
	}
	err = c.graphql(github.ActingOn(ctx, owner+"/"+repo), &r, threadQuery, vars{"o": owner, "r": repo, "n": n})
	p, ok := partialOf(err)
	if !ok {
		return nil, err
	}
	if r.Repository == nil || r.Repository.Item == nil || r.Repository.Item.ID == "" {
		return nil, &Refusal{fmt.Sprintf("%s is no issue or pull request GitHub resolves%s: nothing posted", ref, p.reason())}
	}
	it := r.Repository.Item
	t := &Thread{About: it.ref()}
	if it.Repository.IsPrivate {
		t.Ref, t.id = t.About, it.ID
		return t, nil
	}
	var chain []*threadNode
	for p := it; p != nil && p.ID != ""; p = p.Parent {
		if p.Typename != "PullRequest" {
			chain = append(chain, p)
		}
	}
	refs := make([]string, len(chain))
	for i, p := range chain {
		refs[i] = p.ref()
	}
	plans, err := c.FindPlans(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, p := range chain {
		if pl := plans[strings.ToLower(p.ref())]; pl != nil && pl.Private {
			t.Ref, t.id, t.Via = pl.Ref, pl.id, "the plan of "+p.ref()
			return t, nil
		}
	}
	for _, p := range chain[min(1, len(chain)):] {
		if p.Repository.IsPrivate {
			t.Ref, t.id, t.Via = p.ref(), p.ID, "an epic above "+t.About
			return t, nil
		}
	}
	return nil, &Refusal{fmt.Sprintf("%s is public and has no private thread for the answer: add a plan pull request in a private plans repository whose description's Epic line names it or an epic above it, or make it a sub-issue of an epic in a private repository; nothing posted", t.About)}
}

// Comment posts body on the thread, acting on its repository, and returns
// the comment's URL.
func (c *Client) Comment(ctx context.Context, t *Thread, body string) (string, error) {
	return c.comment(ctx, t.Ref, t.id, body)
}

// comment posts body on the issue or pull request ref of node id, acting on
// ref's repository, and returns the comment's URL.
func (c *Client) comment(ctx context.Context, ref, id, body string) (string, error) {
	owner, repo, _, err := Ref(ref)
	if err != nil {
		return "", err
	}
	var r struct {
		Add *struct {
			Edge struct {
				Node struct {
					URL string `json:"url"`
				} `json:"node"`
			} `json:"commentEdge"`
		} `json:"addComment"`
	}
	err = c.graphql(github.ActingOn(ctx, owner+"/"+repo), &r, `mutation($s:ID!,$b:String!){addComment(input:{subjectId:$s,body:$b}){commentEdge{node{url}}}}`,
		vars{"s": id, "b": body})
	p, ok := partialOf(err)
	if !ok {
		return "", err
	}
	// The URL is all the answer is read for: with it the comment is posted.
	if r.Add == nil || r.Add.Edge.Node.URL == "" {
		return "", fmt.Errorf("commenting on %s%s", ref, p.reason())
	}
	return r.Add.Edge.Node.URL, nil
}
