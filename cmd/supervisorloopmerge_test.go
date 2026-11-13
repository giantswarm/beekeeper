package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/gate"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	loopMergeRepo       = "o/r"
	reviewedStoppedCase = "stopped"
	reviewedHeldCase    = "held"
	reviewedRedCase     = "red"
)

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopReviewedMergeWaitsForHoldWithoutFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expire bool
	}{
		{name: "hold-cleared"},
		{name: "hold-expired", expire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.Merge.DevctlOwners = []string{"o"}
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Holds = append(st.Holds, state.Hold{Target: loopMergeRepo, Reason: reviewedHeldCase})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				r.pass(t)
				if r.called(gate.GateCmdName) != nil {
					t.Fatalf("held merge reached gate: %v", r.calls)
				}
				r.a.now = r.a.now.Add(time.Hour)
			}
			for _, verb := range []string{"merge.authorized", "merge.start", loop.LoopMergeFailed, loop.LoopFailureVerb, loop.LoopFileVerb} {
				if events := r.logged(t, verb); len(events) != 0 {
					t.Fatalf("hold produced %s: %+v", verb, events)
				}
			}
			if !strings.Contains(r.out.String(), "hold:"+loopMergeRepo) {
				t.Fatalf("hold wait was not explained: %s", r.out)
			}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				if !tc.expire {
					st.Holds = nil
				} else {
					st.Holds[0].Until = time.Now().Add(-time.Second)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			if r.called(gate.GateCmdName) == nil || len(r.logged(t, "merge.authorized")) != 1 {
				t.Fatalf("lift did not resume the reviewed merge: %v\n%s", r.calls, r.out)
			}
			if len(r.logged(t, "supervisor.merge")) != 1 {
				t.Fatalf("lifted merge did not finish: %s", r.out)
			}
			if r.called(agentsName, agentStartName) != nil {
				t.Fatalf("lift requested another review: %v", r.calls)
			}
		})
	}
}

func TestLoopReviewedMergeHonorsHoldExceptions(t *testing.T) {
	for _, hold := range []state.Hold{
		{Target: loopMergeRepo, Except: loopPR},
		{Target: loopMergeRepo, ExceptSession: loop.LoopParty.Name},
		{Target: "another/repo"},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s", hold.Target, hold.Except, hold.ExceptSession), func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Holds = append(st.Holds, hold)
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			if len(r.logged(t, "merge.authorized")) != 1 {
				t.Fatalf("nonblocking hold stopped merge: %s", r.out)
			}
		})
	}
}

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

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopDoesNotAuthorizeOverANewerReviewOfTheEarlierHead(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.ChangesRequested, "Review: changes requested\n\nfix the handler")
	r.pass(t)
	r.pull.Head = loopFixed
	r.park(t, loopPR, time.Now().Add(time.Minute))
	read := loop.ReadLoopPull
	reads := 0
	loop.ReadLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		p, err := read(ctx, ref)
		reads++
		if reads > 1 {
			p.Reviews = append(slices.Clone(p.Reviews), superloop.Review{ID: "R2", Author: loopDesk, Head: loopHead, Verdict: superloop.ChangesRequested, At: time.Now()})
		}
		return p, err
	}
	r.pass(t)
	if reads < 2 {
		t.Fatal("did not exercise the executor's live review read")
	}
	if r.called(gate.GateCmdName) != nil || len(r.logged(t, "merge.authorized")) != 0 {
		t.Fatalf("new findings were overwritten by a fresh authorization: %v", r.calls)
	}
	if !strings.Contains(r.out.String(), "requested changes") {
		t.Fatalf("new review refusal was not reported: %s", r.out)
	}
}

func TestLoopMergeRepairDoesNotBypassQueue(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.command = func(argv []string) (string, int) {
		if slices.Contains(argv, "--admin") || !slices.Contains(argv, loopHead) {
			t.Fatalf("merge flags: %v", argv)
		}
		return "branch conflicts with base", 1
	}
	r.pass(t)
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("conflict returned to the author: %v", c)
	}
	r.reviewerRepair(t, loopReviewer, "Repair the branch")
	r.pass(t)
	if c := r.called(agentsName, agentStartName, loopReviewer); c != nil {
		t.Fatalf("repeated repair start %v", c)
	}
	if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
		t.Fatalf("repeated repair wake %v", c)
	}
}

