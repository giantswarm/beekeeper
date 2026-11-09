package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/board"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestEpicOwnerAndThreeParallelChildren(t *testing.T) {
	now := time.Now().UTC()
	epic := board.Item{Ref: "o/r#1", URL: "https://github.com/o/r/issues/1", OnBoard: true, Kind: "Epic", Status: "In Progress", Updated: now, OpenSubIssues: 4}
	for n := 2; n <= 5; n++ {
		epic.SubIssues = append(epic.SubIssues, board.Item{Ref: fmt.Sprintf("o/r#%d", n), Updated: now})
	}
	snap := &board.Snapshot{Items: []board.Item{epic}, Gate: board.Gate{Kind: []string{"Epic"}, Go: "In Progress"}, Order: []config.BoardStep{{Name: "active", Status: []string{"In Progress"}, SubIssues: true}}}
	cfg := config.Board{}
	cands, view := epicCandidates(snap, cfg, board.Rank(snap, cfg, now))
	sc := pickScope{epics: view, epicLimit: 1, epicParallel: 3, listed: now, alive: func(state.Party) bool { return false }}
	st := &state.State{}
	pick := nextFree(st, cands, sc).Pick
	if pick == nil || !pick.EpicOwner {
		t.Fatalf("first pick must be the owner: %+v", pick)
	}
	st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: "owner", Name: "owner"}, Task: epicOwnerTask + epic.Ref, Park: &state.Park{On: epic.Ref}})
	for n := 2; n <= 4; n++ {
		pick = nextFree(st, cands, sc).Pick
		want := fmt.Sprintf("o/r#%d", n)
		if pick == nil || pick.Ref != want {
			t.Fatalf("want %s: %+v", want, pick)
		}
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{Session: want, Name: want}, Task: "Work " + want})
		st.Deliveries = append(st.Deliveries, state.Delivery{Ref: want, Step: state.DeliveryImplementing, Since: now, Agent: want})
	}
	if p := nextFree(st, cands, sc).Pick; p != nil {
		t.Fatalf("fourth child admitted at parallel cap: %+v", p)
	}
	st.Agents[1].Done = true
	st.Deliveries[0].Step = state.DeliveryDone
	// Closed children disappear from the next board snapshot. Simulate that here.
	cands = append(cands[:1], cands[2:]...)
	pick = nextFree(st, cands, sc).Pick
	if pick == nil || pick.Ref != "o/r#5" {
		t.Fatalf("fourth child did not start after a merge: %+v", pick)
	}
	second := board.Candidate{Item: board.Item{Ref: "o/r#10"}, EpicOwner: true}
	sc.epics.Epics = append(sc.epics.Epics, epicDelivery{Ref: second.Ref})
	if why := epicAdmission(st, second, sc); !strings.Contains(why, "WIP") {
		t.Fatalf("second epic did not wait: %q", why)
	}
	st.Agents[0].Done = true
	sc.epics.Epics = sc.epics.Epics[1:]
	if why := epicAdmission(st, second, sc); why != "" {
		t.Fatal(why)
	}
}

func TestEpicWithoutChildrenStillOffersAcceptanceOwner(t *testing.T) {
	now := time.Now().UTC()
	snap := &board.Snapshot{Items: []board.Item{{Ref: "o/r#1", OnBoard: true, Kind: "Epic", Status: "In Progress", Updated: now}}, Gate: board.Gate{Kind: []string{"Epic"}, Go: "In Progress"}, Order: []config.BoardStep{{Name: "active", Status: []string{"In Progress"}, SubIssues: true}}}
	got, view := epicCandidates(snap, config.Board{}, nil)
	if len(got) != 1 || !got[0].EpicOwner || len(view.Epics) != 1 {
		t.Fatalf("%+v %+v", got, view)
	}
	snap.Items[0].Status = "Up Next"
	got, view = epicCandidates(snap, config.Board{}, nil)
	if len(got) != 0 || len(view.Epics) != 0 {
		t.Fatal("planning epic admitted")
	}
}

func TestEpicBlockedChildKeepsItsGate(t *testing.T) {
	st := &state.State{Agents: []state.Agent{{Task: epicOwnerTask + "o/r#1"}}}
	c := board.Candidate{Item: board.Item{Ref: "o/r#2"}, Epic: "o/r#1", Skip: "held: blocked by o/r#3"}
	got := nextFreePreview(st, []board.Candidate{c}, pickScope{alive: func(state.Party) bool { return false }})
	if got.Pick != nil || len(got.Skipped) != 1 || got.Skipped[0].Skip != c.Skip {
		t.Fatalf("%+v", got)
	}
}

func TestEpicOwnerWakesOnceAcrossLoopRestarts(t *testing.T) {
	r := newLoopRig(t)
	view := epicSnapshot{At: r.a.timeNow(), Epics: []epicDelivery{{Ref: "o/r#1", Children: []board.Item{{Ref: "o/r#2"}}}}}
	if err := r.a.store.WriteFile(epicSnapshotFile, view); err != nil {
		t.Fatal(err)
	}
	st := &state.State{Agents: []state.Agent{{Party: state.Party{Name: "owner"}, Task: epicOwnerTask + "o/r#1", Park: &state.Park{On: "o/r#1"}}}}
	for range 2 {
		l := &supervisorLoop{a: r.a}
		if err := l.wakeEpicOwners(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, args := range r.calls {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentWakeName {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("wakes: %d, calls: %v", n, r.calls)
	}
	st.Deliveries = []state.Delivery{{Ref: "o/r#2", Step: state.DeliveryDone, Since: r.a.timeNow()}}
	l := &supervisorLoop{a: r.a}
	if err := l.wakeEpicOwners(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("completion did not wake owner: %v", r.calls)
	}
}

func TestEpicCompletionRequiresProofAndNoOpenChildren(t *testing.T) {
	r := newLoopRig(t)
	previous := epicGitHub
	t.Cleanup(func() { epicGitHub = previous })
	open := true
	closed := 0
	epicGitHub = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "api" {
			if open {
				return []byte("1"), nil
			}
			return []byte("0"), nil
		}
		closed++
		return nil, nil
	}
	st := &state.State{WorkerReports: []state.WorkerReport{{Task: epicOwnerTask + "o/r#1", Finished: true, Text: "children merged"}}}
	l := &supervisorLoop{a: r.a}
	if err := l.completeEpics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Fatal("closed without proof")
	}
	st.WorkerReports[0].Text = epicAcceptanceProven + "end-to-end evidence recorded"
	if err := l.completeEpics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Fatal("closed with open children")
	}
	open = false
	for range 2 {
		if err := l.completeEpics(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}
	if closed != 1 {
		t.Fatalf("close attempts: %d", closed)
	}
	if len(r.calls) != 1 || r.calls[0][0] != loopBoard {
		t.Fatalf("Done move: %v", r.calls)
	}
}
