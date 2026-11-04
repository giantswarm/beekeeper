package github

import (
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
// pullsPerHead the pull requests read per branch per page, forks' included.
const (
	headsPerRequest = 40
	pullsPerHead    = 10
)

// HeadPulls reads, in GraphQL requests per repository owner (ByOwner) of at
// most headsPerRequest branches, the newest pull request each branch of
// heads opened from the repository itself, never a fork's. A branch that
// heads none is missing from the map. An unavailable answer fails the read.
func HeadPulls(ctx context.Context, run GH, heads []Head) (map[Head]HeadPull, error) {
	pulls := map[Head]HeadPull{}
	for _, group := range ByOwner(heads, func(h Head) string { return h.Repo }) {
		for len(group) > 0 {
			n := min(len(group), headsPerRequest)
			if err := headPulls(ActingOn(ctx, group[0].Repo), run, group[:n], pulls); err != nil {
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

// headPage is a branch whose newest repository pull request is still being
// sought; after is the cursor past the forks already read.
type headPage struct {
	Head
	after string
}

// headPulls reads heads of one owner into pulls, following each branch's
// pages until a repository pull request is found or its pages end.
func headPulls(ctx context.Context, run GH, heads []Head, pulls map[Head]HeadPull) error {
	pages := make([]headPage, len(heads))
	for i, h := range heads {
		pages[i].Head = h
	}
	for len(pages) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		pctx := TryTimeout(ctx, refsTry+time.Duration(len(pages))*refsTryPerRef)
		next, err := headPullPage(pctx, run, pages, pulls)
		if err != nil {
			return err
		}
		pages = next
	}
	return nil
}

// headPullPage reads one page per unresolved branch and returns only the
// branches with more pages and no repository pull request yet.
func headPullPage(ctx context.Context, run GH, heads []headPage, pulls map[Head]HeadPull) ([]headPage, error) {
	var q strings.Builder
	q.WriteString("query{")
	for i, h := range heads {
		owner, name, ok := strings.Cut(h.Repo, "/")
		if !ok {
			return nil, fmt.Errorf("%s is not owner/repo", h.Repo)
		}
		after := ""
		if h.after != "" {
			after = ",after:" + quote(h.after)
		}
		fmt.Fprintf(&q, "h%d:repository(owner:%s,name:%s){pullRequests(headRefName:%s,first:%d%s,orderBy:{field:CREATED_AT,direction:DESC}){nodes{number state headRefOid closedAt isCrossRepository}pageInfo{hasNextPage endCursor}}}",
			i, quote(owner), quote(name), quote(h.Branch), pullsPerHead, after)
	}
	q.WriteString("}")
	out, err := run(ctx, "api", "graphql", "-f", "query="+q.String())
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	if doc.Data == nil {
		return nil, fmt.Errorf("gh api graphql: no data")
	}
	var next []headPage
	for i, h := range heads {
		var v struct {
			Pulls *struct {
				Nodes    []headNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"pullRequests"`
		}
		raw, ok := doc.Data[fmt.Sprintf("h%d", i)]
		if !ok {
			return nil, fmt.Errorf("gh api graphql: no answer for %s branch %s", h.Repo, h.Branch)
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("gh api graphql: %s branch %s: %w", h.Repo, h.Branch, err)
		}
		if v.Pulls == nil || v.Pulls.Nodes == nil {
			return nil, fmt.Errorf("gh api graphql: unavailable pull requests for %s branch %s", h.Repo, h.Branch)
		}
		own := slices.IndexFunc(v.Pulls.Nodes, func(n headNode) bool { return !n.Fork })
		if own < 0 {
			if p := v.Pulls.PageInfo; p.HasNextPage {
				if p.EndCursor == "" || p.EndCursor == h.after {
					return nil, fmt.Errorf("pull requests for %s branch %s: next page has no advancing cursor", h.Repo, h.Branch)
				}
				h.after = p.EndCursor
				next = append(next, h)
			}
			continue
		}
		n := v.Pulls.Nodes[own]
		p := HeadPull{N: n.Number, State: n.State, HeadOid: n.HeadOid}
		if n.ClosedAt != nil {
			p.ClosedAt = *n.ClosedAt
		}
		pulls[h.Head] = p
	}
	return next, nil
}
