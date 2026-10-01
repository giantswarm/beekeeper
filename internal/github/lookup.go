package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PR is a pull request: its owner/repo and number.
type PR struct {
	Repo string `json:"repo"`
	N    int    `json:"n"`
}

// Lookup reads in one GraphQL request the titles of pulls and the open draft
// pull requests of the repositories drafts (owner/repo), oldest first.
func Lookup(ctx context.Context, run GH, pulls []PR, drafts []string) (map[PR]string, []PR, error) {
	if len(pulls) == 0 && len(drafts) == 0 {
		return nil, nil, nil
	}
	var q strings.Builder
	q.WriteString("query{")
	for i, p := range pulls {
		owner, name, ok := strings.Cut(p.Repo, "/")
		if !ok {
			return nil, nil, fmt.Errorf("%s is not owner/repo", p.Repo)
		}
		fmt.Fprintf(&q, "p%d:repository(owner:%s,name:%s){pullRequest(number:%d){title}}", i, quote(owner), quote(name), p.N)
	}
	if len(drafts) > 0 {
		search := "is:pr is:open draft:true sort:created-asc"
		for _, r := range drafts {
			search += " repo:" + r
		}
		fmt.Fprintf(&q, "d:search(query:%s,type:ISSUE,first:50){nodes{...on PullRequest{number repository{nameWithOwner}}}}", quote(search))
	}
	q.WriteString("}")
	// A pull request GitHub does not answer fails the request but keeps the
	// others' data.
	out, err := run(ctx, "api", "graphql", "-f", "query="+q.String())
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(out, &doc); doc.Data == nil {
		return nil, nil, cmp.Or(err, jerr, fmt.Errorf("gh api graphql: no data"))
	}
	titles := map[PR]string{}
	for i, p := range pulls {
		var r struct {
			PullRequest *struct {
				Title string `json:"title"`
			} `json:"pullRequest"`
		}
		if raw, ok := doc.Data[fmt.Sprintf("p%d", i)]; ok && json.Unmarshal(raw, &r) == nil && r.PullRequest != nil {
			titles[p] = r.PullRequest.Title
		}
	}
	var open []PR
	if raw, ok := doc.Data["d"]; ok {
		var s struct {
			Nodes []struct {
				Number     int `json:"number"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, fmt.Errorf("gh api graphql: %w", err)
		}
		for _, n := range s.Nodes {
			open = append(open, PR{Repo: n.Repository.NameWithOwner, N: n.Number})
		}
	}
	return titles, open, nil
}

// quote is s as a GraphQL string literal, whose escapes are JSON's.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
