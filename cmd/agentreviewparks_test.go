package cmd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const pendingReviewWorktree = "/fixture/worktree"

const multiParkSecondPR = "o/r#8"

func TestReviewParksRetainEachPullRequest(t *testing.T) {
	w := parkingWatch(t, map[string]string{refOne: github.Open, refTwo: github.Open})
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	head := loopHead
	parkReviewHead = func(string) (string, string) { return head, parkVerificationUnavailable }
	me := state.Party{Name: agentOne}
	for _, ref := range []string{refOne, refTwo, refOne} {
		if _, err := w.Host.park(me, ref, deskReviewWait); err != nil {
			t.Fatal(err)
		}
		head = loopFixed
	}
	st, err := w.Host.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReviewParks) != 2 {
		t.Fatalf("lost or duplicated request: %+v", st.ReviewParks)
	}
	for _, ref := range []string{refOne, refTwo} {
		rp, ok := st.ReviewParkFor(ref)
		if !ok || rp.ReviewHead != loopFixed || rp.Agent != agentOne {
			t.Fatalf("request %s: %+v", ref, rp)
		}
	}
	// A plain wait cannot erase either independently proven request.
	parkReviewHead = func(string) (string, string) { return "", parkVerificationUnavailable }
	if _, err := w.Host.park(me, "#3", "answer"); err != nil {
		t.Fatal(err)
	}
	st, _ = w.Host.store.Read()
	if len(st.ReviewParks) != 2 {
		t.Fatalf("plain wait erased review requests: %+v", st.ReviewParks)
	}
}

func TestLoopReviewsEveryPullOfSameWorker(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	const second = multiParkSecondPR
	r.pulls[second] = superloop.Pull{Ref: second, URL: secondURL, State: superloop.Open, Head: loopHead, Green: true, Author: loopDesk}
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		ag := st.Agents[0]
		ag.Park = &state.Park{On: second, At: time.Now(), ReviewHead: loopHead}
		recordAgentReviewPark(st, ag)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	for _, ref := range []string{loopPR, second} {
		found := false
		for _, call := range r.calls {
			if len(call) > 1 && call[0] == agentsName && call[1] == agentStartName && strings.Contains(strings.Join(call, " "), superloop.ReviewerName(ref, 1)) {
				found = true
			}
		}
		if !found {
			t.Fatalf("review missing for %s: %v\n%s", ref, r.calls, r.out)
		}
	}
}

