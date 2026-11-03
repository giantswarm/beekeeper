package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopMergeRepo       = "o/r"
	reviewedStoppedCase = "stopped"
	reviewedHeldCase    = "held"
	reviewedRedCase     = "red"
)

func TestLoopMergesReviewedHeadWithoutWorkerWake(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.command = func(argv []string) (string, int) {
		if slices.Contains(argv, superloop.KindMerge) {
			r.pull.State = superloop.Merged
			r.pull.MergeCommit = loopFixed
		}
		return "", 0
	}
	r.pass(t)
	var merged bool
	for _, argv := range r.cmds {
		if len(argv) > 3 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == superloop.KindMerge {
			merged = slices.Contains(argv, "--match-head-commit") && slices.Contains(argv, loopHead)
		}
	}
	if !merged {
		t.Fatalf("loop did not merge the reviewed head: commands %v; worker calls %v", r.cmds, r.calls)
	}
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("mechanical merge woke worker: %v", c)
	}
	r.pass(t)
	if len(r.logged(t, "supervisor.merge")) != 1 {
		t.Fatal("merge decision repeated")
	}
}

func TestLoopMergeRepairAndAdmin(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.command = func(argv []string) (string, int) {
		if !slices.Contains(argv, "--admin") || !slices.Contains(argv, loopHead) {
			t.Fatalf("merge flags: %v", argv)
		}
		return "branch conflicts with base", 1
	}
	r.pass(t)
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], "Repair the branch") {
		t.Fatalf("repair not requested: %v", r.calls)
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("repeated repair wake %v", c)
	}
}

func TestLoopMergeEndDeclaresRemainingWork(t *testing.T) {
	for _, work := range []string{"", "rollout and prove release"} {
		t.Run(work, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Park.PostMerge = &work; return nil, nil }); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			r.command = func([]string) (string, int) {
				r.pull.State = superloop.Merged
				r.pull.MergeCommit = loopFixed
				return "", 0
			}
			r.pass(t)
			r.pass(t)
			wake := r.called(agentsName, agentWakeName, loopWorker)
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if work == "" {
				if wake != nil || len(st.Agents) > 0 && !st.Agents[0].Done {
					t.Fatalf("no-work task was not closed quietly: wake %v, agents %+v", wake, st.Agents)
				}
			} else if wake == nil || !strings.Contains(wake[3], work) {
				t.Fatalf("post-merge work not requested: %v", r.calls)
			}
		})
	}
}

func TestReviewedMergeRefusesUnclaimedMovedRedAndStoppedHeads(t *testing.T) {
	for _, scenario := range []string{"unclaimed", "moved", reviewedRedCase, reviewedStoppedCase, reviewedHeldCase, "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			if scenario != "unclaimed" {
				if err := r.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "conflict":
				r.pull.Mergeable = "CONFLICTING"
			case "moved":
				r.pull.Head = loopFixed
			case reviewedRedCase:
				r.pull.Green = false
			case reviewedStoppedCase:
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Stop = &state.Stop{}; return nil, nil }); err != nil {
					t.Fatal(err)
				}
			case reviewedHeldCase:
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Holds = append(st.Holds, state.Hold{Target: loopMergeRepo, Reason: reviewedHeldCase})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
			if doc.ExitCode == 0 || len(r.cmds) > 0 {
				t.Fatalf("unsafe mutation: doc %+v commands %v", doc, r.cmds)
			}
		})
	}
}

func TestReviewedMergeUsesConfiguredMethodAndGatePin(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.Methods = map[string]string{loopMergeRepo: mergeMethodRebase}
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	if err := r.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != 0 {
		t.Fatalf("merge %+v", doc)
	}
	argv := r.cmds[0]
	for _, flag := range []string{"--rebase", "--admin", "--match-head-commit", loopHead} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("missing %s in %v", flag, argv)
		}
	}
	g := gateRun{app: r.a, repo: loopMergeRepo, pr: 7, reviewedHead: loopHead, argv: []string{"devctl", "pr", superloop.KindMerge, loopMergeRepo, "7"}}
	pinned, err := g.mergeArgv()
	if err != nil || !slices.Contains(pinned, "--head") || !slices.Contains(pinned, loopHead) {
		t.Fatalf("gate lost its pin: %v %v", pinned, err)
	}
}

