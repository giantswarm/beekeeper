package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RefState is the state of an issue or pull request as GitHub answered:
// OPEN, CLOSED or MERGED, and for an issue a closing keyword closed, what
// closed it (Closer): the pull request whose merge did, as owner/repo#n,
// or a bare commit as "commit <sha>". Empty for a close by hand.
type RefState struct {
	State  string
	Closer string
}

// closedBy reads an issue's latest closed event's closer: the pull
// request whose closing keyword closed it, directly or through its merge
// commit.
const closedBy = "timelineItems(last:1,itemTypes:[CLOSED_EVENT]){nodes{...on ClosedEvent{closer{__typename ...on PullRequest{number repository{nameWithOwner}}...on Commit{abbreviatedOid associatedPullRequests(first:1){nodes{number repository{nameWithOwner}}}}}}}}"

// closer is a closed event's closer as GitHub answers it.
type closer struct {
	Type       string `json:"__typename"`
	Number     int    `json:"number"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Oid        string `json:"abbreviatedOid"`
	Associated struct {
		Nodes []struct {
			Number     int `json:"number"`
			Repository struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		} `json:"nodes"`
	} `json:"associatedPullRequests"`
}

// ref is the closer as owner/repo#n (a pull request, or the one a commit
// belongs to) or "commit <sha>"; "" for none.
func (c *closer) ref() string {
	switch {
	case c == nil:
		return ""
	case c.Type == "PullRequest":
		return fmt.Sprintf("%s#%d", c.Repository.NameWithOwner, c.Number)
	case len(c.Associated.Nodes) > 0:
		n := c.Associated.Nodes[0]
		return fmt.Sprintf("%s#%d", n.Repository.NameWithOwner, n.Number)
	default:
		return "commit " + c.Oid
	}
}

// RefStates reads in one GraphQL request the state of each issue or pull
// request of refs, and for a closed issue the pull request or commit whose
// closing keyword closed it. A reference GitHub does not answer is missing
// from the map.
func RefStates(ctx context.Context, run GH, refs []PR) (map[PR]RefState, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var q strings.Builder
	q.WriteString("query{")
	for i, r := range refs {
		owner, name, ok := strings.Cut(r.Repo, "/")
		if !ok {
			return nil, fmt.Errorf("%s is not owner/repo", r.Repo)
		}
		fmt.Fprintf(&q, "r%d:repository(owner:%s,name:%s){issueOrPullRequest(number:%d){...on Issue{state %s}...on PullRequest{state}}}", i, quote(owner), quote(name), r.N, closedBy)
	}
	q.WriteString("}")
	// A reference GitHub does not answer fails the request but keeps the
	// others' data.
	out, err := run(ctx, "api", "graphql", "-f", "query="+q.String())
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(out, &doc); doc.Data == nil {
		return nil, cmp.Or(err, jerr, fmt.Errorf("gh api graphql: no data"))
	}
	states := map[PR]RefState{}
	for i, r := range refs {
		var v struct {
			Item *struct {
				State    string `json:"state"`
				Timeline struct {
					Nodes []struct {
						Closer *closer `json:"closer"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"issueOrPullRequest"`
		}
		raw, ok := doc.Data[fmt.Sprintf("r%d", i)]
		if !ok || json.Unmarshal(raw, &v) != nil || v.Item == nil || v.Item.State == "" {
			continue
		}
		s := RefState{State: v.Item.State}
		// A reopened issue's past close no longer closes it.
		if s.State == Closed && len(v.Item.Timeline.Nodes) > 0 {
			s.Closer = v.Item.Timeline.Nodes[0].Closer.ref()
		}
		states[r] = s
	}
	return states, nil
}
