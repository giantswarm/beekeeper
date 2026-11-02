package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const reviewFixIssue = "https://github.com/o/r/issues/5"

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
