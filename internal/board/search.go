package board

import (
	"context"
	"errors"
)

type searchPage struct {
	Nodes    []issueJSON `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

// searchAfter reads the rest of a search step in the search's order. A full
// page of skipped candidates must not hide later eligible work.
func (c *Client) searchAfter(ctx context.Context, query, cursor string) ([]Item, []string, error) {
	var items []Item
	var missing []string
	seen := map[string]bool{}
	for {
		if cursor == "" || seen[cursor] {
			return nil, nil, errors.New("search pagination did not advance")
		}
		seen[cursor] = true
		var r struct {
			Search searchPage `json:"search"`
		}
		err := c.graphql(ctx, &r, `query($q:String!,$c:String!){search(query:$q,type:ISSUE,first:50,after:$c){nodes{...I} pageInfo{hasNextPage endCursor}}}`+issueFields,
			vars{"q": searchQuery(query), "c": cursor})
		p, ok := partialOf(err)
		if !ok {
			return nil, nil, err
		}
		if p != nil {
			for _, e := range p.Errors {
				missing = noted(missing, e.Message)
			}
		}
		for _, n := range r.Search.Nodes {
			if n.open() {
				items = append(items, n.item())
			}
		}
		if !r.Search.PageInfo.HasNextPage {
			return items, missing, nil
		}
		cursor = r.Search.PageInfo.EndCursor
	}
}