func TestMergedPullKeepsOtherReviewRequests(t *testing.T) {
	for _, roster := range []bool{false, true} {
		t.Run(map[bool]string{false: "off roster", true: "registered"}[roster], func(t *testing.T) {
			r := newLoopRig(t)
			r.park(t, loopPR, time.Now())
			r.pulls[multiParkSecondPR] = superloop.Pull{Ref: multiParkSecondPR, URL: secondURL, State: superloop.Merged, Head: loopHead, MergeCommit: loopFixed}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				ag := st.Agents[0]
				pm := ""
				ag.Park.PostMerge = &pm
				recordAgentReviewPark(st, ag)
				ag.Park = &state.Park{On: multiParkSecondPR, ReviewHead: loopHead, PostMerge: &pm}
				recordAgentReviewPark(st, ag)
				if roster {
					st.Agents[0].Park.PostMerge = &pm
				} else {
					st.Agents = nil
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, _ := r.a.store.Read()
			l := &loop.Loop{Host: r.a, Merged: map[string]superloop.Pull{loopPR: {MergeCommit: loopFixed}}}
			_, err := l.EndMerged(t.Context(), st, superloop.Action{Key: "test-end", Kind: superloop.KindEnd, Ref: loopPR, URL: loopURL, Message: strings.ToLower(superloop.Merged)})
			if err != nil {
				t.Fatal(err)
			}
			st, _ = r.a.store.Read()
			if len(st.ReviewParks) != 1 || st.ReviewParks[0].On != multiParkSecondPR || len(st.WorkerReports) != 0 || (roster && st.Agents[0].Done) {
				t.Fatalf("ended task before second PR: %+v", st)
			}
			_, err = l.EndMerged(t.Context(), st, superloop.Action{Key: "test-end-second", Kind: superloop.KindEnd, Ref: multiParkSecondPR, URL: secondURL, Message: strings.ToLower(superloop.Merged)})
			if err != nil {
				t.Fatal(err)
			}
			st, _ = r.a.store.Read()
			if len(st.ReviewParks) != 0 || len(st.WorkerReports) != 1 || (roster && !st.Agents[0].Done) {
				t.Fatalf("last merge did not finish task: %+v", st)
			}
		})
	}
}

func TestReviewParkPostMergeWorkStaysWithItsPullRequest(t *testing.T) {
	w := parkingWatch(t, map[string]string{refOne: github.Open, refTwo: github.Open})
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	parkReviewHead = func(string) (string, string) { return loopHead, parkVerificationUnavailable }
	me := state.Party{Name: agentOne}
	if _, err := w.Host.park(me, refOne, deskReviewWait, "release first PR"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Host.park(me, refTwo, deskReviewWait); err != nil {
		t.Fatal(err)
	}
	st, _ := w.Host.store.Read()
	first, _ := st.ReviewParkFor(refOne)
	second, _ := st.ReviewParkFor(refTwo)
	if first.PostMerge == nil || *first.PostMerge != "release first PR" || second.PostMerge == nil || *second.PostMerge != "" {
		t.Fatalf("post-merge work crossed PRs: first %+v second %+v", first, second)
	}
}

func TestLastMergedPullPreservesEarlierPostMergeWork(t *testing.T) {
	r := newLoopRig(t)
	r.park(t, loopPR, time.Now())
	r.pulls[multiParkSecondPR] = superloop.Pull{Ref: multiParkSecondPR, URL: secondURL, State: superloop.Merged, Head: loopHead, MergeCommit: loopFixed}
	work, empty := "prove the release", ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &work
		recordAgentReviewPark(st, st.Agents[0])
		other := st.Agents[0]
		other.Park = &state.Park{On: multiParkSecondPR, ReviewHead: loopHead, PostMerge: &empty}
		recordAgentReviewPark(st, other)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	l := &loop.Loop{Host: r.a, Merged: map[string]superloop.Pull{loopPR: {MergeCommit: loopFixed}}}
	st, err := r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	task := st.Agents[0].Task
	park := *st.Agents[0].Park
	end := func(ref, url string) {
		t.Helper()
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.EndMerged(t.Context(), st, superloop.Action{Key: "end " + ref, Kind: superloop.KindEnd, Ref: ref, URL: url, Message: strings.ToLower(superloop.Merged)}); err != nil {
			t.Fatal(err)
		}
	}
	end(loopPR, loopURL)
	wake := r.called(agentsName, agentWakeName, loopWorker)
	if wake == nil || !strings.Contains(wake[3], work) {
		t.Fatalf("post-merge work not requested: %v", r.calls)
	}
	st, err = r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ReviewParks) != 1 || st.ReviewParks[0].On != multiParkSecondPR {
		t.Fatalf("unexpected remaining review parks: %+v", st.ReviewParks)
	}
	end(multiParkSecondPR, secondURL)
	st, err = r.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	worker := st.Agents[0]
	if worker.Done || worker.Task != task || worker.Park == nil || worker.Park.PostMerge == nil || *worker.Park.PostMerge != work || worker.Park.On != park.On || worker.Park.ReviewHead != park.ReviewHead {
		t.Fatalf("last merge erased unfinished continuation: %+v", worker)
	}
	if len(st.ReviewParks) != 0 || len(st.WorkerReports) != 0 {
		t.Fatalf("last merge retained review records or reported completion: parks=%+v reports=%+v", st.ReviewParks, st.WorkerReports)
	}
}

func TestPendingReviewParkRecoversAfterGitHubFailure(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			r := newLoopRig(t)
			r.a.cfg.GitHub.AllowedOrgs = []string{"o"}
			old := loop.RecoverParkReviewHead
			t.Cleanup(func() { loop.RecoverParkReviewHead = old })
			readable := false
			loop.RecoverParkReviewHead = func(_ context.Context, on, dir string) string {
				if on != loopPR || dir != pendingReviewWorktree {
					t.Fatalf("verification %s in %s", on, dir)
				}
				if !readable {
					return ""
				}
				return loopHead
			}
			party := state.Party{Name: loopWorker, Session: "pending-review"}
			remaining := "prove after merge"
			p := &state.Park{On: loopPR, At: r.a.now, Waits: deskReviewWait, PostMerge: &remaining}
			if !legacy {
				p.Worktree = pendingReviewWorktree
			}
			if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents = []state.Agent{{Party: party, Task: loopIssueTask, Park: p}}
				st.Starts = append(st.Starts, state.Start{Party: party, Dir: pendingReviewWorktree})
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			l := &loop.Loop{Host: r.a}
			recover := func() *state.State {
				st, err := r.a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if err := l.RecoverReviewRequests(context.Background(), st); err != nil {
					t.Fatal(err)
				}
				st, err = r.a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				return st
			}
			st := recover()
			if st.Agents[0].Park.ReviewHead != "" || len(st.ReviewParks) != 0 {
				t.Fatal("unverified request gained review authority")
			}
			readable = true
			// A request that did not verify waits for its backoff, not the
			// next pass: each look starts git and reads GitHub.
			verify := loop.RecoverParkReviewHead
			loop.RecoverParkReviewHead = func(context.Context, string, string) string {
				t.Fatal("unverified request retried before its backoff")
				return ""
			}
			recover()
			loop.RecoverParkReviewHead = verify
			r.a.now = r.a.now.Add(loop.ReviewRecoveryFirst)
			st = recover()
			rp, ok := st.ReviewParkFor(loopPR)
			if !ok || rp.ReviewHead != loopHead || rp.PostMerge == nil || *rp.PostMerge != remaining {
				t.Fatalf("recovered request: %+v", rp)
			}
			loop.RecoverParkReviewHead = func(context.Context, string, string) string { t.Fatal("verified request retried"); return "" }
			recover()
		})
	}
}
