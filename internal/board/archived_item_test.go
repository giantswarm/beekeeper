package board

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// tidiedStatus is the Status board tidy leaves an archived item in; moveI1 is
// I1's move to In Progress.
const (
	tidiedStatus = "Done"
	moveI1       = "I1=s3"
)

// archivedItem is issue5 with its item I1 on the board archived in Done,
// open or closed, with or without a Team.
func archivedItem(state, team string) string {
	t := jsonNull
	if team != "" {
		t = `{"name":"` + team + `"}`
	}
	return `{"data":{"repository":{"issue":{"id":"ISSUE","state":"` + state + `","projectItems":{"nodes":[` +
		`{"id":"I1","isArchived":true,"project":{"id":"P"},"status":{"name":"` + tidiedStatus + `"},"team":` + t + `}]}}}}}`
}

const notOnBoardClosed = `{"data":{"repository":{"issue":{"id":"ISSUE","state":"CLOSED","projectItems":{"nodes":[]}}}}}`

// The loop's intake of a closed issue puts it nowhere, archived item or
// not, and leaves an open issue's archived item as it is: no write, so no
// rank follows and nothing is retried
// (https://github.com/giantswarm/beekeeper/issues/3731).
func TestIntakeLeavesClosedIssuesAndArchivedItems(t *testing.T) {
	for _, tc := range []struct {
		name, item string
		want       Moved
	}{
		{"closed, archived", archivedItem(stateClosed, ""), Moved{Ref: issue5, From: tidiedStatus, To: tidiedStatus, Closed: true, Archived: true}},
		{"closed, off the board", notOnBoardClosed, Moved{Ref: issue5, To: backlog, Closed: true}},
		{"open, archived without a Team", archivedItem(stateOpen, ""), Moved{Ref: issue5, From: tidiedStatus, To: tidiedStatus, Archived: true, Kept: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGH{t: t, item: tc.item}
			c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
			mv, err := c.Intake(context.Background(), issue5, "backlog")
			if err != nil || *mv != tc.want || len(f.mutations) != 0 {
				t.Fatalf("Intake = %+v, %v; want %+v and no mutations, got %v", mv, err, tc.want, f.mutations)
			}
		})
	}
}

// rank leaves an archived item, which GitHub refuses to move: a closed
// issue's whatever the place says, an open issue's unless --unarchive
// restores it first. Neither the anchor nor the board is read for an item
// left.
func TestRankLeavesAnArchivedItemUnlessUnarchived(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		state     string
		unarchive bool
		want      Ranked
		mutations []string
	}{
		{"closed", stateClosed, false, Ranked{Ref: issue5, Archived: true, Closed: true}, nil},
		{"closed, --unarchive", stateClosed, true, Ranked{Ref: issue5, Archived: true, Closed: true}, nil},
		{"open", stateOpen, false, Ranked{Ref: issue5, Archived: true}, nil},
		{"open, --unarchive", stateOpen, true, Ranked{Ref: issue5, Above: issue9, Unarchived: true, Team: teamX}, []string{"unarchive I1", "I1=t1", rankAfterNine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGH{t: t, item: archivedItem(tc.state, ""), byIssue: map[string]string{"9": item9}}
			c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
			rk, err := c.Rank(ctx, issue5, Place{After: issue9, Unarchive: tc.unarchive})
			if err != nil || *rk != tc.want || !slices.Equal(f.mutations, tc.mutations) {
				t.Fatalf("Rank = %+v, %v; mutations %v; want %+v and %v", rk, err, f.mutations, tc.want, tc.mutations)
			}
			if tc.mutations == nil && slices.ContainsFunc(f.queries, func(q string) bool { return strings.Contains(q, "ISSUE9") || itemsRead.MatchString(q) }) {
				t.Errorf("a rank that leaves its item read more: %q", f.queries)
			}
		})
	}
}

// move restores an open issue's archived item before it sets the Status,
// and leaves a closed issue's as it is.
func TestMoveRestoresOnlyAnOpenIssuesArchivedItem(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{t: t, item: archivedItem(stateClosed, teamX)}
	c := &Client{GH: f.run, Board: config.Board{Owner: "o", Project: 7, Team: "X"}}
	mv, err := c.Move(ctx, issue5, "in progress")
	if want := (Moved{Ref: issue5, From: tidiedStatus, To: inProgress, Closed: true, Archived: true}); err != nil || *mv != want || len(f.mutations) != 0 {
		t.Fatalf("Move of a closed issue's archived item = %+v, %v; mutations %v", mv, err, f.mutations)
	}
	f = &fakeGH{t: t, item: archivedItem(stateOpen, teamX)}
	c.GH = f.run
	mv, err = c.Move(ctx, issue5, "in progress")
	if want := (Moved{Ref: issue5, From: tidiedStatus, To: inProgress, Archived: true, Unarchived: true}); err != nil || *mv != want ||
		!slices.Equal(f.mutations, []string{"unarchive I1", moveI1}) {
		t.Fatalf("Move of an open issue's archived item = %+v, %v; mutations %v", mv, err, f.mutations)
	}
}
