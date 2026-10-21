//go:build unix

package cmd

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

// siblingRepo and siblingKey are the pull request o/r#7 lands with in the
// pair tests.
const (
	siblingRepo = "o/s"
	siblingKey  = siblingRepo + "#7"
	// pairedKey is the merge o/r#7 itself.
	pairedKey = scratchRepo + "#7"
)

// pairedGate is the waiting merge o/r#7 of a queue app, paired with
// o/s#7, and the app's state; merges are the other places.
func pairedGate(t *testing.T, merges ...state.Merge) (*gateRun, *state.State) {
	t.Helper()
	me := state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(),
		Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()}
	a := queueApp(t, append([]state.Merge{me}, merges...)...)
	a.cfg.Merge.Cap, a.cfg.Merge.SessionCap = 5, 2
	g := &gateRun{app: a, ctx: context.Background(), repo: scratchRepo, pr: 7, lane: a.cfg.LaneOf(scratchRepo), me: me.By, pid: os.Getpid()}
	if err := g.declarePair(siblingKey); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return g, st
}

// sibling is o/s#7 waiting in its own lane in a live gate.
func sibling(t *testing.T) state.Merge {
	return state.Merge{Repo: siblingRepo, PR: 7, Lane: siblingRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: sleeper(t),
		Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()}
}

// A paired merge at its turn waits for its sibling to be next in its own
// lane, then takes two cap slots; the sibling, the first started, takes none.
func TestAPairedMergeStartsOnlyWithItsSiblingNext(t *testing.T) {
	g, st := pairedGate(t)
	if why, _ := g.pairTurn(st); !strings.Contains(why, "it lands with o/s#7, which has no place in a lane yet") {
		t.Errorf("without its sibling: %q", why)
	}
	if !g.now.Equal(st.Pairs[0].Ready[pairedKey]) {
		t.Errorf("the merge at its turn is not ready: %+v", st.Pairs[0])
	}

	st.Merges = append(st.Merges, sibling(t))
	if why, _ := g.pairTurn(st); !strings.Contains(why, "it lands with o/s#7, not next in lane o/s yet") {
		t.Errorf("its sibling behind others: %q", why)
	}
	st.Pairs[0].Ready[siblingKey] = g.now
	if why, need := g.pairTurn(st); why != "" || need != 2 {
		t.Errorf("its sibling next: %q, %d slots, want none and 2", why, need)
	}

	st.Pairs[0].Started, st.Pairs[0].Ready = siblingKey, nil
	if why, need := g.pairTurn(st); why != "" || need != 0 {
		t.Errorf("its sibling started: %q, %d slots, want none and 0", why, need)
	}
}

// Both merges of a pair start in one turn: the first once its sibling is
// next, the sibling right after it outside a full merge.cap.
func TestAPairStartsBackToBack(t *testing.T) {
	stubGitHub(t, github.Open, "")
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	sib := sibling(t)
	g, _ := pairedGate(t, sib)
	g.argv, g.now = mergeArgv(scratchRepo), time.Now()
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Pairs[0].Ready = map[string]time.Time{siblingKey: g.now}
		return nil, nil
	})
	if why, err := g.start("", nil); why != "" || err != nil {
		t.Fatalf("the first with its sibling next: %q, %v", why, err)
	}
	st, _ := g.store.Read()
	if len(st.Pairs) != 1 || st.Pairs[0].Started != pairedKey {
		t.Fatalf("pairs %+v", st.Pairs)
	}

	hostDevctl(t, devctlArgv(9, prMerge)...)
	s := &gateRun{app: g.app, ctx: context.Background(), repo: siblingRepo, pr: 7, lane: g.cfg.LaneOf(siblingRepo), me: sib.By, pid: sib.PID, argv: mergeArgv(siblingRepo)}
	_ = s.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{sib}
		return nil, nil
	})
	if why, err := s.start("", nil); why != "" || err != nil {
		t.Fatalf("the sibling after the first started, the cap full: %q, %v", why, err)
	}
	if d := lastEvent(t, s, "merged"); !strings.Contains(d, "o/s#7 exit 0") {
		t.Errorf("merged event %q", d)
	}
}

// The pair's first start needs room under merge.cap and merge.sessionCap
// for both, its sibling's start none.
func TestAPairTakesTwoSlots(t *testing.T) {
	g, st := pairedGate(t)
	me := st.Merges[0]
	hostDevctl(t, devctlArgv(4, prMerge)...)
	if why := g.capWaitFor(st, me, 1); why != "" {
		t.Errorf("one slot of five with four running: %q", why)
	}
	if why := g.capWaitFor(st, me, 2); !strings.Contains(why, "4 merges and promotes run machine-wide (cap 5), it and the merge it lands with need 2 slots") {
		t.Errorf("two slots of five with four running: %q", why)
	}
	hostDevctl(t, devctlArgv(9, prMerge)...)
	if why := g.capWaitFor(st, me, 0); why != "" {
		t.Errorf("the sibling of a started pair waits on the cap: %q", why)
	}

	st.Merges = append(st.Merges, state.Merge{Repo: "o/c", PR: 1, Lane: "o/c", By: me.By, PID: sleeper(t), Phase: state.Running, Started: time.Now()})
	if why := g.sessionWaitFor(st, 1); why != "" {
		t.Errorf("one of two session slots with one running: %q", why)
	}
	if why := g.sessionWaitFor(st, 2); why == "" {
		t.Error("the pair's first starts with one of its party's two slots free")
	}
	if why := g.sessionWaitFor(st, 0); why != "" {
		t.Errorf("the sibling of a started pair waits on the session cap: %q", why)
	}
}

