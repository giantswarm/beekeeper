package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	fatalScopeRepo = "example/tool"
	fatalOtherRepo = "example/other"
	fatalScopeBase = "trunk"
)

func saveMainOutage(t *testing.T, a *app) fatal.Queue {
	t.Helper()
	q := fatal.Queue{Incidents: []fatal.Incident{{ID: "main:" + fatalScopeRepo, Owner: agentOne, Attempts: 3}}}
	if err := q.Save(a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestFatalMainBoardStartsInBothRepositories(t *testing.T) {
	for _, repo := range []string{fatalScopeRepo, fatalOtherRepo} {
		t.Run(repo, func(t *testing.T) {
			r, busy := fillRig(t)
			saveMainOutage(t, r.a)
			old := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == loop.LoopBoard {
					return fmt.Sprintf(`{"pick":{"ref":"%s#%d","url":"https://github.com/%s/issues/%d"}}`, repo, *busy+10, repo, *busy+10), 0
				}
				return old(args)
			}
			r.pass(t)
			if *busy != 3 {
				t.Fatalf("red main blocked board starts for %s: %d starts; %s", repo, *busy, r.out)
			}
		})
	}
}

func stubFatalBranches(t *testing.T, base string, readErr error) {
	t.Helper()
	oldBase, oldDefault := gate.PullBase, fatalDefaultBranch
	t.Cleanup(func() { gate.PullBase, fatalDefaultBranch = oldBase, oldDefault })
	gate.PullBase = func(context.Context, string, int) (github.PullBase, error) {
		return github.PullBase{Base: base}, readErr
	}
	fatalDefaultBranch = func(context.Context, string) (string, error) { return fatalScopeBase, nil }
}

func TestFatalPublicationScopeAndRepair(t *testing.T) {
	for _, tc := range []struct {
		name, repo, base string
		pr               int
		repair, blocked  bool
		readErr          error
	}{
		{name: "ordinary merge", repo: fatalScopeRepo, base: fatalScopeBase, pr: 7, blocked: true},
		{name: "repair merge", repo: fatalScopeRepo, base: fatalScopeBase, pr: 7, repair: true},
		{name: "other pull", repo: fatalScopeRepo, base: fatalScopeBase, pr: 8, repair: true, blocked: true},
		{name: "other branch", repo: fatalScopeRepo, base: "release/1.x", pr: 7},
		{name: "other repo", repo: fatalOtherRepo, pr: 7},
		{name: "blocked repository promotion", repo: fatalScopeRepo, blocked: true},
		{name: "repair cannot promote", repo: fatalScopeRepo, repair: true, blocked: true},
		{name: "other promotion", repo: fatalOtherRepo},
		{name: "unreadable base", repo: fatalScopeRepo, pr: 7, readErr: errors.New("unavailable"), blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			saveMainOutage(t, a)
			stubFatalBranches(t, tc.base, tc.readErr)
			if tc.repair {
				if err := a.recordFatalRepair(agentOne, fatalScopeRepo+"#7"); err != nil {
					t.Fatal(err)
				}
			}
			// No roster identity remains: the repair survives retirement and takeover.
			err := a.fatalPublicationBarrier(t.Context(), &state.State{}, tc.repo, tc.pr)
			if (err != nil) != tc.blocked {
				t.Fatalf("blocked=%t: %v", tc.blocked, err)
			}
			if tc.blocked && !strings.Contains(err.Error(), "scope: default-branch merges and releases in "+fatalScopeRepo) {
				t.Fatalf("refusal lacks scope: %v", err)
			}
		})
	}
}

func TestFatalRepairParkPersistsAndStatusShowsScope(t *testing.T) {
	ref := fatalScopeRepo + "#7"
	w := parkingWatch(t, map[string]string{ref: github.Open})
	saveMainOutage(t, w.app)
	old := parkReviewHead
	t.Cleanup(func() { parkReviewHead = old })
	parkReviewHead = func(string) (string, string) { return loopHead, "" }
	if _, err := w.park(state.Party{Name: agentOne}, ref, deskReviewWait); err != nil {
		t.Fatal(err)
	}
	q, err := fatal.Read(w.store.Dir())
	if err != nil || q.Incidents[0].Repair != ref {
		t.Fatalf("repair not saved: %+v %v", q, err)
	}
	var out bytes.Buffer
	c := w.fatalCmd()
	c.SetOut(&out)
	c.SetArgs([]string{fatalStateDirOption, w.store.Dir(), executionStatus})
	if err := c.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"scope":"default-branch merges and releases in `+fatalScopeRepo+`"`) {
		t.Fatalf("status lacks scope: %s", &out)
	}
}

func TestFatalMainLoopWaitsBeforeMergeGo(t *testing.T) {
	r := newLoopRig(t)
	q := saveMainOutage(t, r.a)
	q.Incidents[0].ID = "main:" + loopMergeRepo
	if err := q.Save(r.a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	stubFatalBranches(t, fatalScopeBase, nil)
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	act := superloop.Action{Ref: loopPR, URL: loopURL, Head: loopHead, Key: "fatal merge"}
	if _, err := l.MergePull(t.Context(), &state.State{}, act); !errors.Is(err, loop.ErrLoopLater) {
		t.Fatalf("merge not held: %v", err)
	}
	if len(r.logged(t, loop.LoopGoVerb)) != 0 || r.called(gate.GateCmdName) != nil {
		t.Fatal("red main issued merge go or entered gate")
	}
}

func TestFatalMainRecoveryKeepsUnrelatedOutage(t *testing.T) {
	a, _ := stubApp(t)
	q := saveMainOutage(t, a)
	q.Observe(fatal.Observation{ID: "main:" + fatalOtherRepo, Failed: true}, time.Now())
	q.Observe(fatal.Observation{ID: "main:" + fatalScopeRepo}, time.Now())
	if err := q.Save(a.store.Dir()); err != nil {
		t.Fatal(err)
	}
	stubFatalBranches(t, fatalScopeBase, nil)
	if err := a.fatalPublicationBarrier(t.Context(), nil, fatalScopeRepo, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.fatalPublicationBarrier(t.Context(), nil, fatalOtherRepo, 0); err == nil {
		t.Fatal("recovery released the other repository")
	}
}