func TestLoopMergeDoesNotGrantWorkerAuthority(t *testing.T) {
	worker := state.Party{Name: loopWorker}
	e := about(event(loopParty, "supervisor.merge", "[go %s@%s] %s: merged", loopPR, loopHead, loopWorker), loopPR)
	legacy := event(loopParty, "supervisor.go", "[go %s@%s] %s: merge go", loopPR, loopHead, loopWorker)
	if recordedMergeGo([]state.Event{legacy, e}, worker, loopPR, loopHead) {
		t.Fatal("coordinator merge became a worker grant")
	}
}

func TestReviewedRebaseMergeRefusesABehindBase(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.Methods = map[string]string{loopMergeRepo: mergeMethodRebase}
	if err := r.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	// The base requires up-to-date branches and the desk merges without
	// --admin: only a base merge would bring the branch up to date, which a
	// rebase merge cannot take.
	r.pull.MergeState = mergeStateBehind
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != github.SquashNotApplicable || len(r.cmds) != 0 || !strings.Contains(doc.Reason, "behind") {
		t.Fatalf("behind rebase merge: %+v %v", doc, r.cmds)
	}
}

func TestLoopEscalationReleasesMergeWithoutAnswerVerify(t *testing.T) {
	r := newLoopRig(t)
	// Refuse the mechanical merge to keep the pull request open as the
	// worker publishes heads requiring the two allowed review rounds.
	r.command = func([]string) (string, int) { return "branch behind base", 1 }
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.pass(t)
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(time.Minute))
	r.pass(t)
	r.verdict("R2", loopFixed, superloop.Approved, "")
	r.pass(t)
	const third = "3333333333333333333333333333333333333333"
	r.pull.Head = third
	r.park(t, loopPR, time.Now().Add(2*time.Minute))
	r.pass(t)
	if r.called("note", loopAdd) == nil {
		t.Fatal("no escalation")
	}
	n := state.Note{ID: 9, By: loopParty, Text: superloop.EscalationQuestion(loopURL, third)}
	recorder := state.Party{Session: dialogSession, Name: verifyGuide}
	rec := recordedAnswer{About: loopPR, By: loopPerson, Channel: channelSession, RelayedBy: verifyGuide, Answer: dialogMergeChoice, Note: 9}
	if err := r.a.store.Record(answerEvent(recorder, &rec, &n)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	call := r.called(gateCmdName)
	if call == nil || !slices.Contains(call, third) {
		t.Fatalf("session answer did not release its head without answer verify: %v\n%s", r.calls, r.out)
	}
}

