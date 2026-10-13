//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// heldApp is queueApp at the devctl cap, its budget fresh: a merge whose
// turn came says so ("devctl processes run machine-wide") instead of
// running devctl.
func heldApp(t *testing.T, merges ...state.Merge) *app {
	t.Helper()
	stubGitHub(t, github.Open, "")
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	a := queueApp(t, merges...)
	a.cfg.Merge.BudgetFresh = config.Duration{Duration: time.Hour}
	setBudget(t, a, 4000)
	hostDevctls(t, a.cfg.Merge.Cap)
	return a
}

// placedGate is the queued run of o/r#pr, which has its place in the lane.
func placedGate(a *app, pr int, pid int) *gateRun {
	return &gateRun{app: a, ctx: context.Background(), argv: mergeArgv(scratchRepo, strconv.Itoa(pr)), repo: scratchRepo, pr: pr,
		lane: a.cfg.LaneOf(scratchRepo), me: state.Party{Session: "s1", Name: ownerName}, pid: pid, queued: true, placed: true}
}

func setHold(t *testing.T, a *app, h ...state.Hold) {
	t.Helper()
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = h
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, a *app) *state.State {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mergeOf(st *state.State, pr int) (state.Merge, bool) {
	i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Repo == scratchRepo && m.PR == pr })
	if i < 0 {
		return state.Merge{}, false
	}
	return st.Merges[i], true
}

// A merge queued before a hold that lets a later pull request through keeps
// its place and does not run: the exception is the lane's next. Once the
// exception settled and the hold is gone, the queued merge runs from the
// place it joined with, never queued anew. A new merge during the hold is
// still refused.
func TestAQueuedMergeWaitsForAHoldsExceptionAndKeepsItsPlace(t *testing.T) {
	joined := time.Now().Add(-10 * time.Minute).UTC()
	other := sleeper(t)
	a := heldApp(t,
		state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(),
			Phase: state.Waiting, Joined: joined, Seen: time.Now()},
		state.Merge{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: other,
			Phase: state.Waiting, Joined: joined.Add(time.Minute), Seen: time.Now()})
	setHold(t, a, state.Hold{Target: scratchRepo, Except: scratchRepo + "#6", By: state.Party{Name: "supervisor"}, At: time.Now(), Reason: "#6 first"})

	held := placedGate(a, 7, os.Getpid())
	why, err := held.step()
	if err != nil || !strings.Contains(why, "held in lane o/r (o/r except o/r#6)") || !strings.Contains(why, "o/r#7 keeps its place") {
		t.Fatalf("o/r#7 under the hold: %q, %v; want a wait in its place", why, err)
	}
	if m, ok := mergeOf(readState(t, a), 7); !ok || m.Phase != state.Waiting || !m.Joined.Equal(joined) {
		t.Fatalf("o/r#7's place: %+v, %v", m, ok)
	}

	exception := placedGate(a, 6, other)
	if why, err := exception.step(); err != nil || !strings.Contains(why, "devctl processes run machine-wide") || !strings.Contains(why, "o/r#6 is next in lane o/r") {
		t.Errorf("o/r#6, the hold's exception: %q, %v; want it the lane's next, ahead of the held o/r#7", why, err)
	}

	var refused error
	fresh := gateStderr(t, func() {
		refused = a.gate(context.Background(), mergeArgv(scratchRepo, "8"), time.Minute, 0, false)
	})
	if Code(refused) != ExitGateRefused || !strings.Contains(fresh, "refused, o/r is held") {
		t.Errorf("a new merge during the hold: exit %d, %q; want it refused", Code(refused), fresh)
	}

	// o/r#6 merged and settled; the hold is lifted.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool { return m.PR == 6 })
		st.Holds = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if why, err := held.step(); err != nil || !strings.Contains(why, "o/r#7 is next in lane o/r") {
		t.Errorf("o/r#7 after the hold: %q, %v; want it the lane's next", why, err)
	}
	if m, ok := mergeOf(readState(t, a), 7); !ok || !m.Joined.Equal(joined) {
		t.Errorf("o/r#7 was queued anew: %+v, %v", m, ok)
	}
	if d := lastEventOf(t, a, "merge.queued"); strings.Contains(d, "o/r#7") {
		t.Errorf("o/r#7 queued anew: %q", d)
	}
}

// A hold that lets another pull request through, set while a merge's devctl
// waits for CI, ends that devctl before it merges, and the merge goes back
// to its place in the lane. One GitHub reports merged all the same is
// recorded as any other run.
func TestAHoldsExceptionTakesARunningMergeBackToItsPlace(t *testing.T) {
	joined := time.Now().Add(-10 * time.Minute).UTC()
	child := sleeper(t)
	a := heldApp(t, state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(),
		Child: child, Phase: state.Running, Joined: joined, Seen: time.Now(), Started: time.Now().Add(-time.Minute)})
	base := mergeBase(a.store.Dir(), scratchRepo, 7)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".pid", []byte(strconv.Itoa(child)), 0o600); err != nil {
		t.Fatal(err)
	}
	var ended []int
	was := endChild
	endChild = func(pid int) error { ended = append(ended, pid); return nil }
	t.Cleanup(func() { endChild = was })
	g := placedGate(a, 7, os.Getpid())
	g.queued = false

	if _, ok := g.preemptChild(); ok || len(ended) != 0 {
		t.Fatalf("no hold: ended %v", ended)
	}
	setHold(t, a, state.Hold{Target: scratchRepo, Except: scratchRepo + "#6", By: state.Party{Name: "supervisor"}, At: time.Now(), Reason: "#6 first"})
	h, ok := g.preemptChild()
	if !ok || !slices.Equal(ended, []int{child}) {
		t.Fatalf("the hold's exception: preempted %v, ended %v; want o/r#7's devctl ended", ok, ended)
	}
	var p *preemptedError
	if err := g.requeue(base, h); !errors.As(err, &p) || !strings.Contains(p.why, "o/r#6 goes first; o/r#7 keeps its place") || !g.requeued {
		t.Fatalf("requeue: %v", err)
	}
	m, ok := mergeOf(readState(t, a), 7)
	if !ok || m.Phase != state.Waiting || !m.Joined.Equal(joined) || m.Child != 0 || !m.Started.IsZero() {
		t.Errorf("o/r#7 after its devctl ended: %+v, %v; want it waiting in its place", m, ok)
	}

	// devctl merged before it ended: the run is the record's.
	stubGitHub(t, github.Merged, "")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Phase, st.Merges[0].Started = state.Running, time.Now()
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.requeue(base, h); err != nil {
		t.Errorf("a merged run was requeued: %v", err)
	}
}