// reviewerRepair fails t unless this pass started reviewer's repair turn
// with a brief that carries want: a real conflict after approval returns to
// the reviewer, never to the author and never as a second review.
func (r *loopRig) reviewerRepair(t *testing.T, reviewer, want string) {
	t.Helper()
	start := r.called(agentsName, agentStartName, reviewer)
	if start == nil {
		t.Fatalf("no repair turn for %s: %v\n%s", reviewer, r.calls, r.out)
	}
	brief, err := os.ReadFile(start[3])
	if err != nil || !strings.Contains(string(brief), want) {
		t.Fatalf("repair brief of %s lacks %q: %v: %s", reviewer, want, err, brief)
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
	for _, configured := range []bool{false, true} {
		for _, scenario := range []string{"unclaimed", "moved", reviewedRedCase, reviewedStoppedCase, reviewedHeldCase, litConflict, "changes-requested"} {
			t.Run(fmt.Sprintf("%s/devctl=%t", scenario, configured), func(t *testing.T) {
				r := newLoopRig(t)
				if configured {
					r.a.cfg.Merge.DevctlOwners = []string{"o"}
				}
				r.park(t, loopPR, time.Now())
				if scenario != "unclaimed" {
					if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
						t.Fatal(err)
					}
				}
				switch scenario {
				case litConflict:
					r.pull.Mergeable = loop.MergeableConflicting
				case "moved":
					r.pull.Head = loopFixed
				case reviewedRedCase:
					r.pull.Green = false
				case "changes-requested":
					r.pull.Reviews = []superloop.Review{
						{ID: "r1", Head: loopHead, Verdict: superloop.Approved, At: time.Unix(100, 0)},
						{ID: "r2", Head: loopHead, Verdict: superloop.ChangesRequested, At: time.Unix(200, 0)},
					}
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
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestReviewedMergeUsesConfiguredMethodAndGatePin(t *testing.T) {
	r := newLoopRig(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.a.cfg.Merge.Methods = map[string]string{loopMergeRepo: mergeMethodRebase}
	r.a.cfg.Merge.Admin = []string{loopMergeRepo}
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != 0 {
		t.Fatalf("merge %+v", doc)
	}
	argv := r.cmds[0]
	for _, flag := range []string{"--rebase", "--match-head-commit", loopHead} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("missing %s in %v", flag, argv)
		}
	}
	g := gate.GateRun{Host: r.a, Repo: loopMergeRepo, Pr: 7, ReviewedHead: loopHead, Argv: []string{"devctl", "pr", superloop.KindMerge, loopMergeRepo, "7"}}
	pinned, err := g.MergeArgv()
	if err != nil || !slices.Contains(pinned, "--head") || !slices.Contains(pinned, loopHead) {
		t.Fatalf("gate lost its pin: %v %v", pinned, err)
	}
}

func TestLoopMergeDoesNotGrantWorkerAuthority(t *testing.T) {
	worker := state.Party{Name: loopWorker}
	e := about(event(loop.LoopParty, "supervisor.merge", "[go %s@%s] %s: merged", loopPR, loopHead, loopWorker), loopPR)
	legacy := event(loop.LoopParty, "supervisor.go", "[go %s@%s] %s: merge go", loopPR, loopHead, loopWorker)
	if recordedMergeGo([]state.Event{legacy, e}, worker, loopPR, loopHead) {
		t.Fatal("coordinator merge became a worker grant")
	}
}

func TestReviewedRebaseMergeKeepsABehindHead(t *testing.T) {
	r := newLoopRig(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.a.cfg.Merge.Methods = map[string]string{loopMergeRepo: mergeMethodRebase}
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	r.pull.MergeState = mergeStateBehind
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
	if doc.ExitCode != 0 || r.lastMerged != loopHead {
		t.Fatalf("behind rebase merge: %+v %v", doc, r.cmds)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopConflictHandsOffOnceAndMergesAfterRebaseWithoutNewReview(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mergeable string
		repark    bool
		wantGates int
		failure   int
		// repairer gets the repair turn: the approving reviewer for a real
		// conflict, the worker for a failed merge validation.
		repairer string
	}{
		{name: "without_repark", mergeable: loop.MergeableConflicting, repark: false, wantGates: 1, repairer: loopReviewer},
		{name: "with_repark", mergeable: loop.MergeableConflicting, repark: true, wantGates: 1, repairer: loopReviewer},
		{name: "gate_conflict_while_full", mergeable: "UNKNOWN", repark: true, wantGates: 2, failure: github.SquashNotApplicable, repairer: loopWorker},
		{name: "gate_red_while_full", mergeable: "UNKNOWN", repark: true, wantGates: 2, failure: github.SquashRed, repairer: loopWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			r.pull.Branch = "fix-2147-conflict"
			r.park(t, loopPR, time.Now())
			r.started(t, loopWorker, "worker-session-1")
			work := ""
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park.PostMerge = &work
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			// Pass 1: worker ends into ReviewParks and reviewer R1 starts.
			r.pass(t)
			if r.called(agentsName, loop.LoopRemove, loopWorker) == nil || r.called(agentsName, agentStartName, loopReviewer) == nil {
				t.Fatalf("worker did not end or reviewer did not start: %v", r.calls)
			}

			// R1 approves loopHead, which now conflicts with main, while fixer capacity is full.
			r.verdict("R1", loopHead, superloop.Approved, "")
			r.pull.Mergeable = tc.mergeable
			r.command = func(argv []string) (string, int) {
				if slices.Contains(argv, loopHead) {
					return "Pull request cannot be merged: the merge commit cannot be cleanly created", 1
				}
				r.pull.State = superloop.Merged
				r.pull.MergeCommit = loopFixed
				r.lastMerged = r.pull.Head
				return "", 0
			}
			room := 0
			gateRuns := 0
			r.answer = func(args []string) (string, int) {
				if len(args) > 0 && args[0] == gate.GateCmdName {
					gateRuns++
					if gateRuns == 1 && tc.failure != 0 {
						// Model the real synchronous gate: it persists its failed
						// outcome before returning the squash document to the loop.
						if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
							st.Merges = append(st.Merges, state.Merge{Repo: loopMergeRepo, PR: 7, Head: loopHead, By: loop.LoopParty})
							evs := recordRun(st, len(st.Merges)-1, config.Lane{Name: loopMergeRepo}, loop.LoopParty,
								gate.RunOutcome{Rc: tc.failure, Reason: "merge validation failed"}, r.a.now, "")
							if len(st.Failed) != 1 || st.Failed[0].Head != loopHead {
								t.Fatalf("gate lost the tested head: %+v", st.Failed)
							}
							return evs, nil
						}); err != nil {
							t.Fatal(err)
						}
						return fmt.Sprintf(`{"route":"%s","repository":"%s","number":7,"headSha":"%s","reason":"merge validation failed"}`, reviewedMergeRoute, loopMergeRepo, loopHead), tc.failure
					}
				}
				if len(args) >= 2 && args[0] == agentsName && args[1] == agentStartName && room == 0 {
					return "no room for a start", ExitRefused
				}
				return "", 0
			}

			// Passes 2 & 3: capacity is full; the loop must never retry gate each pass.
			r.pass(t)
			r.pass(t)
			if gateRuns != tc.wantGates-1 {
				t.Fatalf("conflicting PR invoked merge gate %d times, want %d", gateRuns, tc.wantGates-1)
			}
			if len(r.logged(t, "supervisor.merge")) != 0 {
				t.Fatalf("completed repair handoff without capacity: %v", r.logged(t, "supervisor.merge"))
			}

			// Pass 4: capacity frees up; the repairer starts once, without invoking gate again.
			room = 1
			r.pass(t)
			if gateRuns != tc.wantGates-1 {
				t.Fatalf("conflicting PR invoked merge gate %d times, want %d", gateRuns, tc.wantGates-1)
			}
			repairStart := r.called(agentsName, agentStartName, tc.repairer)
			if repairStart == nil || len(r.logged(t, "supervisor.merge")) != 1 {
				t.Fatalf("expected %s to start for the repair, got %v\n%s", tc.repairer, r.calls, r.out)
			}

			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Failed) != 0 {
				t.Fatalf("repair left a stale gate failure: %+v", st.Failed)
			}

			// Pass 5: fixer rebases onto main to resolve the conflict (loopFixed); CI is still pending.
			r.pull.Head = loopFixed
			r.pull.Mergeable = "MERGEABLE"
			r.pull.Green = false
			r.baseMerge = func(p superloop.Pull, approved string) (bool, error) {
				return approved == loopHead && p.Head == loopFixed, nil
			}
			r.a.now = r.a.now.Add(loopPullEvery)
			if tc.repark {
				r.park(t, loopPR, r.a.now)
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Agents[0].Park.ReviewHead = loopFixed
					st.Agents[0].Park.PostMerge = &work
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			r.pass(t)
			if gateRuns != tc.wantGates-1 || r.called(agentsName, agentStartName, superloop.ReviewerName(loopPR, 2)) != nil {
				t.Fatalf("acted before CI turned green or started a second review: gate=%d calls=%v", gateRuns, r.calls)
			}

			// Pass 6: CI turns green on the rebased head; the supervisor merges without a second review.
			r.pull.Green = true
			r.a.now = r.a.now.Add(loopPullEvery)
			r.pass(t)
			if r.called(agentsName, agentStartName, superloop.ReviewerName(loopPR, 2)) != nil {
				t.Fatalf("conflict-only rebase started a second review: %v\n%s", r.calls, r.out)
			}
			if gateRuns != tc.wantGates || r.lastMerged != loopFixed {
				t.Fatalf("expected %d gate runs and merge of %s, got gateRuns=%d lastMerged=%s calls=%v\n%s", tc.wantGates, loopFixed, gateRuns, r.lastMerged, r.calls, r.out)
			}
		})
	}
}

func TestLoopMergeFallsBackAutomaticallyWhenReviewedMergeStepBreaks(t *testing.T) {
	t.Run("gh merge tooling failure cannot delegate a queue bypass", func(t *testing.T) {
		r := newLoopRig(t)
		r.a.cfg.Merge.DevctlOwners = []string{"o"}
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		r.command = func(argv []string) (string, int) { return "GraphQL: Resource not accessible by integration", 1 }
		r.pass(t)
		if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
			t.Fatalf("unsafe merge delegated: %v", wake)
		}
		if evs := r.logged(t, "supervisor.go"); len(evs) != 0 {
			t.Fatalf("unsafe merge go: %+v", evs)
		}
	})

	t.Run("interrupted worker merge go reconciles as supervisor.go", func(t *testing.T) {
		r := newLoopRig(t)
		r.a.cfg.Merge.DevctlOwners = []string{"o"}
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		if err := r.a.store.Record(
			about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR),
			event(loop.LoopParty, "agents.wake", "%s: sent by name", loopWorker),
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

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopConflictingPRHandsOffItsRepairOnce(t *testing.T) {
	t.Run("conflicting pull state skips gate and hands off once", func(t *testing.T) {
		r := newLoopRig(t)
		r.park(t, loopPR, time.Now())
		r.pass(t)
		r.verdict("R1", loopHead, superloop.Approved, "")
		r.pull.Mergeable = "CONFLICTING"
		r.pull.MergeState = github.DirtyState
		r.pass(t)
		if c := r.called(gate.GateCmdName); c != nil {
			t.Fatalf("conflicting PR invoked gate: %v", c)
		}
		if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
			t.Fatalf("conflict returned to the author: %v", c)
		}
		r.reviewerRepair(t, loopReviewer, "conflicts with its base")
		if len(r.logged(t, "supervisor.merge")) != 1 {
			t.Fatalf("expected 1 supervisor.merge record, got %d", len(r.logged(t, "supervisor.merge")))
		}
		// Subsequent passes do not retry or start the repair again for the same head.
		r.pass(t)
		if c := r.called(agentsName, agentStartName, loopReviewer); c != nil {
			t.Fatalf("conflicting PR retried its repair start on next pass: %v", c)
		}
		if c := r.called(agentsName, agentWakeName, loopWorker); c != nil {
			t.Fatalf("conflicting PR retried repair wake on next pass: %v", c)
		}
		if c := r.called(gate.GateCmdName); c != nil {
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
			if len(args) > 0 && args[0] == gate.GateCmdName {
				return "queued in lane", gate.ExitGateQueued
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
				Head:   loopHead,
				PR:     7,
				Lane:   loopMergeRepo,
				By:     loop.LoopParty,
				At:     r.a.now,
				Exit:   github.SquashNotApplicable,
				Reason: "pull request is not mergeable (CONFLICTING)",
			})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		r.pass(t)
		if c := r.called(gate.GateCmdName); c != nil {
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

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
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
			Party: state.Party{Name: loopSecondWorker, Session: "s-two"},
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
		case len(args) > 0 && args[0] == gate.GateCmdName:
			close(gateEntered)
			<-releaseGate
			return "", 0
		case slices.Equal(args, []string{loop.LoopBoard, loop.BoardTidyName}):
			close(tidyRan)
			return "", 0
		case len(args) > 0 && args[0] == fillCapacityCommand:
			if slices.Contains(args, "--review") {
				return loopRoomJSON, 0
			}
			r.mu.Lock()
			b := busy
			r.mu.Unlock()
			if b > 0 {
				return `{"ceiling":3,"room":0}`, 0
			}
			// Two existing delivery observations reserve room before their actions
			// settle. A third place permits unrelated board work during the merge.
			return `{"ceiling":3,"room":3}`, 0
		case len(args) >= 2 && args[0] == loop.LoopBoard && args[1] == superloop.KindNext:
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

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopSlowStartDoesNotBlockLaterPassMerge(t *testing.T) {
	for _, slow := range []string{capacity.TaskKindReview, workerRole} {
		t.Run(slow, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, r.a.now)
			r.pass(t) // The first PR's reviewer is already running.
			brief := filepath.Join(t.TempDir(), "worker.md")
			if err := os.WriteFile(brief, []byte("Work {{.URL}}."), 0o600); err != nil {
				t.Fatal(err)
			}
			r.a.cfg.Supervisor.WorkerBrief = brief
			r.a.cfg.Capacity.Choices = []config.Choice{{Harness: config.CodexHarness, Model: "parallel-test-model"}}
			r.a.cfg.Board = config.Board{Owner: "o", Project: 1, TidyEvery: config.Duration{Duration: time.Minute}, EscalateEvery: config.Duration{Duration: -1}}
			const second = "o/s#8"
			r.pulls[second] = superloop.Pull{Ref: second, URL: "https://github.com/o/s/pull/8", State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = append(st.Agents, state.Agent{
					Party: state.Party{Name: loopSecondWorker, Session: "s-two"},
					Task:  "work https://github.com/o/s/pull/8",
					Park:  &state.Park{On: second, At: r.a.now, Waits: deskReviewWait, ReviewHead: loopHead},
				})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			entered, release, merged := make(chan struct{}), make(chan struct{}), make(chan struct{}, 4)
			var starts, busy atomic.Int32
			r.answer = func(args []string) (string, int) {
				switch {
				case len(args) > 0 && args[0] == gate.GateCmdName:
					merged <- struct{}{}
				case len(args) > 0 && args[0] == fillCapacityCommand:
					if slices.Contains(args, "--review") {
						return loopRoomJSON, 0
					}
					if busy.Load() > 0 {
						return `{"ceiling":1,"room":0}`, 0
					}
					if slow == workerRole {
						// Keep a place beyond the pending review reservation.
						return `{"ceiling":2,"room":2}`, 0
					}
					return `{"ceiling":1,"room":1}`, 0
				case len(args) >= 2 && args[0] == loop.LoopBoard && args[1] == superloop.KindNext:
					return `{"pick":{"ref":"o/s#10","url":"https://github.com/o/s/issues/10"}}`, 0
				case len(args) >= 3 && args[0] == agentsName && args[1] == agentStartName:
					review := slices.Contains(args, "--review")
					if !review {
						busy.Add(1)
					}
					if review == (slow == capacity.TaskKindReview) {
						if starts.Add(1) == 1 {
							close(entered)
						}
						<-release
					}
				}
				return "", 0
			}
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			defer func() {
				if !released {
					close(release)
				}
				l.PassActions.Wait()
				l.MergeActions.Wait()
			}()
			pass := func() {
				t.Helper()
				done := make(chan error, 1)
				go func() { done <- l.Pass(context.Background()) }()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("slow start blocked observation")
				}
			}
			pass()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("start did not enter")
			}
			// This approval was absent from the first snapshot. The next pass
			// must observe and submit it while the unrelated start remains stuck.
			now := r.a.now.Add(time.Second)
			r.a.loopNow.Store(&now)
			r.verdict("R1", loopHead, superloop.Approved, "")
			if err := r.a.store.Record(event(state.Party{Name: superloop.ReviewerName(loopPR, 1)}, superloop.VerbDone, "%s done: review complete", superloop.ReviewerName(loopPR, 1))); err != nil {
				t.Fatal(err)
			}
			pass()
			select {
			case <-merged:
			case <-time.After(5 * time.Second):
				t.Fatal("newly approved merge blocked by slow start")
			}
			if got := starts.Load(); got != 1 {
				t.Fatalf("slow start launched %d times, want 1", got)
			}
			// A binary replacement also must not block a pass behind the
			// slow start. Re-execution happens only after the streams drain.
			r.mu.Lock()
			r.replaced = true
			r.mu.Unlock()
			pass()
			if r.execs != 0 {
				t.Fatal("re-executed while the start was still in flight")
			}
			close(release)
			released = true
			l.PassActions.Wait()
			l.MergeActions.Wait()
			if reexec, err := l.Reexec(t.Context()); err != nil || !reexec {
				t.Fatalf("quiet re-execution = %v, %v", reexec, err)
			}
		})
	}
}

// A pass may have read its action list just before a previous pass records
// completion. Reusing that stale list must not start a second reviewer.
func TestLoopCompletedStartSuppressesStaleSnapshot(t *testing.T) {
	r := newLoopRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	r.answer = func(args []string) (string, int) {
		if len(args) > 1 && args[0] == agentsName && args[1] == agentStartName {
			starts++
		}
		return "", 0
	}
	acts := []superloop.Action{{Kind: superloop.KindReview, Key: "review stale snapshot", Ref: loopPR, URL: loopURL, Head: loopHead, Round: 1}}
	for range 2 {
		if err := l.RunPassActions(t.Context(), st, nil, nil, nil, acts); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 1 {
		t.Fatalf("started %d reviewers from the same snapshot, want 1", starts)
	}
}

// Regression test for https://github.com/giantswarm/beekeeper/issues/2178:
// an ownership claim is insufficient even when an older review was fixed.
func TestReviewedMergeRefusesTheLoopClaimWithoutAnExactHeadGo(t *testing.T) {
	r := newLoopRig(t)
	r.verdict("R1", loopHead, superloop.Approved, "Review: approved")
	r.pull.Head = loopFixed
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopFixed, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopFixed)
	if doc.ExitCode != github.SquashRefused || len(r.cmds) != 0 {
		t.Fatalf("ownership claim alone: %+v cmds=%v, want refusal", doc, r.cmds)
	}
	if doc.RequestedBy != "supervisor loop (for "+loopWorker+")" {
		t.Fatalf("RequestedBy = %q", doc.RequestedBy)
	}

	r = newLoopRig(t)
	r.verdict("R1", loopHead, superloop.ChangesRequested, "Review: changes requested\n\nfix planOverlap case matching")
	r.pull.Head = loopFixed
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopFixed, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	r.verdict("R2", loopFixed, superloop.ChangesRequested, "Review: changes requested\n\nstill broken")
	doc = r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopFixed)
	if doc.ExitCode != github.SquashRefused || len(r.cmds) != 0 {
		t.Fatalf("changes requested on the claimed head: %+v cmds=%v, want a refusal", doc, r.cmds)
	}
	if !strings.Contains(doc.Reason, "requested changes") {
		t.Fatalf("unexpected refusal reason: %q", doc.Reason)
	}
}

// Regression test for https://github.com/giantswarm/beekeeper/issues/2178:
// a verdict counts only from the desk's login or the repository's members:
// an outsider's "Review: approved" authorizes nothing, and an outsider's
// "Review: changes requested" holds nothing.
func TestMergeHeadAuthorityTrustsOnlyDeskAndMemberVerdicts(t *testing.T) {
	r := newLoopRig(t)
	p := superloop.Pull{Ref: loopPR, Head: loopFixed, Reviews: []superloop.Review{
		{ID: "R1", Author: "mallory", Head: loopFixed, Verdict: superloop.Approved, Body: loopApproved, At: time.Now()},
	}}
	if why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, p, loopFixed); err != nil || !strings.Contains(why, "no approved desk review") {
		t.Fatalf("outsider approval: %q, %v; want a refusal", why, err)
	}
	p.Reviews[0].Member = true
	if why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, p, loopFixed); err != nil || why != "" {
		t.Fatalf("member approval: %q, %v; want authorized", why, err)
	}
	p.Reviews = []superloop.Review{
		{ID: "R1", Author: loopDesk, Head: loopFixed, Verdict: superloop.Approved, Body: loopApproved, At: time.Now().Add(-time.Minute)},
		{ID: "R2", Author: "mallory", Head: loopFixed, Verdict: superloop.ChangesRequested, Body: "Review: changes requested", At: time.Now()},
	}
	if why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, p, loopFixed); err != nil || why != "" {
		t.Fatalf("desk approval past an outsider's changes requested: %q, %v; want authorized", why, err)
	}
}

// A superseded reviewer's late request for changes on the head it was
// given, posted after its author replaced that head, is no verdict at the
// gate either: the next round's approval of the current head still
// authorizes the merge. Posted before the push, it holds the merge.
// https://github.com/giantswarm/beekeeper/issues/3843
func TestMergeHeadAuthorityIgnoresAReviewOfAReplacedHead(t *testing.T) {
	r := newLoopRig(t)
	p := superloop.Pull{Ref: loopPR, Head: loopFixed, Reviews: []superloop.Review{
		{ID: "R2", Author: loopDesk, Head: loopFixed, Commit: loopFixed, Verdict: superloop.Approved, Body: loopApproved, At: time.Now().Add(-time.Minute)},
		{ID: "R1", Author: loopDesk, Head: loopHead, Commit: loopFixed, Verdict: superloop.ChangesRequested, Body: "Review: changes requested", At: time.Now()},
	}}
	if why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, p, loopFixed); err != nil || why != "" {
		t.Fatalf("approval past a replaced head's review: %q, %v; want authorized", why, err)
	}
	p.Reviews[1].Commit = loopHead
	if why, _, err := r.a.checkMergeHeadAuthority(t.Context(), nil, p, loopFixed); err != nil || why == "" {
		t.Fatalf("changes requested before the push: %q, %v; want a refusal", why, err)
	}
}

// Regression test for https://github.com/giantswarm/beekeeper/issues/2178:
// an issued execution merge grant names its head after " at ", behind a
// grant id whose first group is hex too; the grant is logged without a
// subject and counts as the exact-head go of the pull request its URL names.
func TestMergeHeadAuthorityReadsTheHeadOfAnIssuedGrant(t *testing.T) {
	const grant = "7a6aca80-a22d-4a36-b60a-182367592c4c"
	for text, want := range map[string]string{
		grant + " for https://github.com/o/r/pull/7 at " + loopFixed:                    loopFixed,
		"https://github.com/o/r/pull/7 at " + loopFixed + ": supervisor go checked":     loopFixed,
		"Merge https://github.com/o/r/pull/7 at head 1a2b3c4d5 without a third review?": "1a2b3c4d5",
		"20261103T101500Z at noon":            "",
		"answered at 20261103T101500Z":        "",
		grant + " for https://github.com/o/r": "",
	} {
		if got := headAfterAt(text); got != want {
			t.Errorf("headAfterAt(%q) = %q, want %q", text, got, want)
		}
	}

	r := newLoopRig(t)
	r.verdict("R1", loopHead, superloop.ChangesRequested, "Review: changes requested\n\nfix handler")
	p := r.pull
	p.Head = loopFixed
	ref, _ := parseRef(loopPR)
	if err := r.a.store.Record(event(loop.LoopParty, "execution.merge-issued", "%s for https://github.com/%s/pull/%d at %s", grant, ref.Repo, ref.N, loopFixed)); err != nil {
		t.Fatal(err)
	}
	why, explicit, err := r.a.checkMergeHeadAuthority(t.Context(), nil, p, loopFixed)
	if err != nil || why != "" || !explicit {
		t.Fatalf("issued grant: %q, %v, %v; want an explicit go", why, explicit, err)
	}
}

// An explicit exact-head supervisor.go authorizes reviewedMerge when no
// desk review requested changes since the approval.
func TestReviewedMergeAdmitsExplicitExactHeadSupervisorGo(t *testing.T) {
	r := newLoopRig(t)
	r.verdict("R1", loopHead, superloop.Approved, "Review: approved")
	r.pull.Head = loopFixed
	if err := r.a.store.Record(about(event(loop.LoopParty, "supervisor.go", "[go %s@%s] %s: %s", loopPR, loopFixed, loopWorker, loop.LoopWorkerMergeGo), loopPR)); err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopFixed, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopFixed)
	if doc.ExitCode != 0 || len(r.cmds) != 1 {
		t.Fatalf("expected merge with explicit supervisor.go, got %+v cmds=%v", doc, r.cmds)
	}
}

