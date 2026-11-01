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

// Thread is an issue or pull request as a note's thread: its state (OPEN,
// CLOSED or MERGED) and, for an issue that is a sub-issue, its parent
// issue (owner/repo#n) and that one's state.
type Thread struct {
	State       string
	Parent      string
	ParentState string
}

// threadQuery reads one issue or pull request's state, and an issue's
// parent and its state.
const threadQuery = "r%d:repository(owner:%s,name:%s){issueOrPullRequest(number:%d){...on Issue{state parent{number state repository{nameWithOwner}}}...on PullRequest{state}}}"

// Threads reads, in one GraphQL request per repository owner and batch of
// refsBatch, the state of each issue or pull request of refs and the parent
// of each issue that has one. A reference GitHub does not answer is missing
// from the map; a request that fails is named in the error, the others'
// threads returned with it.
func Threads(ctx context.Context, run GH, refs []PR) (map[PR]Thread, error) {
	out := map[PR]Thread{}
	var errs []error
	for _, group := range ByOwner(refs, func(r PR) string { return r.Repo }) {
		for batch := range slices.Chunk(group, refsBatch) {
			if err := threadBatch(ctx, run, batch, out); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return out, errors.Join(errs...)
}

// threadBatch is one request of Threads, of refs of one owner, into out.
func threadBatch(ctx context.Context, run GH, refs []PR, out map[PR]Thread) error {
	ctx, cancel := context.WithTimeout(ctx, refsRead)
	defer cancel()
	ctx = TryTimeout(ActingOn(ctx, refs[0].Repo), refsTry+time.Duration(len(refs))*refsTryPerRef)
	q := "query{"
	for i, r := range refs {
		owner, name, ok := strings.Cut(r.Repo, "/")
		if !ok {
			return fmt.Errorf("%s is not owner/repo", r.Repo)
		}
		q += fmt.Sprintf(threadQuery, i, quote(owner), quote(name), r.N)
	}
	q += "}"
	raw, err := run(ctx, "api", "graphql", "-f", "query="+q)
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &doc); doc.Data == nil {
		return cmp.Or(err, jerr, errors.New("gh api graphql: no data"))
	}
	for i, r := range refs {
		var v struct {
			Item *struct {
				State  string `json:"state"`
				Parent *struct {
					Number     int    `json:"number"`
					State      string `json:"state"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"parent"`
			} `json:"issueOrPullRequest"`
		}
		b, ok := doc.Data[fmt.Sprintf("r%d", i)]
		if !ok || json.Unmarshal(b, &v) != nil || v.Item == nil || v.Item.State == "" {
			continue
		}
		t := Thread{State: v.Item.State}
		if p := v.Item.Parent; p != nil && p.Number > 0 {
			t.Parent, t.ParentState = fmt.Sprintf("%s#%d", p.Repository.NameWithOwner, p.Number), p.State
		}
		out[r] = t
	}
	return err
}
