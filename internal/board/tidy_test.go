package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

const (
	doneValue = "Done ✅"
	jsonNull  = "null"
)

const tidyMetaJSON = `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[
{"id":"FS","name":"Status","options":[{"id":"s1","name":"Backlog 📦"},{"id":"s3","name":"In Progress ⛏️"},{"id":"s9","name":"Done ✅"}]}]}}}}}`

// tidyGH answers Tidy's requests: the fields, the closed items in pages,
// and records every write.
type tidyGH struct {
	t      *testing.T
	meta   string
	pages  []string
	query  string
	writes []string
}

func (f *tidyGH) run(_ context.Context, args ...string) ([]byte, error) {
	checkSchema(f.t, args)
	q := args[3]
	vars := map[string]string{}
	for i := 4; i+1 < len(args); i += 2 {
		k, v, _ := strings.Cut(args[i+1], "=")
		vars[k] = v
	}
	switch {
	case strings.Contains(q, "archiveProjectV2Item"):
		f.writes = append(f.writes, "archive "+vars["i"])
		return []byte(`{"data":{"archiveProjectV2Item":{"item":{"id":"x"}}}}`), nil
	case strings.Contains(q, "updateProjectV2ItemFieldValue"):
		f.writes = append(f.writes, vars["i"]+"="+vars["v"])
		return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
	case strings.Contains(q, "fields(first:50)"):
		return []byte(f.meta), nil
	case strings.Contains(q, "items(first:100"):
		f.query = vars["q"]
		page := 0
		if vars["c"] != "" {
			page = 1
		}
		return []byte(f.pages[page]), nil
	}
	f.t.Fatalf("unexpected request %v", args)
	return nil, nil
}

// closedNode is a board item of the issue n with status, closed at closed
// (zero for an open issue), archived or not.
func closedNode(id string, n int, status string, closed time.Time, archived bool) string {
	state, at := "CLOSED", fmt.Sprintf("%q", closed.Format(time.RFC3339))
	if closed.IsZero() {
		state, at = "OPEN", jsonNull
	}
	st := jsonNull
	if status != "" {
		st = fmt.Sprintf(`{"name":%q}`, status)
	}
	return fmt.Sprintf(`{"id":%q,"isArchived":%t,"status":%s,"content":{"number":%d,"url":"https://github.com/o/r/issues/%d","state":%q,"closedAt":%s,"repository":{"nameWithOwner":"o/r"}}}`,
		id, archived, st, n, n, state, at)
}

func tidyPage(next bool, nodes ...string) string {
	return fmt.Sprintf(`{"data":{"repositoryOwner":{"projectV2":{"items":{"pageInfo":{"hasNextPage":%t,"endCursor":"C"},"nodes":[%s]}}}}}`, next, strings.Join(nodes, ","))
}

func tidyBoard() config.Board {
	return config.Board{Owner: "o", Project: 7, Team: "X", ArchiveAfter: config.Duration{Duration: config.DefaultBoardArchiveAfter}}
}

