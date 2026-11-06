package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const (
	reviewFixIssue     = "https://github.com/o/r/issues/5"
	reviewFixServedRef = "o/r#15"
	reviewFixServedURL = "https://github.com/o/r/issues/15"
)

func TestReviewFixTerminalParksWakeRemainingTasks(t *testing.T) {
	for _, scenario := range []string{"merged with open issue", "merged with unread issue", "closed pull request"} {
		t.Run(scenario, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			work := ""
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park.PostMerge = &work
				st.Records = append(st.Records, state.Record{Session: st.Agents[0].Party, Issue: reviewFixIssue})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			r.pull.State = superloop.Merged
			if scenario == "closed pull request" {
				r.pull.State = superloop.Closed
			}
			if scenario == "merged with unread issue" {
				refStates = func(context.Context, []github.PR) (map[github.PR]github.RefState, error) {
					return nil, errors.New("read unavailable")
				}
			}
			r.pass(t)
			if r.called(agentsName, agentWakeName, loopWorker) == nil {
				t.Fatalf("terminal PR stranded worker: %v\n%s", r.calls, r.out)
			}
			r.pass(t)
			if call := r.called(agentsName, agentWakeName, loopWorker); call != nil {
				t.Fatalf("duplicate terminal wake %v", call)
			}
		})
	}
}

func TestReviewFixMetadataBelongsOnlyToRosterReviewParks(t *testing.T) {
	for _, roster := range []bool{false, true} {
		for _, head := range []string{"", loopHead} {
			t.Run(fmt.Sprintf("registered=%t/head=%s", roster, head), func(t *testing.T) {
				w := parkingWatch(t, map[string]string{refOne: github.Open})
				was := parkReviewHead
				t.Cleanup(func() { parkReviewHead = was })
				parkReviewHead = func(string) string { return head }
				me := state.Party{Name: agentOne}
				if !roster {
					me = offSession
				}
				line, err := w.park(me, refOne, deskReviewWait, "")
				if err != nil {
					t.Fatal(err)
				}
				st, err := w.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				p := st.Agents[0].Park
				if !roster {
					p = &st.Parks[0]
				}
				want := roster && head != ""
				if (p.PostMerge != nil) != want {
					t.Fatalf("loop metadata on wrong park: %+v", p)
				}
				if !want && strings.Contains(line, "loop reviews and merges") {
					t.Fatalf("unowned park claims loop processing: %s", line)
				}
			})
		}
	}
}

func TestReviewFixOldOffRosterReviewParkStillSettles(t *testing.T) {
	w := parkingWatch(t, map[string]string{refOne: github.Merged})
	work := ""
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Parks = append(st.Parks, state.Park{By: offSession, On: refOne, At: relayNow, RefKind: github.RefPullRequest, ReviewHead: loopHead, PostMerge: &work})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	settled := w.settledParks(t.Context(), st)
	if !slices.ContainsFunc(settled, func(p settledPark) bool { return p.party.Is(offSession) }) {
		t.Fatalf("off-roster park stranded: %+v", settled)
	}
}

func TestReviewFixEndDoesNotFinishAPlainWaiter(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	work := ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &work
		st.Agents = append([]state.Agent{{Party: state.Party{Name: "plain waiter"}, Task: "other work", Park: &state.Park{On: loopPR, PostMerge: &work}}}, st.Agents...)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	l := &supervisorLoop{a: r.a}
	_, err = l.endMerged(t.Context(), st, superloop.Action{Kind: superloop.KindEnd, Key: "end " + loopPR, Ref: loopPR, URL: loopURL, Message: strings.ToLower(superloop.Merged)})
	if err != nil {
		t.Fatal(err)
	}
	st, err = r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[0].Done || !st.Agents[1].Done {
		t.Fatalf("wrong worker completed: %+v", st.Agents)
	}
}

