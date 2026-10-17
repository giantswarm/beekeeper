package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Head is a branch of a repository: owner/repo and the branch name.
type Head struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// HeadPull is the newest pull request a branch of the repository itself
// heads (a fork's branch of the same name is another branch): its number,
// its state (OPEN, CLOSED or MERGED), the commit its head was at and when it
// closed (zero while open).
type HeadPull struct {
	N        int
	State    string
	HeadOid  string
	ClosedAt time.Time
}

// headsPerRequest bounds the branches one GraphQL request asks about;
// pullsPerHead the pull requests read per branch, forks' included.
const (
	headsPerRequest = 40
	pullsPerHead    = 10
)

// HeadPulls reads, in GraphQL requests per repository owner (ByOwner) of at
// most headsPerRequest branches, the newest pull request each branch of
// heads opened from the repository itself, never a fork's. A branch that
// heads none, or that GitHub does not answer, is missing from the map.
func HeadPulls(ctx context.Context, run GH, heads []Head) (map[Head]HeadPull, error) {
	pulls := map[Head]HeadPull{}
	for _, group := range ByOwner(heads, func(h Head) string { return h.Repo }) {
		for len(group) > 0 {
			n := min(len(group), headsPerRequest)
			gctx := TryTimeout(ActingOn(ctx, group[0].Repo), refsTry+time.Duration(n)*refsTryPerRef)
			if err := headPulls(gctx, run, group[:n], pulls); err != nil {
				return nil, err
			}
			group = group[n:]
		}
	}
	return pulls, nil
}

// headNode is a pull request as HeadPulls asks GitHub about it.
type headNode struct {
	Number   int        `json:"number"`
	State    string     `json:"state"`
	HeadOid  string     `json:"headRefOid"`
	ClosedAt *time.Time `json:"closedAt"`
	Fork     bool       `json:"isCrossRepository"`
}

// headPulls is one request of HeadPulls, of heads of one owner, into pulls.
func headPulls(ctx context.Context, run GH, heads []Head, pulls map[Head]HeadPull) error {
	var q strings.Builder
	q.WriteString("query{")
	for i, h := range heads {
		owner, name, ok := strings.Cut(h.Repo, "/")
		if !ok {
			return fmt.Errorf("%s is not owner/repo", h.Repo)
		}
		fmt.Fprintf(&q, "h%d:repository(owner:%s,name:%s){pullRequests(headRefName:%s,first:%d,orderBy:{field:CREATED_AT,direction:DESC}){nodes{number state headRefOid closedAt isCrossRepository}}}",
			i, quote(owner), quote(name), quote(h.Branch), pullsPerHead)
	}
	q.WriteString("}")
	// A repository GitHub does not answer fails the request but keeps the
	// others' data.
	out, err := run(ctx, "api", "graphql", "-f", "query="+q.String())
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(out, &doc); doc.Data == nil {
		return cmp.Or(err, jerr, fmt.Errorf("gh api graphql: no data"))
	}
	for i, h := range heads {
		var v struct {
			Pulls *struct {
				Nodes []headNode `json:"nodes"`
			} `json:"pullRequests"`
		}
		raw, ok := doc.Data[fmt.Sprintf("h%d", i)]
		if !ok || json.Unmarshal(raw, &v) != nil || v.Pulls == nil {
			continue
		}
		own := slices.IndexFunc(v.Pulls.Nodes, func(n headNode) bool { return !n.Fork })
		if own < 0 {
			continue
		}
		n := v.Pulls.Nodes[own]
		p := HeadPull{N: n.Number, State: n.State, HeadOid: n.HeadOid}
		if n.ClosedAt != nil {
			p.ClosedAt = *n.ClosedAt
		}
		pulls[h] = p
	}
	return nil
}
