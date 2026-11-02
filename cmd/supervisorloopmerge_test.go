package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestLoopMergesReviewedHeadWithoutWorkerWake(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.command = func(argv []string) (string, int) {
		if slices.Contains(argv, "merge") {
			r.pull.State = superloop.Merged
			r.pull.MergeCommit = loopFixed
		}
		return "", 0
	}
	r.pass(t)
	var merged bool
	for _, argv := range r.cmds {
		if len(argv) > 3 && argv[0] == "gh" && argv[1] == "pr" && argv[2] == "merge" {
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
	r.a.cfg.Merge.Admin = []string{"o/r"}
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
	for _, scenario := range []string{"unclaimed", "moved", "red", "stopped", "held", "behind", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			if scenario != "unclaimed" {
				if err := r.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "behind":
				r.pull.MergeState = "BEHIND"
			case "conflict":
				r.pull.Mergeable = "CONFLICTING"
			case "moved":
				r.pull.Head = loopFixed
			case "red":
				r.pull.Green = false
			case "stopped":
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Stop = &state.Stop{}; return nil, nil }); err != nil {
					t.Fatal(err)
				}
			case "held":
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Holds = append(st.Holds, state.Hold{Target: "o/r", Reason: "held"})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			doc := r.a.reviewedMerge(t.Context(), "o/r", 7, loopHead)
			if doc.ExitCode == 0 || len(r.cmds) > 0 {
				t.Fatalf("unsafe mutation: doc %+v commands %v", doc, r.cmds)
			}
		})
	}
}

func TestReviewedMergeUsesConfiguredMethodAndGatePin(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.Methods = map[string]string{"o/r": "rebase"}
	r.a.cfg.Merge.Admin = []string{"o/r"}
	if err := r.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	doc := r.a.reviewedMerge(t.Context(), "o/r", 7, loopHead)
	if doc.ExitCode != 0 {
		t.Fatalf("merge %+v", doc)
	}
	argv := r.cmds[0]
	for _, flag := range []string{"--rebase", "--admin", "--match-head-commit", loopHead} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("missing %s in %v", flag, argv)
		}
	}
	g := gateRun{app: r.a, repo: "o/r", pr: 7, reviewedHead: loopHead, argv: []string{"devctl", "pr", "merge", "o/r", "7"}}
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

func TestReviewedAdminMergeRefusesANonStrictBehindBase(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Merge.Admin = []string{"o/r"}
	if err := r.a.store.Record(about(event(loopParty, loopClaimVerb, "[go %s@%s] %s", loopPR, loopHead, loopWorker), loopPR)); err != nil {
		t.Fatal(err)
	}
	was := readReviewedBehind
	t.Cleanup(func() { readReviewedBehind = was })
	readReviewedBehind = func(_ context.Context, _ superloop.Pull) (int, error) { return 1, nil }
	doc := r.a.reviewedMerge(t.Context(), "o/r", 7, loopHead)
	if doc.ExitCode != github.SquashNotApplicable || len(r.cmds) != 0 || !strings.Contains(doc.Reason, "behind") {
		t.Fatalf("behind admin merge: %+v %v", doc, r.cmds)
	}
}

func TestLoopEscalationRequiresVerifiedAnswerAndAcceptsGuideDialog(t *testing.T) {
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
	recorder := state.Party{Session: "guide-session", Name: verifyGuide}
	rec := recordedAnswer{About: loopPR, By: loopPerson, Channel: channelSession, Answer: "Merge", Note: 9}
	if err := r.a.store.Record(answerEvent(recorder, &rec, &n)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(gateCmdName) != nil {
		t.Fatal("an unverified agent relay released the merge")
	}
	rec.Channel = channelGuideDialog
	if err := r.a.store.Record(answerEvent(recorder, &rec, &n)); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	call := r.called(gateCmdName)
	if call == nil || !slices.Contains(call, third) {
		t.Fatalf("guide answer did not release its head: %v\n%s", r.calls, r.out)
	}
}
