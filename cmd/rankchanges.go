package cmd

import (
	"context"
	"fmt"

	"github.com/giantswarm/beekeeper/internal/board"
)

// rankedFile is the side file of the last read of the priority list.
const rankedFile = "board-ranked.json"

// rankChanges compares list with the last read of the priority list: each
// change of a ranked epic's rank or Team beekeeper did not write is a
// board.changed event and a line returned; an epic that left the list is
// read on its own (RankedNow), never the board. The record is list
// afterwards. The first read records only.
func (a *app) rankChanges(ctx context.Context, cl *board.Client, list board.Priorities) []string {
	var was []board.Seen
	found, err := a.store.ReadFile(rankedFile, &was)
	if err != nil || !found {
		_ = a.store.WriteFile(rankedFile, board.SeenOf(list))
		return nil
	}
	var unread []string
	changes := board.Changes(was, list, func(ref string) *board.RankedEpic {
		e, err := cl.RankedNow(ctx, ref)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s left the priority list, its board item unread: %v", ref, err))
			return nil
		}
		return e
	})
	lines := unread
	me, _ := a.caller()
	for _, c := range changes {
		_ = a.store.Log(event(me, "board.changed", "%s", c.Text()))
		lines = append(lines, "changed outside beekeeper: "+c.Text())
	}
	if len(unread) == 0 {
		_ = a.store.WriteFile(rankedFile, board.SeenOf(list))
	}
	return lines
}

// recordRanked records list as the last read of the priority list: a write
// of beekeeper's own is no change.
func (a *app) recordRanked(list board.Priorities) {
	_ = a.store.WriteFile(rankedFile, board.SeenOf(list))
}
