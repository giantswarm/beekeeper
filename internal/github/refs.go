package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RefStates reads in one GraphQL request the state of each issue or pull
// request of refs: OPEN, CLOSED or MERGED. A reference GitHub does not
// answer is missing from the map.
func RefStates(ctx context.Context, run GH, refs []PR) (map[PR]string, error) {
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
		fmt.Fprintf(&q, "r%d:repository(owner:%s,name:%s){issueOrPullRequest(number:%d){...on Issue{state}...on PullRequest{state}}}", i, quote(owner), quote(name), r.N)
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
	states := map[PR]string{}
	for i, r := range refs {
		var v struct {
			Item *struct {
				State string `json:"state"`
			} `json:"issueOrPullRequest"`
		}
		if raw, ok := doc.Data[fmt.Sprintf("r%d", i)]; ok && json.Unmarshal(raw, &v) == nil && v.Item != nil && v.Item.State != "" {
			states[r] = v.Item.State
		}
	}
	return states, nil
}
