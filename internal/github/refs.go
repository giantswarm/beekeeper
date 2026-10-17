package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
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

// One RefStates request asks at most refsBatch references of one owner.
// A try of it may take refsTry and refsTryPerRef for each of its
// references (TryTimeout): a batch of 20 answers in about a second, a
// loaded machine or a slow GitHub takes several. refsRead bounds a
// batch's tries and the waits between them, so a parked set of any size
// is read batch by batch, none cut short by the others.
const (
	refsBatch     = 20
	refsTry       = 10 * time.Second
	refsTryPerRef = 100 * time.Millisecond
	refsRead      = time.Minute
)

// RefStates reads, in GraphQL requests of at most refsBatch references of
// one repository owner (ByOwner), the state of each issue or pull request
// of refs, and for a closed issue the pull request or commit whose closing
// keyword closed it. A reference GitHub does not answer is missing from
// the map. Each try of a request is bounded by its batch's size, so run
// (Retrying) asks a killed one again. A batch that still fails leaves its
// references out and is named in the error, which joins every failed
// batch's; the other batches' states are returned with it.
func RefStates(ctx context.Context, run GH, refs []PR) (map[PR]RefState, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	states := map[PR]RefState{}
	var errs []error
	for _, group := range ByOwner(refs, func(r PR) string { return r.Repo }) {
		batches := slices.Collect(slices.Chunk(group, refsBatch))
		for i, batch := range batches {
			if ctx.Err() != nil {
				errs = append(errs, fmt.Errorf("%s: %w", batchName(batch, i, len(batches)), ctx.Err()))
				continue
			}
			if err := refBatch(ctx, run, batch, states); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", batchName(batch, i, len(batches)), err))
			}
		}
	}
	return states, errors.Join(errs...)
}

// refBatch is one batch of RefStates, its tries bounded by refsRead.
func refBatch(ctx context.Context, run GH, refs []PR, states map[PR]RefState) error {
	ctx, cancel := context.WithTimeout(ctx, refsRead)
	defer cancel()
	ctx = TryTimeout(ActingOn(ctx, refs[0].Repo), refsTry+time.Duration(len(refs))*refsTryPerRef)
	return refStates(ctx, run, refs, states)
}

// batchName names the i-th of n batches of one owner for an error: "refs
// batch 2/3 of giantswarm (20: giantswarm/a#1 … giantswarm/b#9)".
func batchName(refs []PR, i, n int) string {
	owner, _, _ := strings.Cut(refs[0].Repo, "/")
	first, last := refs[0], refs[len(refs)-1]
	return fmt.Sprintf("refs batch %d/%d of %s (%d: %s#%d … %s#%d)", i+1, n, owner, len(refs), first.Repo, first.N, last.Repo, last.N)
}

// refStates is one request of RefStates, of refs of one owner, into states.
func refStates(ctx context.Context, run GH, refs []PR, states map[PR]RefState) error {
	var q strings.Builder
	q.WriteString("query{")
	for i, r := range refs {
		owner, name, ok := strings.Cut(r.Repo, "/")
		if !ok {
			return fmt.Errorf("%s is not owner/repo", r.Repo)
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
		return cmp.Or(err, jerr, fmt.Errorf("gh api graphql: no data"))
	}
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
	return nil
}
