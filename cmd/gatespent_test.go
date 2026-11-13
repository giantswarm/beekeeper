//go:build unix

package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// errGraphQLSpent is gh's answer to a read while the GraphQL budget is spent.
var errGraphQLSpent = errors.New("gh pr view 7: GraphQL: API rate limit already exceeded for user ID 1")

// stubSpentRead makes the pull request's read refused under a spent limit
// until reset, the budget reading GraphQL refused until then (spent) or
// with no limit spent (no reset known); it counts the reads. The fixture
// clock advances explicitly so setup cannot consume the simulated reset.
func stubSpentRead(t *testing.T, now func() time.Time, reset time.Time, spent bool) *int {
	t.Helper()
	asked := stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	read := gate.PullBase
	gate.PullBase = func(ctx context.Context, repo string, n int) (github.PullBase, error) {
		if now().Before(reset) {
			*asked++
			return github.PullBase{}, errGraphQLSpent
		}
		return read(ctx, repo, n)
	}
	wasBudget := readBudget
	t.Cleanup(func() { readBudget = wasBudget })
	readBudget = func(*app, context.Context) (github.Budget, error) {
		b := github.Budget{Remaining: 5000, Limit: 5000, Reset: now().Add(time.Hour), GraphQL: &github.GraphQL{Limit: 5000, Remaining: 4000}}
		if spent && now().Before(reset) {
			b.GraphQL = &github.GraphQL{Refused: "API rate limit already exceeded", Reset: reset}
		}
		return b, nil
	}
	return asked
}

// spentLane is a lane of two places: o/r#7 at its head, its gate this one,
// and o/r#8 queued behind it by another session.
func spentLane(t *testing.T) *app {
	t.Helper()
	behind := state.Merge{Repo: scratchRepo, PR: 8, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: sleeper(t), Phase: state.Waiting,
		Joined: time.Now().Add(-time.Minute), Seen: time.Now()}
	me := state.Merge{Repo: scratchRepo, PR: 7, Lane: scratchRepo, By: state.Party{Session: "s1", Name: ownerName}, PID: os.Getpid(), Phase: state.Waiting,
		Joined: time.Now().Add(-time.Hour), Seen: time.Now()}
	a := queueApp(t, me, behind)
	a.cfg.Merge.BudgetFresh = config.Duration{Duration: time.Minute}
	return a
}

// placesOf are the keys of the lane's places, in queue order.
func placesOf(t *testing.T, a *app) []string {
	t.Helper()
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, m := range st.Merges {
		keys = append(keys, m.Key())
	}
	return keys
}