func TestReviewFixQueuedMergeIsNotSubmittedAgain(t *testing.T) {
	for _, phase := range []string{state.Waiting, state.Running} {
		t.Run(phase, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			submissions := 0
			r.answer = func(args []string) (string, int) {
				if len(args) == 0 || args[0] != gateCmdName {
					return "", 0
				}
				submissions++
				if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Merges = append(st.Merges, state.Merge{Repo: loopMergeRepo, PR: 7, By: loopParty, Phase: phase, PID: os.Getpid(), Run: "queued fixture", Seen: time.Now()})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				return "", ExitGateQueued
			}
			r.pass(t)
			for range 2 {
				r.pass(t)
				if call := r.called(gateCmdName); call != nil {
					t.Fatalf("live gate submitted again: %v", call)
				}
			}
			if submissions != 1 || len(r.logged(t, loopFailureVerb)) != 0 {
				t.Fatalf("queue failed or resubmitted: %d %v", submissions, r.logged(t, loopFailureVerb))
			}
			r.pull.State, r.pull.MergeCommit = superloop.Merged, loopFixed
			r.pass(t)
			if r.called(gateCmdName) != nil || len(r.logged(t, "supervisor.merge")) != 1 {
				t.Fatalf("queued completion not reconciled: %v\n%s", r.calls, r.out)
			}
		})
	}
}

func TestReviewFixGateRefusalsNeverRequestBranchRepair(t *testing.T) {
	for _, tc := range []struct {
		code    int
		message string
	}{
		{ExitGateQueued, "gate queued"},
		{ExitGateDuplicate, "is already queued in lane"},
		{ExitGateDuplicate, "is already merging"},
		{ExitGateRefused, "waited for its turn behind the running merge"},
		{ExitGateRefused, "held: conflict with the release window"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pass(t)
			r.verdict("R1", loopHead, superloop.Approved, "")
			r.answer = func(args []string) (string, int) {
				if len(args) > 0 && args[0] == gateCmdName {
					return tc.message, tc.code
				}
				return "", 0
			}
			r.pass(t)
			if call := r.called(agentsName, agentWakeName, loopWorker); call != nil {
				t.Fatalf("gate wait/refusal woke worker for repair: %v", call)
			}
			if tc.code == ExitGateQueued || tc.code == ExitGateDuplicate {
				if len(r.logged(t, loopFailureVerb)) != 0 {
					t.Fatal("queue wait recorded as failure")
				}
			}
		})
	}
}

func TestReviewFixBranchFailureKeepsStructuredResultWithGateLogs(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pass(t)
	r.verdict("R1", loopHead, superloop.Approved, "")
	r.answer = func(args []string) (string, int) {
		if len(args) == 0 || args[0] != gateCmdName {
			return "", 0
		}
		raw, err := json.Marshal(github.SquashDoc{Route: reviewedMergeRoute, Repo: loopMergeRepo, Number: 7, HeadSha: loopHead, ExitCode: github.SquashNotApplicable, Reason: "branch behind its base"})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n" + GatePrefix + "nothing merged; repair needed\n", github.SquashNotApplicable
	}
	r.pass(t)
	call := r.called(agentsName, agentWakeName, loopWorker)
	if call == nil || !strings.Contains(call[3], "branch behind its base") {
		t.Fatalf("gate diagnostics hid the mutation result: %v\n%s", r.calls, r.out)
	}
}

