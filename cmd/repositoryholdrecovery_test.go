package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// The tests of https://github.com/giantswarm/beekeeper/issues/3364: a
// repository hold stops every start the loop makes there, its recovery
// restarts too, and supervisor.ownership.never stops the go as well.

const (
	recoveryHoldRepo   = "o/r"
	recoveryHoldReason = "the repository is being migrated"
	recoveryHeldWorker = "held worker"
	// recoveryHeldWait starts what a start's wait says of a repository hold.
	recoveryHeldWait = "held: repository "
)

// holdRecoveryRepo holds repo as `beekeeper hold set <repo>` does.
func holdRecoveryRepo(t *testing.T, store state.Store, repo string) {
	t.Helper()
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = append(st.Holds, state.Hold{Target: repo, Reason: recoveryHoldReason, By: state.Party{Name: loopPerson}, At: time.Now()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// liftRecoveryHolds lifts every hold, as `beekeeper hold lift` does.
func liftRecoveryHolds(t *testing.T, store state.Store) {
	t.Helper()
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A quota-stopped agy worker's replacement is a start in its repository: a
// hold there keeps the entry, its records and its quota marker, says the
// hold, and the first pass after the lift starts the replacement.
func TestAgyQuotaReplacementWaitsForTheRepositoryHold(t *testing.T) {
	const repo = "giantswarm/beekeeper"
	f := newQuotaFixture(t, quotaTestWorker, quotaTestTask, repo+"#2150")
	holdRecoveryRepo(t, f.store, repo)
	var out bytes.Buffer
	var runCalls [][]string
	l := f.loop(t, &out, func(args []string) (string, int, error) {
		runCalls = append(runCalls, args)
		return "started replacement\n", 0, nil
	})
	st, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RecoverQuota(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if err := loopRoster(l, context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(runCalls) != 0 {
		t.Fatalf("a held repository's quota replacement started: %v", runCalls)
	}
	if want := recoveryHeldWait + repo + ": " + recoveryHoldReason; !strings.Contains(out.String(), want) || strings.Contains(out.String(), "re-queuing") {
		t.Fatalf("output = %q, want the wait naming %q and no re-queue", out.String(), want)
	}
	after, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Agents) != 1 || !after.Agents[0].Is(f.party) || agyQuotaError(f.cfg, after.Agents[0]) == nil {
		t.Fatalf("agents = %+v, want the quota-stopped entry and its marker kept", after.Agents)
	}

	liftRecoveryHolds(t, f.store)
	if after, err = f.store.Read(); err != nil {
		t.Fatal(err)
	}
	if err := l.RecoverQuota(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	if len(runCalls) != 1 || !slices.Contains(runCalls[0], agentStartName) || !slices.Contains(runCalls[0], quotaTestWorker) {
		t.Fatalf("after the lift runCalls = %v, want the replacement's start", runCalls)
	}
}

// A parked worker whose session is gone is restarted by a fresh start: a
// hold on its repository keeps it parked on its task, and the first pass
// after the lift restarts it.
func TestLoopRecoveryWaitsForTheRepositoryHold(t *testing.T) {
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	stubGonePark(t)
	var live []*claude.Session
	stubFresh(t, &live)
	ag := state.Agent{Party: state.Party{Name: recoveryHeldWorker, HostSession: "codex_held-session"}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, Resumable: r.a.now, Answer: "the note was answered"}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = append(st.Agents, ag); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	holdRecoveryRepo(t, r.a.store, recoveryHoldRepo)
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	recoverPass := func() *state.State {
		t.Helper()
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if err := l.RecoverParks(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		if st, err = r.a.store.Read(); err != nil {
			t.Fatal(err)
		}
		return st
	}
	held := recoverPass()
	if c := r.called(agentsName, agentStartName); c != nil {
		t.Fatalf("a held repository's parked worker restarted: %v", c)
	}
	if len(held.Agents) != 1 || held.Agents[0].Park == nil || held.Agents[0].Task != ag.Task {
		t.Fatalf("the held restart lost its task or park: %+v", held.Agents)
	}
	if want := recoveryHeldWait + recoveryHoldRepo + ": " + recoveryHoldReason; !strings.Contains(r.out.String(), want) {
		t.Fatalf("the wait does not name the hold %q:\n%s", want, r.out)
	}

	liftRecoveryHolds(t, r.a.store)
	lifted := recoverPass()
	if c := r.called(agentsName, agentStartName, ag.Name); c == nil {
		t.Fatalf("the lift did not restart the worker: %v\n%s", r.calls, r.out)
	}
	if len(lifted.Agents) != 1 || lifted.Agents[0].Park != nil {
		t.Fatalf("after the lift: %+v", lifted.Agents)
	}
}

// An unresumable worker parked on an alert is replaced by a fresh start on
// its original task: a hold on that task's repository keeps the old entry
// and the pending replacement, and the first pass after the lift staffs it.
func TestLoopAlertReplacementWaitsForTheRepositoryHold(t *testing.T) {
	r := newLoopRig(t)
	f := loopFiring()
	readLoopAlerts = func(context.Context, *app) ([]alerts.Firing, error) { return []alerts.Firing{f}, nil }
	loopResumable = func(*app, *state.State, state.Agent) bool { return false }
	r.park(t, loop.AlertParkPrefix+f.Name(), time.Now())
	holdRecoveryRepo(t, r.a.store, recoveryHoldRepo)
	for range 2 {
		r.pass(t)
		if r.called(agentsName, agentStartName) != nil || r.called(agentsName, loop.LoopRemove) != nil {
			t.Fatalf("a held repository's alert replacement ran: %v", r.calls)
		}
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != loopWorker || st.Agents[0].Park == nil {
		t.Fatalf("the held replacement retired its worker: %+v", st.Agents)
	}
	if want := recoveryHeldWait + recoveryHoldRepo + ": " + recoveryHoldReason; !strings.Contains(r.out.String(), want) {
		t.Fatalf("the wait does not name the hold %q:\n%s", want, r.out)
	}

	liftRecoveryHolds(t, r.a.store)
	r.pass(t)
	if r.called(agentsName, loop.LoopRemove, loopWorker) == nil || r.called(agentsName, agentStartName, loopWorker) == nil {
		t.Fatalf("the lift did not staff the pending replacement: %v\n%s", r.calls, r.out)
	}
}

// supervisor.ownership.never stops the go at the merge executor too: a
// decision that reaches it for such a repository merges nothing and waits,
// naming why, without reading the pull request or entering the gate.
func TestLoopMergeExecutorMergesNothingInANeverRepository(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.Ownership.Never = []string{recoveryHoldRepo}
	l := &loop.Loop{Host: r.a, Said: map[string]bool{}}
	act := superloop.Action{Kind: superloop.KindGo, Key: superloop.KindGo + " " + loopPR + "@" + loopHead, Ref: loopPR, URL: loopURL, Head: loopHead}
	_, err := l.MergePull(t.Context(), &state.State{}, act)
	if !errors.Is(err, loop.ErrLoopLater) || !strings.Contains(fmtErr(err), "supervisor.ownership.never") {
		t.Fatalf("a never repository's go was not held: %v", err)
	}
	r.mu.Lock()
	cmds := slices.Clone(r.cmds)
	r.mu.Unlock()
	if r.called(gate.GateCmdName) != nil || len(cmds) != 0 {
		t.Fatalf("a never repository's go reached the merge: calls %v, commands %v", r.calls, cmds)
	}
}

// fmtErr is err's text, "" for none.
func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
