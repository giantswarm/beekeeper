package cmd

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	crossHome    = "personal/tasks#1"
	crossForeign = "org/engine#1"
	crossEpic    = "org/engine#10"
	crossChild   = "org/engine#11"
	crossBacklog = "org/engine#2"
)

// One project holds native issues from both owners. Its status and position
// order must survive selection and the loop's capacity-bound dispatch.
func crossOwnerSnapshot(t *testing.T, now time.Time) *board.Snapshot {
	t.Helper()
	item := func(ref, status string) board.Item {
		ownerRepo, number, _ := strings.Cut(ref, "#")
		return board.Item{Ref: ref, URL: "https://github.com/" + ownerRepo + "/issues/" + number, OnBoard: true, Status: status, Created: now, Updated: now}
	}
	ep := item(crossEpic, stepInProgress)
	ep.Kind, ep.OpenSubIssues = kindEpic, 1
	child := item(crossChild, "")
	child.OnBoard = false
	child.Parent = crossEpic
	ep.SubIssues = []board.Item{child}
	return &board.Snapshot{
		Order: []config.BoardStep{
			{Name: stepInProgress, Status: []string{stepInProgress}, SubIssues: true},
			{Name: stepUpNext, Status: []string{stepUpNext}},
			{Name: "Backlog", Status: []string{"Backlog"}},
		},
		Gate: board.Gate{Kind: []string{kindEpic}, Planning: []string{stepUpNext}, Go: stepInProgress},
		// Deliberately put Backlog before Up Next before In Progress.
		Items:      []board.Item{item(crossBacklog, "Backlog"), item(crossHome, stepUpNext), item(crossForeign, stepUpNext), ep},
		Priorities: board.Priorities{{Ref: crossEpic, Rank: 2}, {Ref: crossHome, Rank: 1}},
	}
}

func crossOwnerCandidates(snap *board.Snapshot, now time.Time) []board.Candidate {
	cands := board.Rank(snap, config.Board{Owner: "personal"}, now)
	board.Annotate(cands, snap.Priorities.Ranks(), snap.Parent)
	return cands
}

func TestCrossOwnerBoardOrderPriorityAndUnstaffedEpic(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	snap := crossOwnerSnapshot(t, now)
	cands := crossOwnerCandidates(snap, now)
	want := []string{crossChild, crossHome, crossForeign, crossBacklog}
	var got []string
	for _, c := range cands {
		got = append(got, c.Ref)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if cands[0].Priority != 2 || cands[0].PriorityOf != crossEpic || cands[1].Priority != 1 {
		t.Fatalf("priority annotations lost across owners: %+v", cands)
	}
	// No worker serves the foreign epic: its child is immediately free.
	scope := pickScope{listed: now, alive: func(state.Party) bool { return false }}
	res := nextFree(&state.State{}, cands, scope)
	if res.Pick == nil || res.Pick.Ref != crossChild {
		t.Fatalf("unstaffed epic pick = %+v", res.Pick)
	}
	// Planning still holds the foreign epic's children, without blocking the
	// other owner's Up Next item. No hand-start queue changes the gate.
	snap.Items[3].Status = stepUpNext
	res = nextFree(&state.State{}, crossOwnerCandidates(snap, now), scope)
	if res.Pick == nil || res.Pick.Ref != crossHome {
		t.Fatalf("planning pick = %+v", res.Pick)
	}
}

func TestLoopStartsCrossOwnerBoardPickWithinCapacity(t *testing.T) {
	for _, room := range []bool{true, false} {
		t.Run(map[bool]string{true: "capacity permits", false: "capacity refuses"}[room], func(t *testing.T) {
			r, busy := fillRig(t)
			snap := crossOwnerSnapshot(t, r.a.now)
			original := r.answer
			var dispatched []string
			r.answer = func(args []string) (string, int) {
				switch args[0] {
				case fillCapacityCommand:
					if !room || *busy > 0 {
						return fillNoRoomJSON, 0
					}
				case loopBoard:
					st, err := r.a.store.Read()
					if err != nil {
						t.Fatal(err)
					}
					res := nextFree(st, crossOwnerCandidates(snap, r.a.now), pickScope{listed: r.a.now, alive: func(state.Party) bool { return false }})
					out, err := json.Marshal(res)
					if err != nil {
						t.Fatal(err)
					}
					return string(out), 0
				case agentsName:
					if args[1] == agentStartName {
						brief, err := os.ReadFile(args[3])
						if err != nil {
							t.Fatal(err)
						}
						dispatched = append(dispatched, string(brief))
					}
				}
				return original(args)
			}
			r.pass(t)
			if !room {
				if *busy != 0 || len(dispatched) != 0 || r.called(loopBoard, "next") != nil {
					t.Fatalf("capacity bypassed: %s", r.out)
				}
				return
			}
			if *busy != 1 || len(dispatched) != 1 || !strings.Contains(dispatched[0], "https://github.com/org/engine/issues/11") {
				t.Fatalf("foreign board pick not dispatched: busy %d, briefs %v, %s", *busy, dispatched, r.out)
			}
			calls := r.called(agentsName, agentStartName)
			if len(calls) == 0 || !slices.Contains(calls, harnessAuto) {
				t.Fatalf("lost capacity model choice: %v", calls)
			}
		})
	}
}