func TestReviewFixEndsWorkerFreesSlotStartsFixerAndMerges(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.ReviewRounds = 2
	r.pull.Branch = "fix-2079-branch"
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "worker-session-1")
	work := ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &work
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Pass 1: the review-parked worker ends (removed from st.Agents, saved in st.ReviewParks)
	// and the reviewer starts in the freed slot.
	r.pass(t)
	if r.called(agentsName, loopRemove, loopWorker) == nil {
		t.Fatalf("review-parked worker was not ended: %v\n%s", r.calls, r.out)
	}
	if start := r.called(agentsName, agentStartName, loopReviewer); start == nil {
		t.Fatalf("reviewer did not start after worker ended: %v\n%s", r.calls, r.out)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 0 || len(st.ReviewParks) != 1 || st.ReviewParks[0].ReviewHead != loopHead {
		t.Fatalf("expected ended worker in ReviewParks, got Agents=%+v ReviewParks=%+v", st.Agents, st.ReviewParks)
	}

	// Pass 2: R1 requests changes; since no worker is waiting, the loop starts a fresh fixer on the PR's branch.
	r.verdict("R1", loopHead, superloop.ChangesRequested, "fix the nil check in handler.go:42")
	r.pass(t)
	fixerStart := r.called(agentsName, agentStartName, loopWorker)
	if fixerStart == nil {
		t.Fatalf("ChangesRequested did not start a fixer worker: %v\n%s", r.calls, r.out)
	}
	briefBytes, err := os.ReadFile(fixerStart[3])
	if err != nil {
		t.Fatal(err)
	}
	briefText := string(briefBytes)
	if !strings.Contains(briefText, "fix-2079-branch") || !strings.Contains(briefText, "fix the nil check in handler.go:42") {
		t.Fatalf("fixer brief missing branch or findings:\n%s", briefText)
	}

	// Pass 3: the fixer pushes loopFixed and parks for desk review; it ends and R2 starts.
	r.pull.Head = loopFixed
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		party := state.Party{Name: loopWorker, Session: "worker-session-2"}
		st.Agents = append(st.Agents, state.Agent{Party: party, Task: "fix " + loopURL, Park: &state.Park{On: loopPR, At: time.Now().Add(time.Minute), Waits: deskReviewWait, ReviewHead: loopFixed, PostMerge: &work}})
		st.Starts = append(st.Starts, state.Start{Party: party, By: loopParty, At: time.Now().Add(time.Minute)})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, loopRemove, loopWorker) == nil {
		t.Fatalf("fixer worker was not ended on review park: %v\n%s", r.calls, r.out)
	}
	reviewer2 := superloop.ReviewerName(loopPR, 2)
	if r.called(agentsName, agentStartName, reviewer2) == nil {
		t.Fatalf("second review round did not start: %v\n%s", r.calls, r.out)
	}

	// Pass 4: R2 approves; the loop merges through gate (--reviewed-head) with no worker waiting.
	r.verdict("R2", loopFixed, superloop.Approved, "")
	r.pass(t)
	if r.called(gateCmdName) == nil || r.lastMerged != loopFixed {
		t.Fatalf("approved second round did not merge: %v\n%s", r.calls, r.out)
	}
}

