package cmd

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/brieflint"
	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const continuationOldSession = "continuation-old-session"
const continuationNewSession = "continuation-new-session"
const continuationTaskRef = "o/r#12"

func TestParkWakeQueuesFreshContinuationWithoutHarness(t *testing.T) {
	for _, harness := range []string{claudeComm, config.CodexHarness, capacity.HarnessAgy} {
		t.Run(harness, func(t *testing.T) {
			w := parkingWatch(t, nil)
			if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Harness = harness
				st.Agents[0].Task = recoveryTask
				st.Agents[0].Park = &state.Park{At: w.now, On: "#3", Waits: "answer"}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := w.wakeAgent(context.Background(), state.Party{Name: resumer}, agentOne, parkAnswer, ""); err != nil {
					t.Fatal(err)
				}
			}
			ag := agentOf(t, w)
			if ag.Park == nil || len(ag.Park.Continuation) != 1 || !strings.Contains(ag.Park.Continuation[0], parkAnswer) {
				t.Fatalf("event not retained once: %+v", ag.Park)
			}
		})
	}
}

func TestContinuationStartupRetainsEventsOnCapacityRefusal(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	ag := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, On: "#3", Waits: "answer", Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{ag}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == fillCapacityCommand {
			return fillNoRoomJSON, 0
		}
		if args[0] == agentsName && args[1] == agentStartName {
			return "capacity full", ExitRefused
		}
		t.Fatalf("unexpected command %v", args)
		return "", 1
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if err := l.ContinueParks(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	st, _ = r.a.store.Read()
	if len(st.Agents) != 1 || len(st.Agents[0].Park.Continuation) != 1 || st.Agents[0].Park.Continuing {
		t.Fatalf("task lost: %+v", st.Agents)
	}
}

func TestContinuationTransfersOwnershipOnlyAfterAcceptedStart(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask, AssignedAt: r.a.now.Add(-time.Hour),
		Park: &state.Park{At: r.a.now, On: "#3", Continuation: []string{parkAnswer}}}
	next := state.Agent{Party: state.Party{Name: "replacement", Session: continuationNewSession}, Undelivered: "retry delivery"}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{old, next}
		st.Records = []state.Record{{Session: old.Party, Issue: continuationTaskRef}}
		st.Grants = []state.Grant{{Resource: labOne, To: old.Party}}
		st.AlertOwners = []state.AlertOwner{{Alert: "fp", Name: loopWorkloadDown, By: old.Party}}
		st.Merges = []state.Merge{{Repo: repo, PR: 12, By: old.Party}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &loop.Loop{Host: r.a}
	if err := l.FinishContinuation(old, next.Name); err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if len(st.Agents) != 2 {
		t.Fatal("retired before task acceptance")
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[1].Undelivered = ""; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := l.FinishContinuation(old, next.Name); err != nil {
		t.Fatal(err)
	}
	st, _ = r.a.store.Read()
	if len(st.Agents) != 1 || st.Agents[0].Task != old.Task || !st.Records[0].Session.Is(next.Party) || !st.Grants[0].To.Is(next.Party) || !st.AlertOwners[0].By.Is(next.Party) || !st.Merges[0].By.Is(next.Party) {
		t.Fatalf("ownership not handed over: %+v", st.Agents)
	}
}

func TestContinuationRefusesNewEventsDuringStartup(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: w.now, Continuing: true}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err := w.queueContinuation(state.Party{Name: resumer}, agentOf(t, w), parkAnswer)
	if err == nil || !strings.Contains(err.Error(), "retry event delivery") {
		t.Fatalf("event acknowledged without delivery: %v", err)
	}
}

func TestReviewWaitIgnoresLegacyMechanicalTimers(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: w.now, On: loopPR, ReviewHead: loopHead, Waits: deskReviewWait}
		recordAgentReviewPark(st, st.Agents[0])
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.wakeTimer(context.Background(), timerFire{t: state.Timer{ID: 1, Wake: agentOne, When: "pr-checks " + loopPR}, reason: "fixture condition settled"})
	if len(*woke) != 0 || len(agentOf(t, w).Park.Continuation) != 0 {
		t.Fatal("mechanical review event used a worker turn")
	}
}

