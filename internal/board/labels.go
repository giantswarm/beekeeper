package board

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

type labelPage struct {
	Nodes    []struct{ Name string } `json:"nodes"`
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

// readLabels completes only the label connections that overflowed the
// initial page. All candidate sources need complete labels: a missing lease
// label would bypass the caller's resource ownership check.
func (c *Client) readLabels(ctx context.Context, snap *Snapshot) error {
	complete := map[string][]string{}
	fill := func(it *Item) error {
		key := strings.ToLower(it.Ref)
		if labels, ok := complete[key]; ok && it.labelsMore {
			it.Labels, it.labelsMore, it.labelsCursor = labels, false, ""
		}
		if !it.labelsMore {
			complete[key] = it.Labels
			return nil
		}
		owner, repo, n, err := Ref(it.Ref)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for it.labelsMore {
			cursor := it.labelsCursor
			if cursor == "" || seen[cursor] {
				return errors.New("label pagination did not advance")
			}
			seen[cursor] = true
			var r struct {
				Repository *struct {
					Issue *struct {
						Labels labelPage `json:"labels"`
					} `json:"issue"`
				} `json:"repository"`
			}
			err := c.graphql(github.ActingOn(ctx, owner+"/"+repo), &r, `query($o:String!,$r:String!,$n:Int!,$c:String!){repository(owner:$o,name:$r){issue(number:$n){labels(first:100,after:$c){nodes{name} pageInfo{hasNextPage endCursor}}}}}`,
				vars{"o": owner, "r": repo, "n": n, "c": cursor})
			if err != nil {
				return fmt.Errorf("labels of %s: %w", it.Ref, err)
			}
			if r.Repository == nil || r.Repository.Issue == nil {
				return fmt.Errorf("labels of %s are unread", it.Ref)
			}
			page := r.Repository.Issue.Labels
			for _, label := range page.Nodes {
				it.Labels = append(it.Labels, label.Name)
			}
			it.labelsMore, it.labelsCursor = page.PageInfo.HasNextPage, page.PageInfo.EndCursor
		}
		complete[key] = it.Labels
		return nil
	}
	for i := range snap.Items {
		if err := fill(&snap.Items[i]); err != nil {
			return err
		}
		for j := range snap.Items[i].SubIssues {
			if err := fill(&snap.Items[i].SubIssues[j]); err != nil {
				return err
			}
		}
	}
	for _, items := range snap.Search {
		for i := range items {
			if err := fill(&items[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