func TestLoopMergeFallsBackAutomaticallyWhenReviewedMergeStepBreaks(t *testing.T) {
	t.Run("gh pr merge tooling failure triggers session merge go", func(t *testing.T) {
		r := newLoopRig(t)
		r.a.cfg.Merge.DevctlOwners = []string{"o"}
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		r.command = func(argv []string) (string, int) {
			if slices.Contains(argv, superloop.KindMerge) {
				return "GraphQL: Resource not accessible by integration", 1
			}
			return "", 0
		}
		var grantedBeforeWake bool
		r.answer = func(args []string) (string, int) {
			if len(args) > 0 && args[0] == gateCmdName && !slices.Contains(args, "--reviewed-head") {
				t.Fatalf("unpinned gate call: %v", args)
			}
			if len(args) >= 3 && args[0] == agentsName && args[1] == agentWakeName && args[2] == loopWorker {
				evs := r.logged(t, "supervisor.go")
				grantedBeforeWake = recordedMergeGo(evs, state.Party{Name: loopWorker}, loopPR, loopHead)
			}
			return "", 0
		}
		r.pass(t)
		if !grantedBeforeWake {
			t.Fatal("supervisor.go was not recorded before waking the worker")
		}
		if r.called("note", loopAdd) != nil {
			t.Fatalf("broken merge step asked the person: %v", r.calls)
		}
		wake := r.called(agentsName, agentWakeName, loopWorker)
		if wake == nil || strings.Contains(wake[3], "Repair the branch") || !strings.Contains(wake[3], "devctl pr merge o/r 7") || !strings.Contains(wake[3], loopHead) {
			t.Fatalf("worker was not directed to merge approved head: %v\n%s", r.calls, r.out)
		}
		evs := r.logged(t, "supervisor.go")
		if len(evs) != 1 || !recordedMergeGo(evs, state.Party{Name: loopWorker}, loopPR, loopHead) {
			t.Fatalf("worker merge go not granted: %+v", evs)
		}
	})

	t.Run("interrupted worker merge go reconciles as supervisor.go", func(t *testing.T) {
		r := newLoopRig(t)
		r.a.cfg.Merge.DevctlOwners = []string{"o"}
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		if err := r.a.store.Record(
			about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR),
			event(loopParty, "agents.wake", "%s: sent by name", loopWorker),
		); err != nil {
			t.Fatal(err)
		}
		r.pass(t)
		evs := r.logged(t, "supervisor.go")
		if len(evs) != 1 || !recordedMergeGo(evs, state.Party{Name: loopWorker}, loopPR, loopHead) {
			t.Fatalf("interrupted worker merge go did not reconcile as supervisor.go: go=%+v merge=%+v\n%s", evs, r.logged(t, "supervisor.merge"), r.out)
		}
	})
}

func TestLoopConflictingPRHandsOffToWorkerOnce(t *testing.T) {
	t.Run("conflicting pull state skips gate and hands off once", func(t *testing.T) {
		r := newLoopRig(t)
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		r.pull.Mergeable = "CONFLICTING"
		r.pull.MergeState = github.DirtyState
		r.pass(t)
		if c := r.called(gateCmdName); c != nil {
			t.Fatalf("conflicting PR invoked gate: %v", c)
		}
		wake := r.called(agentsName, agentWakeName, loopWorker)
		if wake == nil || !strings.Contains(wake[3], "conflicts with its base") {
			t.Fatalf("conflicting PR did not wake worker for repair: %v", r.calls)
		}
		if len(r.logged(t, "supervisor.merge")) != 1 {
			t.Fatalf("expected 1 supervisor.merge record, got %d", len(r.logged(t, "supervisor.merge")))
		}
		// Subsequent passes do not retry or wake again for the same head.
		r.pass(t)
		if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
			t.Fatalf("conflicting PR retried repair wake on next pass: %v", c)
		}
		if c := r.called(gateCmdName); c != nil {
			t.Fatalf("conflicting PR retried gate on next pass: %v", c)
		}
	})

	t.Run("queued gate failure in state hands off once", func(t *testing.T) {
		r := newLoopRig(t)
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		// First pass queues behind a busy lane.
		r.answer = func(args []string) (string, int) {
			if len(args) > 0 && args[0] == gateCmdName {
				return "queued in lane", ExitGateQueued
			}
			return "", 0
		}
		r.pass(t)
		if len(r.logged(t, "supervisor.merge")) != 0 {
			t.Fatal("queued merge should not be recorded complete before it finishes")
		}
		// The detached gate runner finishes with a conflict/branch refusal in st.Failed.
		if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Failed = append(st.Failed, state.Failed{
				Repo:   loopMergeRepo,
				PR:     7,
				Lane:   loopMergeRepo,
				By:     loopParty,
				At:     r.a.now,
				Exit:   github.SquashNotApplicable,
				Reason: "pull request is not mergeable (CONFLICTING)",
			})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		r.pass(t)
		if c := r.called(gateCmdName); c != nil {
			t.Fatalf("failed detached merge called gate again instead of handing off: %v", c)
		}
		wake := r.called(agentsName, agentWakeName, loopWorker)
		if wake == nil || !strings.Contains(wake[3], "CONFLICTING") {
			t.Fatalf("detached merge failure did not wake worker: %v", r.calls)
		}
		if len(r.logged(t, "supervisor.merge")) != 1 {
			t.Fatalf("expected 1 supervisor.merge record, got %d", len(r.logged(t, "supervisor.merge")))
		}
		r.pass(t)
		if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
			t.Fatalf("repeated repair wake after detached failure: %v", c)
		}
	})
}