func TestStoppedExecutionRetainsItsPendingContinuation(t *testing.T) {
	a, r := executionTestApp(t)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: a.now, Continuation: []string{parkAnswer}}
		st.Starts[0].Execution.Observation.StopAcknowledged = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if why := stoppedExecutionWhy(st, st.Agents[0]); why != "" {
		t.Fatalf("%s lost its queued task: %s", r.Name, why)
	}
}

func TestContinuationStartsFreshOnceWithTaskAndEvent(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, On: "#3", Waits: noteDecision, Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{old}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	starts := 0
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			starts++
			if slices.Contains(args, "--"+loop.LoopHostFlag) {
				t.Fatal("continuation changed its managed publish branch")
			}
			brief, err := readStateFixture(r.a.store.Dir(), args[3])
			if err != nil || !strings.Contains(string(brief), old.Task) || !strings.Contains(string(brief), parkAnswer) || !slices.Contains(args, capacity.HarnessAuto) {
				t.Fatalf("continuation brief: %s; %v", brief, err)
			}
			r.started(t, args[2], continuationNewSession)
		}
		return "", 0
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, _ := r.a.store.Read()
		if err := l.ContinueParks(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := r.a.store.Read()
	if starts != 1 || len(st.Agents) != 1 || st.Agents[0].Session != continuationNewSession || st.Agents[0].Task != old.Task {
		t.Fatalf("takeover: starts=%d agents=%+v", starts, st.Agents)
	}
}

func TestLateWakeFollowsFreshContinuationInsteadOfOldTranscript(t *testing.T) {
	old := state.Party{Name: "previous worker", Session: continuationOldSession}
	next := state.Party{Name: "current worker", Session: continuationNewSession}
	st := &state.State{Starts: []state.Start{{Party: old, Successor: &next}}, Agents: []state.Agent{{Party: next, Task: recoveryTask}}}
	a := &app{}
	for _, q := range []string{old.Name, old.Session} {
		ag, rostered, err := a.wakeParty(st, q)
		if err != nil || !rostered || !ag.Is(next) {
			t.Fatalf("late wake reached an old transcript: %+v %v", ag, err)
		}
	}
	st.Agents = nil
	st.Starts = append(st.Starts, state.Start{Party: next, Successor: &old})
	if _, _, err := a.wakeParty(st, old.Session); err == nil {
		t.Fatal("successor cycle resumed a transcript")
	}
}

func TestLoopReconcilesQueuedContinuationAfterRestart(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Record(event(loop.LoopParty, continuationQueuedEvent, "%s: findings retained", loopWorker)); err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	_, done, err := l.Reconcile(context.Background(), st, superloop.Action{Kind: superloop.KindRed}, loop.LoopClaim{Target: loopWorker, At: r.a.now.Add(-time.Minute)})
	if err != nil || !done {
		t.Fatalf("queued delivery replayed after restart: %v, %v", done, err)
	}
}

func TestContinuationWithoutChoicesUsesGatedDefaultStart(t *testing.T) {
	r, _ := fillRig(t)
	r.a.cfg.Capacity.Choices = nil
	r.a.cfg.LeaseDir = t.TempDir()
	old := state.Agent{Party: state.Party{Name: holderParked, Session: continuationOldSession}, Task: recoveryTask,
		Park: &state.Park{At: r.a.now, Continuation: []string{parkAnswer}}}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents = []state.Agent{old}; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	started := false
	r.answer = func(args []string) (string, int) {
		if args[0] == agentsName && args[1] == agentStartName {
			if slices.Contains(args, capacity.HarnessAuto) {
				t.Fatal("forced an unavailable choice pool")
			}
			started = true
			r.started(t, args[2], continuationNewSession)
		}
		return "", 0
	}
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if err := l.ContinueParks(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("legacy desk lost its fresh continuation")
	}
}