// A review or supervisor go can be withdrawn while merge-result checks run.
// Refresh both public reviews and durable desk policy before invoking gh.
func TestReviewedMergeRefreshesAuthorityBeforeMutation(t *testing.T) {
	const revokedGoCase = "revoked go"
	const changesRequestedCase = "changes requested"
	const unreadableCase = "unreadable authority"
	for _, scenario := range []string{litUnchanged, changesRequestedCase, "dismissed approval", revokedGoCase, "moved head", overrideClosed, "red CI", litDraft, conflictingWord, unreadableCase, reviewedStoppedCase, reviewedHeldCase} {
		t.Run(scenario, func(t *testing.T) {
			r := newLoopRig(t)
			r.verdict("R1", loopHead, superloop.Approved, loopApproved)
			if scenario == revokedGoCase {
				r.pull.Reviews = nil
				if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopGoVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
				t.Fatal(err)
			}
			// The queue lookup is the last read after merge-result validation,
			// including runs that skip local checks because no checkout exists.
			readReviewedMergeQueue = func(context.Context, superloop.Pull) (bool, error) {
				switch scenario {
				case changesRequestedCase:
					r.verdict("R2", loopHead, superloop.ChangesRequested, "Review: changes requested")
				case "dismissed approval":
					r.pull.Reviews = nil
				case revokedGoCase:
					if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopClaimEndVerb, "[go %s@%s] withdrawn", loopPR, loopHead), loopPR)); err != nil {
						t.Fatal(err)
					}
				case "moved head":
					r.pull.Head = loopFixed
				case overrideClosed:
					r.pull.State = superloop.Closed
				case "red CI":
					r.pull.Green = false
				case "draft":
					r.pull.Draft = true
				case conflictingWord:
					r.pull.Mergeable = loop.MergeableConflicting
				case unreadableCase:
					loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
						return superloop.Pull{}, errors.New("reviews unavailable")
					}
				case reviewedStoppedCase, reviewedHeldCase:
					if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
						if scenario == reviewedStoppedCase {
							st.Stop = &state.Stop{}
						} else {
							st.Holds = append(st.Holds, state.Hold{Target: loopMergeRepo, Reason: reviewedHeldCase})
						}
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil
			}
			doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopHead)
			if scenario == "unchanged" {
				if doc.ExitCode != 0 || len(r.cmds) != 1 {
					t.Fatalf("approved merge: %+v commands=%v", doc, r.cmds)
				}
				return
			}
			want := github.SquashRefused
			if scenario == unreadableCase {
				want = github.SquashTooling
			}
			if doc.ExitCode != want || len(r.cmds) != 0 {
				t.Fatalf("stale authority: %+v commands=%v; want exit %d without mutation", doc, r.cmds, want)
			}
		})
	}
}