func TestTidyMovesClosedItemsToDoneAndArchivesThemAfterTheGrace(t *testing.T) {
	f := &tidyGH{t: t, meta: tidyMetaJSON, pages: []string{
		tidyPage(true,
			// Closed yesterday, still In Progress: to Done, not archived yet.
			closedNode("I1", 1, inProgress, days(1), false),
			// Closed three weeks ago, In Progress: to Done, then archived.
			closedNode("I2", 2, inProgress, days(21), false),
			// Closed three weeks ago and Done: archived only.
			closedNode("I3", 3, doneValue, days(21), false),
			// A draft: no content URL.
			`{"id":"D","isArchived":false,"status":null,"content":{}}`,
		),
		tidyPage(false,
			// Done, closed yesterday: nothing to do.
			closedNode("I4", 4, doneValue, days(1), false),
			// Archived already: left alone.
			closedNode("I5", 5, inProgress, days(60), true),
			// Open after all (the filter is checked again): left alone.
			closedNode("I6", 6, inProgress, time.Time{}, false),
			// Closed exactly at the grace: archived, without a Status first.
			closedNode("I7", 7, "", now.Add(-config.DefaultBoardArchiveAfter), false),
		),
	}}
	c := &Client{GH: f.run, Board: tidyBoard()}
	td, err := c.Tidy(context.Background(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.query != `is:closed team:"X"` {
		t.Errorf("query = %q, want the team's closed items", f.query)
	}
	want := []string{"I1=s9", "I2=s9", "archive I2", "archive I3", "I7=s9", "archive I7"}
	if !slices.Equal(f.writes, want) {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	if td.Done != doneValue || len(td.Items) != 4 || td.Left != 0 {
		t.Fatalf("tidy = %+v", td)
	}
	if it := td.Items[1]; it.Ref != ref2 || !it.Done || !it.Archived || it.From != inProgress || it.Item != "I2" {
		t.Errorf("item 2 = %+v", it)
	}
	if it := td.Items[0]; !it.Done || it.Archived {
		t.Errorf("item 1 = %+v: moved to Done, kept on the board within the grace", it)
	}
}

func TestTidyDryRunWritesNothing(t *testing.T) {
	f := &tidyGH{t: t, meta: tidyMetaJSON, pages: []string{tidyPage(false, closedNode("I2", 2, inProgress, days(21), false))}}
	c := &Client{GH: f.run, Board: tidyBoard()}
	td, err := c.Tidy(context.Background(), now, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || len(td.Items) != 1 || !td.Items[0].Done || !td.Items[0].Archived || !td.DryRun {
		t.Errorf("dry run: writes %v, tidy %+v", f.writes, td)
	}
}

func TestTidyNeverArchivesWithANegativeGraceAndWithoutTeamReadsEveryItem(t *testing.T) {
	f := &tidyGH{t: t, meta: tidyMetaJSON, pages: []string{tidyPage(false, closedNode("I2", 2, inProgress, days(400), false), closedNode("I3", 3, doneValue, days(400), false))}}
	b := tidyBoard()
	b.Team, b.ArchiveAfter.Duration = "", -1
	c := &Client{GH: f.run, Board: b}
	if _, err := c.Tidy(context.Background(), now, false); err != nil {
		t.Fatal(err)
	}
	if f.query != "is:closed" {
		t.Errorf("query = %q", f.query)
	}
	if !slices.Equal(f.writes, []string{"I2=s9"}) {
		t.Errorf("writes = %v, want the move to Done alone", f.writes)
	}
}

func TestTidyArchivesAsTheyAreOnABoardWithoutDone(t *testing.T) {
	meta := `{"data":{"repositoryOwner":{"projectV2":{"id":"P","fields":{"nodes":[{"id":"FS","name":"Status","options":[{"id":"s3","name":"In Progress ⛏️"}]}]}}}}}`
	f := &tidyGH{t: t, meta: meta, pages: []string{tidyPage(false, closedNode("I1", 1, inProgress, days(1), false), closedNode("I2", 2, inProgress, days(21), false))}}
	c := &Client{GH: f.run, Board: tidyBoard()}
	td, err := c.Tidy(context.Background(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	if td.Done != "" || !slices.Equal(f.writes, []string{"archive I2"}) {
		t.Errorf("tidy %+v, writes %v", td, f.writes)
	}
}

func TestTidyBoundsItsWrites(t *testing.T) {
	var nodes []string
	for i := range tidyBatch {
		nodes = append(nodes, closedNode(fmt.Sprintf("I%d", i), i+1, inProgress, days(21), false))
	}
	f := &tidyGH{t: t, meta: tidyMetaJSON, pages: []string{tidyPage(false, nodes...)}}
	c := &Client{GH: f.run, Board: tidyBoard()}
	td, err := c.Tidy(context.Background(), now, false)
	if err != nil {
		t.Fatal(err)
	}
	// Two writes each: half are tidied, half left for the next tidy.
	if len(f.writes) != tidyBatch || len(td.Items) != tidyBatch/2 || td.Left != tidyBatch/2 {
		t.Errorf("writes %d, items %d, left %d", len(f.writes), len(td.Items), td.Left)
	}
}