func TestAlertWakeAndEndedParkCandidatePreserveQueuedContinuation(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	ag := state.Agent{
		Party: state.Party{Name: loopWorker, HostSession: "codex_queued"},
		Task:  parkTask,
		Park:  &state.Park{At: r.a.now, On: "alert:WorkloadDown", Resumable: r.a.now, Continuation: []string{"alert fired"}},
	}
	if endedParkCandidate(ag) {
		t.Fatal("park with queued continuation should not be recovered as an ended park candidate")
	}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{ag}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.SettleAlertWake(superloop.Action{Kind: loop.LoopAlertKind, Key: "alert wake fp " + loopWorker, Agent: loopWorker}, ag); err != nil {
		t.Fatal(err)
	}
	st, _ := r.a.store.Read()
	if st.Agents[0].Park == nil || len(st.Agents[0].Park.Continuation) != 1 {
		t.Fatalf("settleAlertWake cleared queued continuation: %+v", st.Agents[0].Park)
	}
}

// Exercise generated briefs through the real lint and duplicate-PR gate, not
// just a fake successful start. Both timer and message delivery must replace
// the owner of an existing PR without turning its previous transcript.
func TestContinuationOfOpenPullPassesRealStartGates(t *testing.T) {
	for _, fromTimer := range []bool{false, true} {
		t.Run(fmt.Sprint("timer=", fromTimer), func(t *testing.T) {
			r, _ := fillRig(t)
			r.a.cfg.LeaseDir = t.TempDir()
			party := state.Party{Name: holderParked, Session: continuationOldSession}
			old := state.Agent{Party: party, Task: "Continue https://github.com/o/r/issues/12",
				Park: &state.Park{By: party, At: r.a.now, On: loopPR, RefKind: github.RefPullRequest, Waits: "timer wakes me to verify proof, then close"}}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{old}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			const message = "Guide run 32: verify #888.\n```\nSee https://github.com/other/repo/issues/99\n```"
			if fromTimer {
				w := &watcher{app: r.a}
				w.wakeTimer(t.Context(), timerFire{t: state.Timer{ID: 42, By: party, Wake: party.Name, Due: r.a.now, What: message}, reason: paramDue})
			} else if err := r.a.queueContinuation(loop.LoopParty, old, message); err != nil {
				t.Fatal(err)
			}
			previousRead := readRepoOpenPulls
			t.Cleanup(func() { readRepoOpenPulls = previousRead })
			readRepoOpenPulls = func(context.Context, string) ([]unmergedPull, error) {
				return []unmergedPull{{Number: 7, URL: "https://github.com/o/r/pull/7", Body: "Closes https://github.com/o/r/issues/12"}}, nil
			}
			starts := 0
			r.answer = func(args []string) (string, int) {
				if args[0] != agentsName || args[1] != agentStartName {
					return "", 0
				}
				starts++
				brief, err := readStateFixture(r.a.store.Dir(), args[3])
				if err != nil {
					t.Fatal(err)
				}
				findings, err := brieflint.Lint(args[3], bytes.NewReader(brief))
				if err != nil || len(findings) != 0 || !strings.Contains(string(brief), message) {
					t.Fatalf("quoted wake did not survive lint: %+v, %v, %s", findings, err, brief)
				}
				issues := briefIssues(string(brief))
				if slices.Contains(issues, "other/repo#99") {
					t.Fatal("quoted event assigned unrelated work")
				}
				previousAs := r.a.as
				r.a.as = "unrelated worker"
				if err := r.a.openPullStartGate(t.Context(), args[2], string(brief), issues, false, false, false); err == nil {
					t.Fatal("non-loop caller inherited continuation exemption")
				}
				r.a.as = loop.LoopParty.Name
				if err := r.a.openPullStartGate(t.Context(), "wrong successor", string(brief), issues, false, false, false); err == nil {
					t.Fatal("wrong successor inherited continuation exemption")
				}
				err = r.a.openPullStartGate(t.Context(), args[2], string(brief), issues, false, false, false)
				r.a.as = previousAs
				if err != nil {
					t.Fatalf("continuation refused by real PR gate: %v", err)
				}
				r.started(t, args[2], continuationNewSession)
				return "", 0
			}
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				st, _ := r.a.store.Read()
				if err := l.ContinueParks(t.Context(), st); err != nil {
					t.Fatal(err)
				}
			}
			st, _ := r.a.store.Read()
			if starts != 1 || len(st.Agents) != 1 || st.Agents[0].Session != continuationNewSession {
				t.Fatalf("continuation not started once: %d, %+v", starts, st.Agents)
			}
			// The exemption must not authorize an ordinary second worker.
			if err := r.a.openPullStartGate(t.Context(), "competing worker", old.Task, []string{continuationTaskRef}, false, false, false); err == nil {
				t.Fatal("ordinary duplicate worker was admitted")
			}
		})
	}
}

