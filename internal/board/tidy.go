package board

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DoneStatus is the Status a closed item is moved to, the board's value or
// an unambiguous part of it.
const DoneStatus = "done"

// tidyBatch bounds the writes one Tidy makes: a board that was never tidied
// is tidied over several passes, each a bounded number of requests.
const tidyBatch = 100

// Tidied is one closed item Tidy moved to Done, archived, or both.
type Tidied struct {
	Ref string `json:"ref"`
	URL string `json:"url"`
	// Item is the board item's id, which restores an archived item
	// (gh project item-archive --undo).
	Item   string    `json:"item"`
	Closed time.Time `json:"closed"`
	// From is the Status the item had when it was moved to Done ("" for
	// none); Done says it was moved.
	From string `json:"from,omitempty"`
	Done bool   `json:"done,omitempty"`
	// Archived says the item was archived: closed for board.archiveAfter.
	Archived bool `json:"archived,omitempty"`
}

// Tidy is what one tidy of the board did, or would do on a dry run.
type Tidy struct {
	// Done is the board's Status a closed item is moved to; "" when the
	// board has none, the items then archived with their Status as it is.
	Done  string   `json:"done,omitempty"`
	Items []Tidied `json:"items,omitempty"`
	// Left is how many closed items wait for a later tidy past tidyBatch
	// writes.
	Left int `json:"left,omitempty"`
	// DryRun says nothing was written.
	DryRun bool `json:"dryRun,omitempty"`
}

// closedItem is a board item whose issue or pull request is closed, as
// closedItems reads it.
type closedItem struct {
	ID       string
	Archived bool
	Status   string
	Ref, URL string
	Closed   time.Time
}

// Tidy keeps the board free of finished work: every closed issue or pull
// request among the team's items (board.team; every item without one) whose
// Status is not Done is moved to Done, and one closed for board.archiveAfter
// or longer is archived (archiveProjectV2Item), after the move. An archived
// item stays on the board, searchable (is:archived) and restorable. A
// negative archiveAfter archives nothing; the moves to Done still happen. A
// board without a Done status has its items archived as they are. One
// request per 100 closed items reads them, and at most tidyBatch writes are
// made, the rest counted in Left for the next tidy. dry writes nothing.
func (c *Client) Tidy(ctx context.Context, now time.Time, dry bool) (*Tidy, error) {
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	sf := m.Fields[StatusField]
	t := &Tidy{DryRun: dry}
	if done, err := Resolve(StatusField, sf.names(), DoneStatus); err == nil {
		t.Done = done
	}
	items, err := c.closedItems(ctx)
	if err != nil {
		return nil, err
	}
	after := c.Board.ArchiveAfter.Duration
	writes := 0
	for _, it := range items {
		if it.Archived {
			continue
		}
		td := Tidied{Ref: it.Ref, URL: it.URL, Item: it.ID, Closed: it.Closed, From: it.Status}
		td.Done = t.Done != "" && it.Status != t.Done
		td.Archived = after >= 0 && !it.Closed.IsZero() && !now.Before(it.Closed.Add(after))
		if !td.Done && !td.Archived {
			continue
		}
		n := 0
		if td.Done {
			n++
		}
		if td.Archived {
			n++
		}
		if writes+n > tidyBatch {
			t.Left++
			continue
		}
		writes += n
		if !dry {
			if td.Done {
				if err := c.set(ctx, m.ID, it.ID, sf.ID, sf.option(t.Done)); err != nil {
					return t, fmt.Errorf("moving %s to %s: %w", it.Ref, t.Done, err)
				}
			}
			if td.Archived {
				if err := c.archive(ctx, m.ID, it.ID); err != nil {
					// The move to Done stands: it is reported with the error.
					if td.Done {
						td.Archived = false
						t.Items = append(t.Items, td)
					}
					return t, fmt.Errorf("archiving %s: %w", it.Ref, err)
				}
			}
		}
		t.Items = append(t.Items, td)
	}
	return t, nil
}

// archive archives the item of the project.
func (c *Client) archive(ctx context.Context, project, item string) error {
	var done json.RawMessage
	return c.graphql(ctx, &done, `mutation($p:ID!,$i:ID!){archiveProjectV2Item(input:{projectId:$p,itemId:$i}){item{id}}}`,
		vars{"p": project, "i": item})
}

// closedItems reads the board's closed issues and pull requests (merged
// ones too), the team's when board.team names one, 100 per request.
func (c *Client) closedItems(ctx context.Context) ([]closedItem, error) {
	q := "is:closed"
	if c.Board.Team != "" {
		q += fmt.Sprintf(" %s:%q", strings.ToLower(TeamField), c.Board.Team)
	}
	type content struct {
		Number     int        `json:"number"`
		URL        string     `json:"url"`
		State      string     `json:"state"`
		ClosedAt   *time.Time `json:"closedAt"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	var out []closedItem
	for cursor := ""; ; {
		var r struct {
			Owner struct {
				Project struct {
					Items struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsArchived bool   `json:"isArchived"`
							Status     *struct {
								Name string `json:"name"`
							} `json:"status"`
							Content content `json:"content"`
						} `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"repositoryOwner"`
		}
		vs := vars{"o": c.Board.Owner, "n": c.Board.Project, "q": q}
		if cursor != "" {
			vs["c"] = cursor
		}
		err := c.graphql(ctx, &r, `query($o:String!,$n:Int!,$q:String!,$c:String){repositoryOwner(login:$o){...on ProjectV2Owner{projectV2(number:$n){
items(first:100,after:$c,query:$q){pageInfo{hasNextPage endCursor} nodes{id isArchived
status:fieldValueByName(name:"`+StatusField+`"){...on ProjectV2ItemFieldSingleSelectValue{name}}
content{...on Issue{number url state closedAt repository{nameWithOwner}} ...on PullRequest{number url state closedAt repository{nameWithOwner}}}}}}}}}`, vs)
		if err != nil {
			return nil, err
		}
		items := r.Owner.Project.Items
		for _, n := range items.Nodes {
			// A draft has no URL; the query's is:closed is checked again.
			if n.Content.URL == "" || n.Content.State == stateOpen {
				continue
			}
			it := closedItem{ID: n.ID, Archived: n.IsArchived, URL: n.Content.URL,
				Ref: fmt.Sprintf("%s#%d", n.Content.Repository.NameWithOwner, n.Content.Number)}
			if n.Status != nil {
				it.Status = n.Status.Name
			}
			if n.Content.ClosedAt != nil {
				it.Closed = *n.Content.ClosedAt
			}
			out = append(out, it)
		}
		if !items.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = items.PageInfo.EndCursor
	}
}