// A refusal of either merge refuses the other with the same reason: the
// queued run past its wait records it, the sibling's gate refuses with it
// and ends the pair.
func TestARefusalOfEitherRefusesBoth(t *testing.T) {
	stubGitHub(t, github.Open, "")
	busy, queued := sleeper(t), sleeper(t)
	a := queueApp(t,
		state.Merge{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: busy, Phase: state.Waiting, Joined: time.Now().Add(-time.Hour), Seen: time.Now()},
		state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: queued, Phase: state.Waiting, Joined: time.Now(), Seen: time.Now()})
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Pairs = []state.Pair{{Keys: []string{pairedKey, siblingKey}, At: time.Now()}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(gateFromEnv, strconv.Itoa(queued))
	if err := a.gate(context.Background(), mergeArgv(scratchRepo), 0, 0, true); Code(err) != ExitGateRefused {
		t.Fatalf("the queued run past its wait: exit %d (%v), want %d", Code(err), err, ExitGateRefused)
	}
	st, _ := a.store.Read()
	if len(st.Pairs) != 1 || st.Pairs[0].RefusedKey != pairedKey || !strings.Contains(st.Pairs[0].Refused, "waited 0s for its turn") {
		t.Fatalf("pairs %+v", st.Pairs)
	}

	var err error
	stderr := gateStderr(t, func() {
		err = a.gate(context.Background(), mergeArgv(siblingRepo), 0, 0, false)
	})
	if Code(err) != ExitGateRefused {
		t.Fatalf("the sibling: exit %d (%v), want %d; stderr %s", Code(err), err, ExitGateRefused, stderr)
	}
	if !strings.Contains(stderr, "o/s#7 lands with o/r#7, which was refused: waited 0s for its turn") {
		t.Errorf("stderr %q", stderr)
	}
	st, _ = a.store.Read()
	if len(st.Pairs) != 0 || slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.Key() == siblingKey }) {
		t.Errorf("the refused pair stays: pairs %+v, merges %+v", st.Pairs, st.Merges)
	}
}

// --pair is the gate's: the queued run's devctl never sees it, and the pair
// is recorded before the merge waits on.
func TestAPairedMergeQueuesWithoutTheFlag(t *testing.T) {
	stubGitHub(t, github.Open, "")
	_, launched := stubSelf(t)
	a := busyLaneApp(t)
	if err := a.gate(context.Background(), mergeArgv(scratchRepo, merge.PairFlag, siblingKey), 0, 0, false); Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	if spec := launched(); slices.Contains(spec.Argv, merge.PairFlag) || !slices.Equal(spec.Command, mergeArgv(scratchRepo)) {
		t.Errorf("argv %q, command %q, want neither with %s", spec.Argv, spec.Command, merge.PairFlag)
	}
	st, _ := a.store.Read()
	if len(st.Pairs) != 1 || st.Pairs[0].Sibling(pairedKey) != siblingKey {
		t.Errorf("pairs %+v", st.Pairs)
	}
	if lines := pairLines(st); lines[pairedKey] != "lands with o/s#7 (no place in a lane yet)" || lines[siblingKey] != "lands with o/r#7 (position 2 in lane o/r)" {
		t.Errorf("pair lines %q", lines)
	}
	if out := a.capture(func() { a.printLanes(a.laneViews(st)) }); !strings.Contains(out, "o/r#7 by \"worker\", waiting since") ||
		!strings.Contains(out, ", lands with o/s#7 (no place in a lane yet)") {
		t.Errorf("lanes %q", out)
	}
}

// A merge pairs with one other at most, never with itself, and a merge
// refused before declares its pair anew.
func TestAPairDeclaration(t *testing.T) {
	g, _ := pairedGate(t)
	if err := g.declarePair(siblingKey); err != nil {
		t.Errorf("the same pair again: %v", err)
	}
	if err := g.declarePair("o/t#4"); Code(err) != ExitGateRefused {
		t.Errorf("a second sibling: %v, want refused", err)
	}
	if err := g.declarePair(pairedKey); Code(err) != ExitGateRefused {
		t.Errorf("itself: %v, want refused", err)
	}
	if err := g.declarePair("nothing"); Code(err) != ExitGateRefused {
		t.Errorf("no pull request: %v, want refused", err)
	}
	_ = g.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Pairs[0].Refused, st.Pairs[0].RefusedKey = "a reviewer", pairedKey
		return nil, nil
	})
	if err := g.declarePair("https://github.com/o/s/pull/7"); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.store.Read(); len(st.Pairs) != 1 || st.Pairs[0].Refused != "" {
		t.Errorf("the rerun keeps its refusal: %+v", st.Pairs)
	}
}