// The task a post-merge continuation continues: its issue, the pull request
// that is only "Part of" it, and that pull request's reviewed head.
const (
	continuedIssue = "example/app#1"
	continuedPull  = "example/app#2"
	continuedHead  = "c0ffee5eed"
)

// The loop's fresh continuation of a worker parked on a merged pull request
// that only says "Part of" its issue continues the task's delivery: its
// start keeps the merged pull, and its idle --done is accepted.
func TestContinuationStartKeepsTheMergedPullOfItsDelivery(t *testing.T) {
	work := "prove the release"
	for _, tc := range []struct {
		name     string
		pulls    []string
		next     func(prev state.Agent) string
		accepted bool
	}{
		{name: "continuation of a released merge", pulls: []string{continuedPull}, next: continuationName, accepted: true},
		{name: "continuation whose delivery lacks the parked pull", next: continuationName, accepted: true},
		{name: "another start of the issue", pulls: []string{continuedPull}, next: func(state.Agent) string { return "app 1 again" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := stubApp(t)
			parked := a.now.Add(-time.Hour)
			prev := state.Agent{Party: state.Party{Session: continuationOldSession, Name: "app 1"}, Task: "Work https://github.com/example/app/issues/1", AssignedAt: parked.Add(-time.Hour),
				Park: &state.Park{On: continuedPull, RefKind: github.RefPullRequest, ReviewHead: continuedHead, PostMerge: &work, At: parked, Continuing: true}}
			st := &state.State{
				Agents:  []state.Agent{prev},
				Records: []state.Record{{Session: prev.Party, Issue: continuedIssue, At: prev.AssignedAt}},
				Deliveries: []state.Delivery{{Ref: continuedIssue, Pulls: slices.Clone(tc.pulls), Step: state.DeliveryDone, Outcome: state.OutcomeMerged,
					Evidence: "https://github.com/example/app/pull/2", Agent: prev.Name, Started: prev.AssignedAt, Since: parked}},
			}
			next := state.Party{Session: continuationNewSession, Name: tc.next(prev)}
			if _, err := recordStart(st, state.Start{Party: next, At: a.now}, "Continue the existing task", continuedIssue, func(p state.Party) bool { return p.Is(next) }); err != nil {
				t.Fatal(err)
			}
			ag := st.Agents[len(st.Agents)-1]
			lookup := func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
				return map[github.PR]github.RefState{}, nil
			}
			_, err := a.completionOutcomeWith(t.Context(), st, ag, true, "", false, "", lookup)
			d := st.Deliveries[0]
			if !tc.accepted {
				if err == nil || len(d.Pulls) != 0 || d.Step != state.DeliveryImplementing {
					t.Fatalf("a start that continues nothing kept the ended delivery: %+v, %v", d, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("continuation's idle --done refused: %v (delivery %+v)", err, d)
			}
			if !slices.Equal(d.Pulls, []string{continuedPull}) || d.Step != state.DeliveryDone || d.Outcome != state.OutcomeMerged || d.Agent != next.Name || !d.Started.Equal(prev.AssignedAt) {
				t.Fatalf("continuation lost its delivery: %+v", d)
			}
		})
	}
}

// A fixer's review park, its worker off the roster, has post-merge work. Once
// its pull request merged, the loop starts a fresh session for that work
// (StartFixer's end start). That session continues the served issue's
// delivery: the delivery keeps the merged pull request, or gains it, so the
// session's idle --done is accepted without --no-change.
func TestReviewParkContinuationKeepsTheMergedPullOfItsDelivery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery state.Delivery
	}{
		{name: "released merge", delivery: state.Delivery{Pulls: []string{loopPR}, Step: state.DeliveryDone, Outcome: state.OutcomeMerged, Evidence: loopURL}},
		{name: "delivery the park alone links to the pull", delivery: state.Delivery{Pulls: []string{}, Step: state.DeliveryImplementing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			now := r.a.now
			work := "prove the change on main"
			started := now.Add(-2 * time.Hour)
			d := tc.delivery
			d.Ref, d.Started, d.Since = continuationTaskRef, started, now.Add(-time.Minute)
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.ReviewParks = []state.ReviewPark{{On: loopPR, At: now.Add(-time.Minute), ReviewHead: loopHead, Agent: loopWorker,
					Party: state.Party{Name: loopWorker, Session: continuationOldSession}, Serves: continuationTaskRef, PostMerge: &work}}
				st.Deliveries = []state.Delivery{d}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			next := state.Party{Name: loopWorker, Session: continuationNewSession}
			var startErr error
			r.answer = func(args []string) (string, int) {
				switch {
				case args[0] == loop.LoopCapacity:
					return loopRoomJSON, 0
				case len(args) > 3 && args[0] == agentsName && args[1] == agentStartName:
					// The start reads its brief file and registers its
					// session as agents start does.
					brief, err := readBrief(args[3])
					if err != nil {
						startErr = err
						break
					}
					startErr = r.a.store.Update(func(st *state.State) ([]state.Event, error) {
						_, err := recordStart(st, state.Start{Party: next, At: now}, briefTask(brief), strings.Join(briefIssues(brief), ","), func(p state.Party) bool { return p.Is(next) })
						return nil, err
					})
				}
				return "", 0
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			l := &loop.Loop{Host: r.a, Said: map[string]bool{}, Merged: map[string]superloop.Pull{loopPR: {MergeCommit: loopFixed}}}
			if _, err := l.EndMerged(t.Context(), st, superloop.Action{Key: "end " + loopPR, Kind: superloop.KindEnd, Ref: loopPR, URL: loopURL, Message: strings.ToLower(superloop.Merged)}); err != nil {
				t.Fatal(err)
			}
			if startErr != nil {
				t.Fatal(startErr)
			}
			if r.called(agentsName, agentStartName, loopWorker) == nil {
				t.Fatalf("no fresh session for the post-merge work: %v\n%s", r.calls, r.out)
			}
			if st, err = r.a.store.Read(); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(next) })
			if i < 0 {
				t.Fatalf("fresh session not on the roster: %+v", st.Agents)
			}
			merged := func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				out := map[github.PR]github.RefState{}
				for _, ref := range refs {
					out[ref] = github.RefState{Kind: github.RefPullRequest, State: github.Merged}
				}
				return out, nil
			}
			_, err = r.a.completionOutcomeWith(t.Context(), st, st.Agents[i], true, "", false, "", merged)
			k := slices.IndexFunc(st.Deliveries, func(x state.Delivery) bool { return x.Ref == continuationTaskRef })
			if k < 0 {
				t.Fatalf("the task's delivery is gone: %+v", st.Deliveries)
			}
			got := st.Deliveries[k]
			if err != nil {
				t.Fatalf("fresh session's idle --done refused: %v (delivery %+v)", err, got)
			}
			if !slices.Equal(got.Pulls, []string{loopPR}) || got.Agent != loopWorker || !got.Started.Equal(started) {
				t.Fatalf("fresh session lost the task's delivery: %+v", got)
			}
		})
	}
}