func TestLoopMergesFollowBoardPriority(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Board = config.Board{Owner: "o", Project: 1}
	const (
		prLow    = "o/r#7"
		prHigh   = "o/r#8"
		urlHigh  = "https://github.com/o/r/pull/8"
		epicHigh = "o/r#10"
		epicLow  = "o/r#20"
	)
	wasPriority := readPullPriority
	t.Cleanup(func() { readPullPriority = wasPriority })
	readPullPriority = func(_ context.Context, _ *supervisorLoop, ref, serves string) (int, error) {
		switch {
		case ref == prHigh || serves == epicHigh:
			return 1, nil
		case ref == prLow || serves == epicLow:
			return 2, nil
		}
		return 0, nil
	}

	now := time.Now().UTC()
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{
			{
				Party: state.Party{Name: "worker-low"},
				Task:  "work " + epicLow,
				Park:  &state.Park{On: prLow, At: now, Waits: deskReviewWait, ReviewHead: loopHead},
			},
			{
				Party: state.Party{Name: "worker-high"},
				Task:  "work " + epicHigh,
				Park:  &state.Park{On: prHigh, At: now, Waits: deskReviewWait, ReviewHead: loopHead},
			},
		}
		st.Records = []state.Record{
			{Session: state.Party{Name: "worker-low"}, Issue: epicLow, At: now},
			{Session: state.Party{Name: "worker-high"}, Issue: epicHigh, At: now},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pulls[prLow] = superloop.Pull{Ref: prLow, URL: loopURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	r.pulls[prHigh] = superloop.Pull{Ref: prHigh, URL: urlHigh, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}

	// Pass 1 starts reviews for both PRs.
	r.pass(t)
	approvedAt := time.Now().UTC().Add(time.Second)
	lowPull := r.pulls[prLow]
	lowPull.Reviews = []superloop.Review{{ID: "R1", Head: loopHead, Verdict: superloop.Approved, At: approvedAt}}
	r.pulls[prLow] = lowPull
	highPull := r.pulls[prHigh]
	highPull.Reviews = []superloop.Review{{ID: "R1", Head: loopHead, Verdict: superloop.Approved, At: approvedAt}}
	r.pulls[prHigh] = highPull

	// Pass 2 merges both PRs in the o/r lane: rank 1 (o/r#8) must enter gate
	// before rank 2 (o/r#7) and pass --priority 1 and --priority 2.
	r.pass(t)
	var gatePRs []string
	var gatePriorities []string
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == gateCmdName {
			if sep := slices.Index(c, "--"); sep >= 0 && sep+5 < len(c) {
				gatePRs = append(gatePRs, c[sep+5])
			}
			if idx := slices.Index(c, "--priority"); idx >= 0 && idx+1 < len(c) {
				gatePriorities = append(gatePriorities, c[idx+1])
			}
			if !slices.Contains(c, "--wait") || !slices.Contains(c, "0s") {
				t.Fatalf("gate call missing --wait 0s: %v", c)
			}
		}
	}
	if want := []string{"8", "7"}; !slices.Equal(gatePRs, want) {
		t.Fatalf("gate PR order = %v, want %v (rank-1 epic first)", gatePRs, want)
	}
	if want := []string{"1", "2"}; !slices.Equal(gatePriorities, want) {
		t.Fatalf("gate priorities = %v, want %v", gatePriorities, want)
	}
}

func TestLoopParallelMergeDoesNotBlockReviewOrWorkerStart(t *testing.T) {
	r := newLoopRig(t)
	brief := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
		t.Fatal(err)
	}
	r.a.cfg.Supervisor.WorkerBrief = brief
	r.a.cfg.Supervisor.FillRetryEvery.Duration = 5 * time.Minute
	r.a.cfg.Capacity.Choices = []config.Choice{{Harness: config.CodexHarness, Model: "parallel-test-model"}}
	r.a.cfg.Board = config.Board{Owner: "o", Project: 1, TidyEvery: config.Duration{Duration: time.Minute}, EscalateEvery: config.Duration{Duration: -1}}

	const (
		reviewPR  = "o/s#8"
		reviewURL = "https://github.com/o/s/pull/8"
	)
	now := r.a.now
	// loopPR (o/r#7) already had its review started and approved; reviewPR (o/s#8)
	// is newly parked waiting for a reviewer to start; board next has o/s#10 ready.
	r.park(t, loopPR, now)
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")

	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{
			Party: state.Party{Name: "worker two", Session: "s-two"},
			Task:  "work " + reviewURL,
			Park:  &state.Park{On: reviewPR, At: now.Add(time.Second), Waits: deskReviewWait, ReviewHead: loopHead},
		})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pulls[reviewPR] = superloop.Pull{Ref: reviewPR, URL: reviewURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}

	gateEntered := make(chan struct{})
	releaseGate := make(chan struct{})
	reviewStarted := make(chan struct{})
	workerStarted := make(chan struct{})
	tidyRan := make(chan struct{})
	busy := 0

	r.answer = func(args []string) (string, int) {
		switch {
		case len(args) > 0 && args[0] == gateCmdName:
			close(gateEntered)
			<-releaseGate
			return "", 0
		case slices.Equal(args, []string{loopBoard, boardTidyName}):
			close(tidyRan)
			return "", 0
		case len(args) > 0 && args[0] == fillCapacityCommand:
			if slices.Contains(args, "--review") {
				return `{"room":1}`, 0
			}
			r.mu.Lock()
			b := busy
			r.mu.Unlock()
			if b > 0 {
				return `{"ceiling":1,"room":0}`, 0
			}
			return `{"ceiling":1,"room":1}`, 0
		case len(args) >= 2 && args[0] == loopBoard && args[1] == superloop.KindNext:
			return `{"pick":{"ref":"o/s#10","url":"https://github.com/o/s/issues/10"}}`, 0
		case len(args) >= 3 && args[0] == agentsName && args[1] == agentStartName:
			if slices.Contains(args, "--review") {
				close(reviewStarted)
			} else if args[2] == "s 10" {
				r.mu.Lock()
				busy++
				r.mu.Unlock()
				close(workerStarted)
			}
		}
		return "", 0
	}

	passDone := make(chan error, 1)
	go func() {
		passDone <- r.passErr()
	}()

	select {
	case <-gateEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("merge did not enter gate")
	}

	// While the merge of o/r#7 is blocked inside gate waiting on CI, the
	// reviewer start for o/s#8, the worker start for o/s#10, and board tidy
	// must all complete without waiting for the merge.
	for name, ch := range map[string]<-chan struct{}{
		"review start": reviewStarted,
		"worker start": workerStarted,
		"board tidy":   tidyRan,
	} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was blocked by in-flight merge", name)
		}
	}

	close(releaseGate)
	if err := <-passDone; err != nil {
		t.Fatal(err)
	}
	if len(r.logged(t, "supervisor.merge")) != 1 {
		t.Fatalf("expected 1 supervisor.merge event, got %d", len(r.logged(t, "supervisor.merge")))
	}
	if len(r.logged(t, "supervisor.review")) != 2 {
		t.Fatalf("expected 2 supervisor.review events, got %d", len(r.logged(t, "supervisor.review")))
	}
	if len(r.logged(t, "supervisor.next")) != 1 {
		t.Fatalf("expected 1 supervisor.next event, got %d", len(r.logged(t, "supervisor.next")))
	}
}
