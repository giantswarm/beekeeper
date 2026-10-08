//go:build unix

package cmd

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	candidateA = "v1.2.4-rc.1"
	candidateB = "v1.2.4-rc.2"
)

// promoteArgv is devctl release promote o/r.
func promoteArgv() []string { return []string{merge.Tool, "release", promoteArg, scratchRepo} }

// stubCandidate answers promoteCandidate with *newest.
func stubCandidate(t *testing.T, newest *string) *int {
	t.Helper()
	asked := new(int)
	was := promoteCandidate
	t.Cleanup(func() { promoteCandidate = was })
	promoteCandidate = func(context.Context, string) (string, error) {
		*asked++
		return *newest, nil
	}
	return asked
}

// promotePlace is o/r's waiting promotion in a free lane with a fresh
// budget, queued for candidateA, and the queued run that waits for it.
func promotePlace(t *testing.T) (*app, *gateRun) {
	t.Helper()
	me, gate := state.Party{Session: "s1", Name: ownerName}, sleeper(t)
	a := queueApp(t, state.Merge{Repo: scratchRepo, Lane: scratchRepo, By: me, PID: gate, Phase: state.Waiting,
		Joined: time.Now(), Seen: time.Now(), Candidate: candidateA})
	// The machine's own devctl processes count against the cap.
	a.cfg.Merge.BudgetFresh, a.cfg.Merge.Cap = config.Duration{Duration: time.Hour}, 1000
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Budget = &state.Budget{Remaining: 5000, Limit: 5000, Reset: time.Now().Add(time.Hour), At: time.Now()}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, &gateRun{app: a, ctx: context.Background(), argv: promoteArgv(), repo: scratchRepo, lane: a.cfg.LaneOf(scratchRepo),
		me: me, pid: os.Getpid(), queued: true, placed: true, from: gate}
}

// A promotion records the newest release candidate when it joins its lane.
func TestAQueuedPromotionRecordsItsCandidate(t *testing.T) {
	stubGitHub(t, github.Open, "")
	_, launched := stubSelf(t)
	newest := candidateA
	stubCandidate(t, &newest)
	a := busyLaneApp(t)

	if err := a.gate(context.Background(), promoteArgv(), 0, false); Code(err) != ExitGateQueued {
		t.Fatalf("exit %d (%v), want %d", Code(err), err, ExitGateQueued)
	}
	launched()
	st, _ := a.store.Read()
	i := slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 && m.Phase == state.Waiting })
	if i < 0 || st.Merges[i].Candidate != candidateA {
		t.Fatalf("the promotion's place does not record %s: %+v", candidateA, st.Merges)
	}
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "merge.queued" })
	if len(evs) == 0 || evs[0].Detail != "o/r promote in lane o/r, for candidate "+candidateA {
		t.Errorf("queued events %+v", evs)
	}
}

// A promotion whose turn comes when another candidate is newest promotes
// nothing: it is refused, its place leaves the lane, and the refusal names
// both candidates. With its own candidate still newest, it runs.
func TestAPromotionQueuedForOneCandidateNeverPromotesAnother(t *testing.T) {
	noSystemd(t)
	stubGitHub(t, github.Open, "")
	fakeDevctl(t, `echo '{"repositories":[{"candidate":"`+candidateB+`","state":"dispatched"}]}'`)

	a, g := promotePlace(t)
	newest := candidateB
	stubCandidate(t, &newest)
	why, err := g.step()
	if Code(err) != ExitGateRefused || why != "" {
		t.Fatalf("step: %q, exit %d (%v), want a refusal", why, Code(err), err)
	}
	want := "o/r promote: o/r promote was queued for candidate " + candidateA + ", the newest is candidate " + candidateB + " now: nothing promoted"
	if d := lastEventOf(t, a, "merge.refused"); !strings.HasPrefix(d, want) {
		t.Errorf("refusal %q, want %q…", d, want)
	}
	if st, _ := a.store.Read(); len(st.Merges) != 0 {
		t.Errorf("the refused promotion kept a place: %+v", st.Merges)
	}
	if d := lastEventOf(t, a, verbMerged); d != "" {
		t.Errorf("promoted: %q", d)
	}

	a, g = promotePlace(t)
	newest = candidateA
	if why, err := g.step(); err != nil || why != "" {
		t.Fatalf("its own candidate: %q, %v", why, err)
	}
	if d := lastEventOf(t, a, verbMerged); !strings.HasPrefix(d, "o/r promote exit 0, release v1.2.4") {
		t.Errorf("merged event %q", d)
	}
}

// lanes drop takes a waiting promotion out of its lane, and the gate that
// waited for the place refuses instead of joining again.
func TestLanesDropTakesAPromotionOut(t *testing.T) {
	stubGitHub(t, github.Open, "")
	newest := candidateA
	stubCandidate(t, &newest)
	a, g := promotePlace(t)
	// A merge ahead holds the lane, so the promotion waits.
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = append([]state.Merge{{Repo: scratchRepo, PR: 6, Lane: scratchRepo, By: state.Party{Name: aheadName}, PID: sleeper(t),
			Phase: state.Waiting, Joined: time.Now().Add(-time.Minute), Seen: time.Now()}}, st.Merges...)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if why, err := g.step(); err != nil || !strings.Contains(why, "behind o/r#6") {
		t.Fatalf("step before the drop: %q, %v", why, err)
	}

	var out bytes.Buffer
	a.out = &out
	c := a.lanesCmd()
	c.SetArgs([]string{"drop", scratchRepo, promoteArg})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "dropped o/r promote from its lane\n" {
		t.Errorf("drop said %q", out.String())
	}
	st, _ := a.store.Read()
	if slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 }) {
		t.Fatalf("the promotion kept its place: %+v", st.Merges)
	}

	why, err := g.step()
	if Code(err) != ExitGateRefused || why != "" {
		t.Fatalf("step after the drop: %q, exit %d (%v), want a refusal", why, Code(err), err)
	}
	if d := lastEventOf(t, a, "merge.refused"); !strings.Contains(d, "o/r promote was taken out of lane o/r (beekeeper lanes drop): nothing ran") {
		t.Errorf("refusal %q", d)
	}
	if st, _ := a.store.Read(); slices.ContainsFunc(st.Merges, func(m state.Merge) bool { return m.PR == 0 }) {
		t.Errorf("the gate joined the lane again: %+v", st.Merges)
	}
}

func TestDropArg(t *testing.T) {
	for arg, want := range map[string]int{promoteArg: 0, "7": 7} {
		if got, err := dropArg(arg); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", arg, got, err, want)
		}
	}
	for _, arg := range []string{"0", "-1", "promotion", ""} {
		if _, err := dropArg(arg); err == nil {
			t.Errorf("%q: accepted", arg)
		}
	}
}
