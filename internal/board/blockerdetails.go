package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// blockerDetails reads linked pull requests only for recorded open blockers.
// An open blocker holds the item regardless of its PR's draft/parked state,
// so an unresolved detail never turns held work into ready work.
func (c *Client) blockerDetails(ctx context.Context, snap *Snapshot) error {
	var items []*Item
	var collect func([]Item)
	collect = func(xs []Item) {
		for i := range xs {
			items = append(items, &xs[i])
			collect(xs[i].SubIssues)
		}
	}
	collect(snap.Items)
	for _, xs := range snap.Search {
		collect(xs)
	}
	var refs []string
	for _, it := range items {
		for _, ref := range it.OpenBlockedBy {
			if !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	details := map[string]string{}
	const batch = 20
	for start := 0; start < len(refs); start += batch {
		end := min(start+batch, len(refs))
		var q strings.Builder
		q.WriteString("query{")
		for i, ref := range refs[start:end] {
			owner, repo, n, err := Ref(ref)
			if err != nil {
				return err
			}
			fmt.Fprintf(&q, "b%d:repository(owner:%s,name:%s){issue(number:%d){closedByPullRequestsReferences(first:100,includeClosedPrs:false){nodes{number state isDraft repository{nameWithOwner}}}}}", i, quote(owner), quote(repo), n)
		}
		q.WriteString("}")
		var result map[string]struct {
			Issue *struct {
				PRs struct {
					Nodes []struct {
						depRef
						IsDraft bool `json:"isDraft"`
					} `json:"nodes"`
				} `json:"closedByPullRequestsReferences"`
			} `json:"issue"`
		}
		if err := c.graphql(ctx, &result, q.String(), nil); err != nil {
			return err
		}
		for i, ref := range refs[start:end] {
			issue := result[fmt.Sprintf("b%d", i)].Issue
			if issue == nil {
				continue
			}
			var prs []string
			for _, pr := range issue.PRs.Nodes {
				if pr.State != stateOpen || pr.Number == 0 || pr.Repository.NameWithOwner == "" {
					continue
				}
				state := "unmerged"
				if pr.IsDraft {
					state = "draft"
				}
				prs = append(prs, fmt.Sprintf("%s#%d (%s)", pr.Repository.NameWithOwner, pr.Number, state))
			}
			if len(prs) > 0 {
				details[ref] = ref + " awaiting " + strings.Join(prs, ", ")
			}
		}
	}
	for _, it := range items {
		for _, ref := range it.OpenBlockedBy {
			detail := details[ref]
			if detail == "" {
				detail = ref
			}
			it.BlockerDetails = append(it.BlockerDetails, detail)
		}
	}
	return nil
}