func TestReviewFixPreservesPostMergeAndStartsLocalContinuationFromSurvivingRef(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Supervisor.ReviewRounds = 2
	r.sshLoopHost(t, true)
	host := r.a.cfg.Agents.Hosts[sshTestSelector]
	host.Repositories[loopMergeRepo] = "/srv/repo"
	r.a.cfg.Agents.Hosts[sshTestSelector] = host
	wasDefaultBranch := ghDefaultBranch
	t.Cleanup(func() { ghDefaultBranch = wasDefaultBranch })
	ghDefaultBranch = func(_ context.Context, _ string) (string, error) {
		return mainBranch, nil
	}
	r.pull.Branch = "fix-2079-deleted-after-merge"
	r.pull.Base = mainBranch
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "worker-session-1")
	postMergeWork := "prove v0.175.0 rollout on live cluster"
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &postMergeWork
		st.Records = append(st.Records, state.Record{Session: st.Agents[0].Party, Issue: reviewFixServedRef})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Pass 1: worker ends into ReviewParks and R1 starts.
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReviewParks) != 1 || st.ReviewParks[0].Serves != reviewFixServedRef || st.ReviewParks[0].PostMerge == nil || *st.ReviewParks[0].PostMerge != postMergeWork {
		t.Fatalf("ReviewPark lost Serves or PostMerge: %+v", st.ReviewParks)
	}

	// Pass 2: R1 requests changes; fixer starts on the PR branch with original task and post-merge metadata.
	r.verdict("R1", loopHead, superloop.ChangesRequested, "fix handler.go:42")
	r.answer = func(args []string) (string, int) {
		if len(args) >= 1 && args[0] == loopCapacity {
			if slices.Contains(args, "--host") {
				return `{"room":1}`, 0
			}
			return `{"room":0}`, 0
		}
		return "", 0
	}
	r.pass(t)
	fixerStart := r.called(agentsName, agentStartName, loopWorker)
	if fixerStart == nil || !slices.Contains(fixerStart, "fix-2079-deleted-after-merge") {
		t.Fatalf("expected remote fixer start on PR branch, got %v", fixerStart)
	}
	fixerBriefBytes, err := os.ReadFile(fixerStart[3])
	if err != nil {
		t.Fatal(err)
	}
	fixerBrief := string(fixerBriefBytes)
	if !strings.Contains(fixerBrief, reviewFixServedURL) || !strings.Contains(fixerBrief, postMergeWork) {
		t.Fatalf("fixer brief missing original task URL or post-merge work:\n%s", fixerBrief)
	}

	// Pass 3: the fixer registers and parks with a PLAIN park (no --post-merge flag), as its brief or a worker might.
	r.pull.Head = loopFixed
	wasReviewHead := parkReviewHead
	t.Cleanup(func() { parkReviewHead = wasReviewHead })
	parkReviewHead = func(string) string { return loopFixed }
	fixerParty := state.Party{Name: loopWorker, Session: "worker-session-2"}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: fixerParty, Task: "fix " + loopURL})
		st.Starts = append(st.Starts, state.Start{Party: fixerParty, By: loopParty, At: time.Now().Add(time.Minute), Dir: t.TempDir()})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.park(fixerParty, loopPR, deskReviewWait); err != nil {
		t.Fatal(err)
	}
	r.a.now = time.Now().Add(2 * time.Minute)
	r.pass(t)
	st, err = r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReviewParks) != 1 || st.ReviewParks[0].Serves != reviewFixServedRef || st.ReviewParks[0].PostMerge == nil || *st.ReviewParks[0].PostMerge != postMergeWork {
		t.Fatalf("plain fixer park lost original Serves or PostMerge: %+v", st.ReviewParks)
	}

	// Pass 4: R2 approves and merges. The continuation waits for local
	// room even though the execution host has room.
	r.verdict("R2", loopFixed, superloop.Approved, "")
	r.pass(t)
	r.pull.State = superloop.Merged
	r.pull.MergeCommit = "9999999999999999999999999999999999999999"
	r.pass(t)
	if start := r.called(agentsName, agentStartName, loopWorker); start != nil {
		t.Fatalf("full coordinator started continuation: %v", start)
	}
	r.answer = func(args []string) (string, int) {
		if args[0] == loopCapacity {
			if slices.Contains(args, loopHostFlag) {
				t.Fatal("continuation probed an execution host")
			}
			return loopRoomJSON, 0
		}
		return "", 0
	}
	r.a.cfg.Supervisor.HostsFirst = true
	r.pass(t)
	contStart := r.called(agentsName, agentStartName, loopWorker)
	if contStart == nil {
		t.Fatalf("expected post-merge continuation start, got calls=%v\n%s", r.calls, r.out)
	}
	for _, flag := range []string{loopHostFlag, loopRepoFlag, loopRefFlag, "--" + excludeFlag} {
		if slices.Contains(contStart, flag) {
			t.Errorf("local continuation has placement flag %s: %v", flag, contStart)
		}
	}
	contBriefBytes, err := os.ReadFile(contStart[3])
	if err != nil {
		t.Fatal(err)
	}
	contBrief := string(contBriefBytes)
	if !strings.Contains(contBrief, "Fetch origin and work from "+mainBranch) || !strings.Contains(contBrief, reviewFixServedURL) || !strings.Contains(contBrief, postMergeWork) || strings.Contains(contBrief, "request the next review round") {
		t.Fatalf("continuation brief did not use continuation instructions:\n%s", contBrief)
	}
}