// Reopened episode keys must retain both the claim and exact-head authority;
// ending that episode must revoke the go again.
func TestReopenedPullMergeAuthority(t *testing.T) {
	r := newLoopRig(t)
	r.verdict("R1", loopHead, superloop.Approved, "Review: approved")
	r.pull.Head = loopFixed
	suffix := " reopened " + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	for _, verb := range []string{loop.LoopGoVerb, loop.LoopClaimVerb} {
		if err := r.a.store.Record(about(event(loop.LoopParty, verb, "[go %s@%s%s] %s", loopPR, loopFixed, suffix, loopWorker), loopPR)); err != nil {
			t.Fatal(err)
		}
	}
	doc := r.a.reviewedMerge(t.Context(), loopMergeRepo, 7, loopFixed)
	if doc.ExitCode != 0 || len(r.cmds) != 1 {
		t.Fatalf("reopened go refused: %+v cmds=%v", doc, r.cmds)
	}
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopEndVerb, "[end %s%s] closed", loopPR, suffix), loopPR)); err != nil {
		t.Fatal(err)
	}
	if why, explicit, err := r.a.checkMergeHeadAuthority(t.Context(), nil, r.pull, loopFixed); err != nil || why == "" || explicit {
		t.Fatalf("closed episode retained authority: why=%q explicit=%v err=%v", why, explicit, err)
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopMergeIgnoresFailuresOfOtherHeads(t *testing.T) {
	for _, head := range []string{"", loopFixed} {
		t.Run("failed_head_"+head, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Failed = append(st.Failed, state.Failed{Repo: loopMergeRepo, PR: 7, Head: head, By: loop.LoopParty, Exit: github.SquashRed})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			if r.called(gate.GateCmdName) == nil {
				t.Fatalf("stale failure prevented gate validation: %v", r.calls)
			}
			if wake := r.called(agentsName, agentWakeName, loopWorker); wake != nil {
				t.Fatalf("stale failure requested branch repair: %v", wake)
			}
			if got := len(r.logged(t, "merge.failure-dropped")); got != 1 {
				t.Fatalf("stale failure drop logs = %d, want 1", got)
			}
		})
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopMergeStartAlwaysRecordsOutcome(t *testing.T) {
	const (
		confirmed     = "confirmed merge"
		laneWait      = "lane wait"
		policyRefusal = "policy refusal"
		branchRepair  = "branch repair"
	)
	for _, outcome := range []string{confirmed, laneWait, "gate refusal", policyRefusal, branchRepair, "confirmation read failure"} {
		t.Run(outcome, func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.Merge.Admin = []string{loopMergeRepo}
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			read := loop.ReadLoopPull
			r.answer = func(args []string) (string, int) {
				if len(args) == 0 || args[0] != gate.GateCmdName {
					return "", 0
				}
				switch outcome {
				case confirmed:
					r.pull.State, r.pull.MergeCommit = superloop.Merged, loopFixed
					return "", 0
				case laneWait:
					return "lane occupied by running merge", gate.ExitGateQueued
				case policyRefusal:
					return adminPolicyTestReason, 1
				case branchRepair:
					return fmt.Sprintf(`{"route":%q,"repository":%q,"number":7,"headSha":%q,"reason":"merge-result check failed"}`, reviewedMergeRoute, loopMergeRepo, loopHead), github.SquashRed
				case "confirmation read failure":
					loop.ReadLoopPull = func(context.Context, string) (superloop.Pull, error) {
						return superloop.Pull{}, errors.New("confirmation unavailable")
					}
					return "", 0
				default:
					return "held by release window", gate.ExitGateRefused
				}
			}
			r.pass(t)
			loop.ReadLoopPull = read
			starts := r.logged(t, "merge.start")
			var outcomes []state.Event
			for _, verb := range []string{"merge.finished", "merge.pending", loop.LoopMergeFailed} {
				outcomes = append(outcomes, r.logged(t, verb)...)
			}
			if len(starts) != 1 || len(outcomes) != 1 {
				t.Fatalf("unpaired merge attempt: starts=%v outcomes=%v", starts, outcomes)
			}
			want := loop.LoopMergeFailed
			switch outcome {
			case confirmed:
				want = "merge.finished"
			case laneWait, policyRefusal:
				want = "merge.pending"
			}
			if outcomes[0].Verb != want || outcomes[0].Subject != loopPR || !strings.Contains(outcomes[0].Detail, loopHead) {
				t.Fatalf("incorrect outcome: %+v", outcomes[0])
			}
			if len(r.logged(t, loop.LoopGoVerb)) != 0 || outcome != branchRepair && r.called(agentsName, agentWakeName, loopWorker) != nil {
				t.Fatal("merge outcome bypassed coordinator by waking worker")
			}
		})
	}
}

// Quota-only refusals wait on fresh GitHub reads, not exponential backoff.
// The same exact-head review is used once quota recovers.
//
// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopMergeRetriesUnreadGitHubWithoutBackoff(t *testing.T) {
	for _, code := range []int{gate.ExitGateUnread, gate.ExitGateRefused} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			attempts := 0
			r.answer = func(args []string) (string, int) {
				if len(args) > 0 && args[0] == gate.GateCmdName {
					attempts++
					if attempts <= 3 {
						return "the reviews cannot be read: API rate limit exceeded", code
					}
					r.pull.State, r.pull.MergeCommit = superloop.Merged, loopFixed
				}
				return "", 0
			}
			for range 4 {
				r.pass(t)
			}
			if attempts != 4 || len(r.logged(t, "merge.pending")) != 3 || len(r.logged(t, "merge.finished")) != 1 {
				t.Fatalf("quota recovery did not retry: attempts=%d output=%s", attempts, r.out)
			}
			if len(r.logged(t, loop.LoopFailureVerb)) != 0 || r.called(agentsName, agentWakeName, loopWorker) != nil {
				t.Fatalf("quota read created backoff or worker bypass: %s", r.out)
			}
		})
	}
}

// control: none: the gate extraction only changes names and access paths; assertions are unchanged.
// The merge base has no internal/gate package, so these tests cannot build there.
func TestLoopRetriesLegacyQuotaBackoff(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	key := superloop.KindGo + " " + loopPR + "@" + loopHead
	failure := loop.LoopFailure{Message: "merge gate refused: API rate limit exceeded", Count: 8, Kind: superloop.KindGo, Ref: loopPR, At: r.a.timeNow(), Since: r.a.timeNow(), RetryAt: r.a.timeNow().Add(time.Hour)}
	raw, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Record(about(event(loop.LoopParty, loop.LoopFailureVerb, "[%s] %s", key, raw), loopPR)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(gate.GateCmdName) == nil {
		t.Fatalf("legacy quota backoff still holds the gate: %s", r.out)
	}
}