// With the GraphQL budget spent, the read of whether the base branch has
// auto-release is a wait for the reset, not a refusal: the merge keeps its
// place at the head of its lane, the place behind it stays behind, and at
// the reset it reads again and merges in its original order.
func TestASpentBudgetKeepsTheMergesPlaceUntilTheReset(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	stubGitHub(t, github.Open, "")
	a := spentLane(t)
	reset := a.now.Add(time.Second)
	asked := stubSpentRead(t, func() time.Time { return a.now }, reset, true)
	g := &gate.GateRun{Host: a, Ctx: context.Background(), Argv: mergeArgv(scratchRepo), Repo: scratchRepo, Pr: 7, Lane: a.cfg.LaneOf(scratchRepo),
		Me: state.Party{Session: "s1", Name: ownerName}, Pid: os.Getpid(), Queued: true}
	why, err := g.Step()
	if err != nil || !strings.Contains(why, "until the GitHub budget's reset") || !strings.Contains(why, "keeps its place in lane o/r") {
		t.Fatalf("step: %q, %v; want a wait for the reset", why, err)
	}
	if d := lastEventOf(t, a, "merge.refused"); d != "" {
		t.Errorf("the merge was refused: %q", d)
	}
	st := gateState(t, g)
	if got := placesOf(t, a); strings.Join(got, " ") != "o/r#7 o/r#8" || !st.Merges[0].Reset.Equal(reset.UTC()) || st.Merges[0].Phase != state.Waiting {
		t.Fatalf("places %v (%+v), want o/r#7 waiting for the reset ahead of o/r#8", got, st.Merges)
	}
	// Before the reset the place waits without reading GitHub again.
	reads := *asked
	if why, err := g.Step(); err != nil || !strings.Contains(why, "until the GitHub budget's reset") || *asked != reads {
		t.Errorf("a second step: %q, %v, %d reads; want the wait without a read", why, err, *asked-reads)
	}

	a.now = reset.Add(time.Millisecond)
	if why, err := g.Step(); why != "" || err != nil {
		t.Fatalf("step after reset: %q, %v; want the merge run", why, err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.Contains(d, "o/r#7 exit 0") {
		t.Errorf("merged event %q", d)
	}
}

// A read refused under a rate limit whose reset is unknown refuses the
// merge (77), but keeps its place for merge.seedTTL: the same command, run
// again, rejoins its lane ahead of the place queued behind it, and merges.
func TestARefusedReadKeepsThePlaceForTheSameCommand(t *testing.T) {
	noSystemd(t)
	fakeDevctl(t, `echo '`+mergedDoc+`'`)
	stubGitHub(t, github.Open, "")
	a := spentLane(t)
	reset := a.now.Add(time.Second)
	stubSpentRead(t, func() time.Time { return a.now }, reset, false)
	g := &gate.GateRun{Host: a, Ctx: context.Background(), Argv: mergeArgv(scratchRepo), Repo: scratchRepo, Pr: 7, Lane: a.cfg.LaneOf(scratchRepo),
		Me: state.Party{Session: "s1", Name: ownerName}, Pid: os.Getpid(), Queued: true}
	if _, err := g.Step(); Code(err) != gate.ExitGateRefused {
		t.Fatalf("step: exit %d (%v), want refused", Code(err), err)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "its place in lane o/r is kept for 1h0m0s") {
		t.Errorf("merge.refused event %q, want the place kept", d)
	}
	st := gateState(t, g)
	if got := placesOf(t, a); strings.Join(got, " ") != "o/r#7 o/r#8" || !st.Merges[0].Seeded {
		t.Fatalf("places %v (%+v), want o/r#7's place kept ahead of o/r#8", got, st.Merges)
	}

	a.now = reset.Add(time.Millisecond)
	g = &gate.GateRun{Host: a, Ctx: context.Background(), Argv: mergeArgv(scratchRepo), Repo: scratchRepo, Pr: 7, Lane: a.cfg.LaneOf(scratchRepo),
		Me: state.Party{Session: "s1", Name: ownerName}, Pid: os.Getpid()}
	if why, err := g.Step(); why != "" || err != nil {
		t.Fatalf("the same command again: %q, %v; want it to rejoin at its old position and merge", why, err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.Contains(d, "o/r#7 exit 0") {
		t.Errorf("merged event %q", d)
	}
}

// Each pull request reads its own release tree, even in the same lane pass.
func TestTheReleaseTreeIsReadForEachPullRequest(t *testing.T) {
	stubBaseRelease(t, github.BaseRelease{Base: mainBranch, Auto: true}, nil)
	reads := 0
	gate.ReleaseAt = func(context.Context, string, string, string) (github.BaseRelease, error) {
		reads++
		return github.BaseRelease{Base: mainBranch, Auto: true}, nil
	}
	lane := config.Lane{Name: scratchRepo, Repositories: []string{scratchRepo}}
	g := runningMerge(t, scratchRepo, lane)
	gateApp(g.Host).now = time.Now()
	if err := gateApp(g.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges[0].Joined = gateApp(g.Host).now.Add(-time.Minute)
		st.Merges = append(st.Merges, state.Merge{Repo: scratchRepo, PR: 8, Lane: scratchRepo, Phase: state.Waiting, Joined: gateApp(g.Host).now.Add(-time.Minute)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	readFor := func(pr int) {
		t.Helper()
		g.Pr, g.Release = pr, nil
		if why, err := g.ReadRelease(); why != "" || err != nil || g.Release == nil || !g.Release.Auto {
			t.Fatalf("o/r#%d: %q, %v, %+v", pr, why, err, g.Release)
		}
	}
	readFor(7)
	readFor(8)
	if reads != 2 {
		t.Errorf("the base branch was read %d times for two places of the pass, want twice", reads)
	}

	if err := gateApp(g.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{{Repo: scratchRepo, PR: 9, Lane: scratchRepo, Phase: state.Waiting, Joined: gateApp(g.Host).now.Add(time.Minute)}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	gateApp(g.Host).now = gateApp(g.Host).now.Add(2 * time.Minute)
	readFor(9)
	if reads != 3 {
		t.Errorf("a new pass read the base branch %d times in all, want a third read", reads)
	}
}