func TestEndReviewWorkersPersistsReviewParkBeforeRosterRemoval(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "worker-session-1")
	postMergeWork := "verify deployment"
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &postMergeWork
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate `agents remove` durably removing the roster entry on disk and then failing/interrupting before endReviewWorkers finishes.
	failedOnce := false
	r.answer = func(args []string) (string, int) {
		if len(args) >= 2 && args[0] == agentsName && args[1] == loopRemove && !failedOnce {
			failedOnce = true
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = nil
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			return "interrupted after roster removal", 1
		}
		return "", 0
	}
	r.pass(t)
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 0 || len(st.ReviewParks) != 1 || st.ReviewParks[0].ReviewHead != loopHead {
		t.Fatalf("review handoff was lost across roster removal interruption: Agents=%+v ReviewParks=%+v", st.Agents, st.ReviewParks)
	}
	if r.called(agentsName, agentStartName, loopReviewer) == nil {
		t.Fatalf("reviewer did not start from persisted ReviewPark after interruption: %v\n%s", r.calls, r.out)
	}
}

func TestReviewFixEndedWorkerNeedsRepair(t *testing.T) {
	for _, reason := range []string{"conflicting base", "failed CI"} {
		t.Run(reason, func(t *testing.T) {
			r := newLoopRig(t)
			r.pull.Branch = "repair-branch"
			r.park(t, loopPR, time.Now())
			r.started(t, loopWorker, "ended-worker")
			work := ""
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park.PostMerge = &work
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			if r.called(agentsName, loopRemove, loopWorker) == nil {
				t.Fatal("worker did not end")
			}
			r.pull.Green = false
			if reason == "conflicting base" {
				r.pull.MergeState = "DIRTY"
			} else {
				r.pull.Red = []superloop.Check{{Name: "unit checks"}}
			}
			r.pass(t)
			start := r.called(agentsName, agentStartName, loopWorker)
			if start == nil {
				t.Fatalf("no repair worker: %v\n%s", r.calls, r.out)
			}
			brief, err := os.ReadFile(start[3])
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{r.pull.Branch, loopURL, loopHead} {
				if !strings.Contains(string(brief), want) {
					t.Fatalf("repair brief lacks %q: %s", want, brief)
				}
			}
			if r.called(agentsName, agentWakeName, loopWorker) != nil {
				t.Fatal("tried waking ended worker")
			}
			r.pass(t)
			if r.called(agentsName, agentStartName, loopWorker) != nil {
				t.Fatal("duplicate repair start")
			}
		})
	}
}

// A watch memory stop ends the process, not the parked worker's ownership.
func TestLoopKeepsBackgroundReviewWorkerAfterMemoryStop(t *testing.T) {
	for _, postMerge := range []bool{false, true} {
		t.Run(strconv.FormatBool(postMerge), func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.started(t, loopWorker, "memory-stopped-worker")
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Starts[0].Background = true
				st.Starts[0].Dir = t.TempDir()
				if postMerge {
					work := "prove rollout"
					st.Agents[0].Park.PostMerge = &work
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			r.pass(t)
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Name == loopWorker })
			if i < 0 || st.Agents[i].Park == nil || st.Agents[i].Park.ReviewHead != loopHead {
				t.Fatalf("memory-stopped worker lost its park: %+v", st.Agents)
			}
			if r.called(agentsName, loopRemove, loopWorker) != nil {
				t.Fatal("parked worker removed")
			}
			if r.called(agentsName, agentStartName, loopReviewer) == nil {
				t.Fatalf("review not served: %v\n%s", r.calls, r.out)
			}
		})
	}
}

func TestLoopReviewsParkWhileWorkerTurnStillRuns(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.started(t, loopWorker, "active-review-worker")
	r.inTurn = func(p state.Party) bool { return p.Name == loopWorker }
	work := "prove rollout"
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &work
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	if r.called(agentsName, loopRemove, loopWorker) != nil {
		t.Fatal("active worker removed")
	}
	if r.called(agentsName, agentStartName, loopReviewer) == nil {
		t.Fatalf("review waits on worker retirement: %v\n%s", r.calls, r.out)
	}
}

