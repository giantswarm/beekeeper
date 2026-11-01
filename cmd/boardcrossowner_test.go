package cmd

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
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

func TestLoopIntakesAndRanksCrossOwnerFiledIssueAndBoardNextOffersIt(t *testing.T) {
	r, busy := fillRig(t)
	r.a.cfg.Board = config.Board{Owner: "personal", Project: 1}
	snap := crossOwnerSnapshot(t, r.a.now)
	// Place crossHome (priority 1) at the top of the board and crossBacklog
	// (unranked) at the bottom, where an unranked backlog item sits.
	snap.Items = []board.Item{snap.Items[1], snap.Items[2], snap.Items[3], snap.Items[0]}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{
			Party: state.Party{Name: "worker-1", Session: "s-worker-1"},
			Task:  crossHome,
		}}
		st.Records = []state.Record{
			{Session: state.Party{Name: "worker-1", Session: "s-worker-1"}, Issue: crossHome, At: r.a.now},
			{Session: state.Party{Name: "worker-epic", Session: "s-epic"}, Issue: crossChild, At: r.a.now},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Worker-1, serving crossHome (priority 1), reports done and files a
	// blocking/follow-up issue in another owner's repository (org/engine#88).
	const filedRef = "org/engine#88"
	const filedURL = "https://github.com/org/engine/issues/88"
	now := r.a.now.UTC()
	if err := r.a.store.Record(
		event(state.Party{Name: "worker-1"}, superloop.VerbDone, "worker-1 done: finished %s", crossHome),
		event(state.Party{Name: "worker-1"}, superloop.VerbProblem, "worker-1: filed %s to unblock %s", filedURL, crossHome),
	); err != nil {
		t.Fatal(err)
	}

	wasAnchor := readIntakeAnchor
	readIntakeAnchor = func(_ context.Context, _ *app, ref, from string) (string, error) {
		if ref == filedRef && from == crossHome {
			return crossHome, nil
		}
		return crossEpic, nil
	}
	t.Cleanup(func() { readIntakeAnchor = wasAnchor })

	original := r.answer
	var dispatched []string
	r.answer = func(args []string) (string, int) {
		switch {
		case args[0] == fillCapacityCommand && *busy > 0:
			return fillNoRoomJSON, 0
		case slices.Equal(args, []string{loopBoard, "move", filedRef, "backlog", "--intake"}):
			// GitHub adds a new item at the very bottom of the board in Backlog
			// (after crossBacklog).
			snap.Items = append(snap.Items, board.Item{
				Ref: filedRef, URL: filedURL, OnBoard: true, Status: "Backlog", Created: now, Updated: now,
			})
			return "added org/engine#88 to the board personal/1, Team X: → Backlog\n", 0
		case len(args) == 5 && args[0] == loopBoard && args[1] == "rank" && args[2] == filedRef && args[3] == "--after":
			anchor := args[4]
			// Move filedRef right after anchor in snap.Items (as updateProjectV2ItemPosition does).
			idx := slices.IndexFunc(snap.Items, func(it board.Item) bool { return it.Ref == filedRef })
			if idx >= 0 {
				it := snap.Items[idx]
				snap.Items = slices.Delete(snap.Items, idx, idx+1)
				aIdx := slices.IndexFunc(snap.Items, func(x board.Item) bool { return x.Ref == anchor })
				if aIdx >= 0 {
					snap.Items = slices.Insert(snap.Items, aIdx+1, it)
				} else {
					snap.Items = append(snap.Items, it)
				}
			}
			return "ranked org/engine#88 on the board personal/1: after " + anchor + "\n", 0
		case args[0] == loopBoard && args[1] == "next":
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			// Mark crossHome and crossForeign as served so Backlog is reached:
			// because org/engine#88 was ranked right after crossHome (above
			// crossBacklog at the bottom), board next offers org/engine#88 first!
			sc := pickScope{
				listed: r.a.now,
				alive:  func(p state.Party) bool { return p.Session == "s-epic" || p.Session == "s-home" || p.Session == "s-foreign" },
			}
			st.Records = append(st.Records,
				state.Record{Session: state.Party{Name: "worker-home", Session: "s-home"}, Issue: crossHome, At: r.a.now},
				state.Record{Session: state.Party{Name: "worker-foreign", Session: "s-foreign"}, Issue: crossForeign, At: r.a.now},
			)
			res := nextFree(st, crossOwnerCandidates(snap, r.a.now), sc)
			out, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			return string(out), 0
		case args[0] == agentsName && args[1] == agentStartName:
			brief, err := os.ReadFile(args[3])
			if err != nil {
				t.Fatal(err)
			}
			dispatched = append(dispatched, string(brief))
		}
		return original(args)
	}

	r.pass(t)
	if r.called(loopBoard, "move", filedRef, "backlog", "--intake") == nil {
		t.Fatalf("loop did not intake %s onto the board: calls %v\n%s", filedRef, r.calls, r.out)
	}
	if r.called(loopBoard, "rank", filedRef, "--after", crossHome) == nil {
		t.Fatalf("loop did not rank %s after %s: calls %v\n%s", filedRef, crossHome, r.calls, r.out)
	}
	if r.called("note", loopAdd) != nil {
		t.Fatalf("person was asked about filed issue: calls %v", r.calls)
	}
	if *busy != 1 || len(dispatched) != 1 || !strings.Contains(dispatched[0], filedURL) {
		t.Fatalf("board next did not offer ranked cross-owner issue %s ahead of bottom backlog: busy %d, briefs %v\n%s", filedURL, *busy, dispatched, r.out)
	}
}