func TestParkedPullNamesOnlyAPullRequest(t *testing.T) {
	for _, tc := range []struct {
		park *state.Park
		want string
	}{
		{park: &state.Park{On: continuedPull, RefKind: github.RefPullRequest}, want: continuedPull},
		{park: &state.Park{On: "https://github.com/example/app/pull/2", ReviewHead: continuedHead}, want: continuedPull},
		{park: &state.Park{On: continuedIssue, RefKind: github.RefIssue}},
		{park: &state.Park{On: "#7"}},
		{},
	} {
		if got := parkedPull(tc.park); got != tc.want {
			t.Errorf("parkedPull(%+v) = %q, want %q", tc.park, got, tc.want)
		}
	}
}

// The fresh worker the loop starts for a pull request whose worker left the
// roster for the desk review takes that worker's place: the note it filed,
// the timer it set, its serve record and its desk-review park name the
// fresh worker, and a late answer or wake for the old session reaches the
// fresh worker instead of resuming the old transcript beside it.
//
// control: none: takeOverReviewPark is new, so the merge base cannot build this test
func TestFixerTakesOverTheWorkerItsReviewParkLeft(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.LeaseDir = t.TempDir()
	old := state.Party{Name: holderParked, HostSession: codex.HostPrefix + continuationOldSession}
	fixer := state.Agent{Party: state.Party{Name: holderParked, HostSession: codex.HostPrefix + continuationNewSession}, Task: "Fix the findings on https://github.com/o/r/pull/13"}
	rp := state.ReviewPark{On: "o/r#13", ReviewHead: "abc123", Agent: holderParked, Party: old, Serves: continuationTaskRef}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{fixer}
		st.EndedWorkers = []state.Start{{Party: old, Task: recoveryTask}}
		st.ReviewParks = []state.ReviewPark{rp}
		st.Records = []state.Record{{Session: old, Issue: continuationTaskRef}}
		st.Notes = []state.Note{{ID: 7, By: old, Text: "May the desk buy the larger build cache?"}}
		st.Timers = []state.Timer{{ID: 3, Wake: old.HostSession, By: old, What: "look at the cache again"}}
		st.Grants = []state.Grant{{Resource: labOne, To: old}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		return takeOverReviewPark(a, st, rp, holderParked)
	}); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !st.Notes[0].By.Is(fixer.Party):
		t.Errorf("the old session's note stays its own: %+v", st.Notes[0].By)
	case st.Timers[0].Wake != fixer.Name || !st.Timers[0].By.Is(fixer.Party):
		t.Errorf("the old session's timer wakes %q, set by %+v", st.Timers[0].Wake, st.Timers[0].By)
	case len(st.Records) != 1 || !st.Records[0].Session.Is(fixer.Party) || !strings.EqualFold(st.Records[0].Issue, continuationTaskRef):
		t.Errorf("serve records %+v", st.Records)
	case !st.ReviewParks[0].Party.Is(fixer.Party):
		t.Errorf("the desk-review park stays the old session's: %+v", st.ReviewParks[0].Party)
	case !st.Grants[0].To.Is(fixer.Party):
		t.Errorf("the old session's grant stays its own: %+v", st.Grants[0].To)
	}
	for _, q := range []string{old.HostSession, old.Name} {
		ag, rostered, err := a.wakeParty(st, q)
		if err != nil || !rostered || !ag.Is(fixer.Party) {
			t.Errorf("a wake of %q reaches %+v (rostered %v, %v), not the fresh worker", q, ag.Party, rostered, err)
		}
	}
}