func TestContinuationWaitsWhenCoordinatorCapacityCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		code int
	}{
		{name: "empty capacity response"},
		{name: "capacity command failure", out: loopRoomJSON, code: 1},
		{name: handoverMalformedMode, out: "{"},
		{name: "missing room", out: "{}"},
		{name: "full", out: `{"room":0}`},
		{name: "negative", out: `{"room":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			r.sshLoopHost(t, true)
			r.a.cfg.Supervisor.HostsFirst = true
			r.answer = func(args []string) (string, int) {
				if args[0] == loopCapacity {
					if slices.Contains(args, loopHostFlag) {
						t.Fatal("continuation probed an execution host")
					}
					return tc.out, tc.code
				}
				return "", 0
			}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			l := &supervisorLoop{a: r.a, said: map[string]bool{}}
			err = l.startFixer(t.Context(), st, superloop.Action{Kind: superloop.KindEnd, Ref: loopPR, Agent: loopWorker, Key: "continuation"})
			if !errors.Is(err, errLoopLater) {
				t.Fatalf("unreadable coordinator capacity: %v", err)
			}
			if start := r.called(agentsName, agentStartName); start != nil {
				t.Fatalf("started without coordinator capacity: %v", start)
			}
		})
	}
}

func TestLoopPrunesEndedReviewParksAndStartsActiveReviewUnderAllowanceHold(t *testing.T) {
	r := newLoopRig(t)
	now := r.a.now
	const (
		endedPR      = "o/r#2"
		unreviewedPR = "o/r#3"
		otherPR      = "o/r#9"
	)
	r.pulls[unreviewedPR] = superloop.Pull{Ref: unreviewedPR, URL: "https://github.com/o/r/pull/3", State: superloop.Merged, Head: loopHead, MergeCommit: loopFixed, Author: loopDesk}
	r.pulls[otherPR] = superloop.Pull{Ref: otherPR, URL: "https://github.com/o/r/pull/9", State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.ReviewParks = []state.ReviewPark{
			{On: endedPR, At: now.Add(-40 * time.Second), ReviewHead: loopHead, Agent: "ended-worker", Party: state.Party{Name: "ended-worker", Host: "ended-id"}},
			{On: unreviewedPR, At: now.Add(-20 * time.Second), ReviewHead: loopHead, Agent: "merged-worker", Party: state.Party{Name: "merged-worker", Host: "merged-id"}},
		}
		st.Agents = append(st.Agents, state.Agent{
			Party: state.Party{Name: "plain-waiter", Host: "plain-id"},
			Park:  &state.Park{On: otherPR, At: now.Add(-10 * time.Second), Waits: "overlap: waiting for o/r#9"},
		})
		return []state.Event{
			{At: now.Add(-35 * time.Second), By: loopParty, Verb: "supervisor.end", Subject: endedPR, Detail: "[end " + endedPR + "] https://github.com/o/r/pull/2 merged: tracking ends"},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.park(t, loopPR, now)
	var readOrder []string
	prevRead := readLoopPull
	readLoopPull = func(ctx context.Context, ref string) (superloop.Pull, error) {
		readOrder = append(readOrder, ref)
		if ref == otherPR {
			return superloop.Pull{}, errLoopLater
		}
		return prevRead(ctx, ref)
	}
	r.pass(t)
	if slices.Contains(readOrder, endedPR) {
		t.Fatalf("an ended ReviewPark was read from GitHub: %v", readOrder)
	}
	if len(readOrder) == 0 || readOrder[0] != loopPR {
		t.Fatalf("active live review park was not observed first: %v", readOrder)
	}
	if r.called(agentsName, agentStartName, loopReviewer) == nil {
		t.Fatalf("reviewer did not start when later PR hit allowance hold: %v\n%s", r.calls, r.out)
	}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{endedPR, unreviewedPR} {
		if _, ok := st.ReviewParkFor(stale); ok {
			t.Fatalf("stale ReviewPark %s remained in state: %+v", stale, st.ReviewParks)
		}
	}
}
