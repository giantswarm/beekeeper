package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// PR is a pull request: its owner/repo and number.
type PR struct {
	Repo string `json:"repo"`
	N    int    `json:"n"`
}

// Lookup reads in GraphQL requests per repository owner (ByOwner) the
// titles of pulls and the open draft pull requests of the repositories
// drafts (owner/repo), oldest first within each owner. Further draft pages
// are read without asking for the titles again; an incomplete search is an
// error, never a partial list reported as complete.
func Lookup(ctx context.Context, run GH, pulls []PR, drafts []string) (map[PR]string, []PR, error) {
	if len(pulls) == 0 && len(drafts) == 0 {
		return nil, nil, nil
	}
	// A part is a pull request to title or, with N 0, a repository whose
	// drafts are searched.
	parts := slices.Clone(pulls)
	for _, r := range drafts {
		parts = append(parts, PR{Repo: r})
	}
	titles := map[PR]string{}
	var open []PR
	for _, group := range ByOwner(parts, func(p PR) string { return p.Repo }) {
		var gp []PR
		var gd []string
		for _, p := range group {
			if p.N == 0 {
				gd = append(gd, p.Repo)
			} else {
				gp = append(gp, p)
			}
		}
		o, err := lookup(ActingOn(ctx, group[0].Repo), run, gp, gd, titles)
		if err != nil {
			return nil, nil, err
		}
		open = append(open, o...)
	}
	return titles, open, nil
}

// draftSearch is one page of the draft search and its continuation.
type draftSearch struct {
	Count int `json:"issueCount"`
	Nodes []struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

// lookup reads titles and every draft page of one owner.
func lookup(ctx context.Context, run GH, pulls []PR, drafts []string, titles map[PR]string) ([]PR, error) {
	var open []PR
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := lookupPage(ctx, run, pulls, drafts, titles, after)
		if err != nil {
			return nil, err
		}
		for _, n := range page.Nodes {
			open = append(open, PR{Repo: n.Repository.NameWithOwner, N: n.Number})
		}
		if !page.PageInfo.HasNextPage {
			if page.Count > len(open) {
				return nil, fmt.Errorf("draft search returned %d of %d pull requests: incomplete results", len(open), page.Count)
			}
			return open, nil
		}
		if cursor := page.PageInfo.EndCursor; cursor == "" || cursor == after {
			return nil, fmt.Errorf("draft search: next page has no advancing cursor")
		} else {
			after = cursor
		}
		pulls = nil
	}
}

// lookupPage is one request of Lookup, of pulls and drafts of one owner:
// the titles go into titles, the draft search page is returned.
func lookupPage(ctx context.Context, run GH, pulls []PR, drafts []string, titles map[PR]string, after string) (draftSearch, error) {
	var q strings.Builder
	q.WriteString("query{")
	for i, p := range pulls {
		owner, name, ok := strings.Cut(p.Repo, "/")
		if !ok {
			return draftSearch{}, fmt.Errorf("%s is not owner/repo", p.Repo)
		}
		fmt.Fprintf(&q, "p%d:repository(owner:%s,name:%s){pullRequest(number:%d){title}}", i, quote(owner), quote(name), p.N)
	}
	if len(drafts) > 0 {
		search := "is:pr is:open draft:true sort:created-asc"
		for _, r := range drafts {
			search += " repo:" + r
		}
		cursor := ""
		if after != "" {
			cursor = ",after:" + quote(after)
		}
		fmt.Fprintf(&q, "d:search(query:%s,type:ISSUE,first:50%s){issueCount nodes{...on PullRequest{number repository{nameWithOwner}}}pageInfo{hasNextPage endCursor}}", quote(search), cursor)
	}
	q.WriteString("}")
	// A pull request GitHub does not answer fails the request but keeps the
	// others' data.
	out, err := run(ctx, "api", "graphql", "-f", "query="+q.String())
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(out, &doc); doc.Data == nil {
		return draftSearch{}, cmp.Or(err, jerr, fmt.Errorf("gh api graphql: no data"))
	}
	// Only a title may fail with the rest kept: a request without titles (a
	// draft continuation) that fails is an error, whatever data it carries.
	if err != nil && len(pulls) == 0 {
		return draftSearch{}, err
	}
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
	if len(drafts) == 0 {
		return draftSearch{}, nil
	}
	var page *draftSearch
	if jerr := json.Unmarshal(doc.Data["d"], &page); jerr != nil || page == nil {
		return draftSearch{}, cmp.Or(err, jerr, fmt.Errorf("gh api graphql: no draft search data"))
	}
	return *page, nil
}

// quote is s as a GraphQL string literal, whose escapes are JSON's.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
