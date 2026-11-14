package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

const parkVerificationUnavailable = "verification unavailable"

const (
	parkTask   = "beekeeper#86: agents park"
	resumer    = "the supervisor"
	parkAnswer = "yes, roll it after 18:00, not before"
)

// parkingWatch is a running watch over a roster of agentOne with a task and
// note #3 open for Alex, the calling session agentOne; GitHub answers refs.
func parkingWatch(t *testing.T, refs map[string]string) *Watcher {
	t.Helper()
	w, _ := overtakingWatch(t, refs)
	beforeREST := readParkRefREST
	t.Cleanup(func() { readParkRefREST = beforeREST })
	readParkRefREST = func(context.Context, github.PR) (github.RefState, error) {
		return github.RefState{}, errParkLookup
	}

	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: parkTask, AssignedAt: relayNow}}
		st.Notes = []state.Note{{ID: 3, Kind: noteDecision, For: personTimo, Text: askedQ, By: state.Party{Name: agentOne}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func park(t *testing.T, w *Watcher, args ...string) error {
	t.Helper()
	c := watchApp(w.Host).agentParkCmd()
	c.SetArgs(args)
	c.SetOut(watchApp(w.Host).out)
	c.SetErr(watchApp(w.Host).out)
	return c.Execute()
}

func agentOf(t *testing.T, w *Watcher) state.Agent {
	t.Helper()
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st.Agents[0]
}

// answer closes note #3 with Alex's answer, as note answer does.
func answer(t *testing.T, w *Watcher) {
	t.Helper()
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		n := st.Notes[0]
		st.Notes = nil
		rec := &recordedAnswer{About: privateThread, By: personTimo, Via: viaCLI, Answer: parkAnswer, Note: n.ID}
		return []state.Event{answerEvent(state.Party{Name: personTimo, Person: personTimo}, rec, &n)}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// stubWakes records every wake instead of waking.
func stubWakes(t *testing.T) *[]string {
	t.Helper()
	var woke []string
	was := wakeOwner
	wakeOwner = func(_ *app, _ context.Context, by state.Party, q, msg, _ string) error {
		woke = append(woke, by.Name+" → "+q+": "+msg)
		return nil
	}
	t.Cleanup(func() { wakeOwner = was })
	return &woke
}

// A parked agent keeps its task and counts parked, not busy; one without a
// task, or parked on a note that is not open, is refused.
func TestParkCountsTheAgentParked(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := park(t, w, "--on", "#9", "an answer"); err == nil || !strings.Contains(err.Error(), "note #9 is not open") {
		t.Errorf("park on a closed note: %v", err)
	}
	if err := park(t, w, "--on", "3", "Alex's answer on the rollout"); err != nil {
		t.Fatal(err)
	}
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if want := fmt.Sprintf("beekeeper agents resume %q", agentOne); !strings.Contains(out, want) {
		t.Errorf("park output lacks the copy-pasteable %s: %q", want, out)
	}
	ag := agentOf(t, w)
	if ag.Task != parkTask || ag.Park == nil || ag.Park.On != "#3" {
		t.Fatalf("agent %+v park %+v", ag, ag.Park)
	}
	st, _ := watchApp(w.Host).store.Read()
	c := countAgents(st, nil, relayNow, nil)
	if len(c.Busy) != 0 || len(c.Parked) != 1 || !strings.Contains(c.Parked[0], "parked on #3: Alex's answer on the rollout") {
		t.Errorf("count %+v", c)
	}

	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		reportIdle(&st.Agents[0], relayNow)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ag := agentOf(t, w); ag.Park != nil {
		t.Errorf("agents idle left the park %+v", ag.Park)
	}
	if err := park(t, w, "the supervisor's go"); err == nil || !strings.Contains(err.Error(), "has no task") {
		t.Errorf("park without a task: %v", err)
	}
}

// Answering the note an agent parked on says AGENT RESUMABLE once; with
// agents.autoResume the next poll resumes it once with the answer word for
// word in a fresh continuation and retains the park until startup.
func TestAnsweredNoteResumesTheParkedAgentOnce(t *testing.T) {
	w := parkingWatch(t, nil)
	watchApp(w.Host).cfg.Agents.AutoResume = true
	woke := stubWakes(t)
	if err := park(t, w, "--on", "#3", "Alex's answer"); err != nil {
		t.Fatal(err)
	}
	w.Pending(context.Background(), nil)
	if strings.Contains(watchApp(w.Host).out.(*bytes.Buffer).String(), "AGENT RESUMABLE") || len(*woke) > 0 {
		t.Fatalf("resumable with its note open: %s %q", watchApp(w.Host).out, *woke)
	}
	answer(t, w)
	for range 3 {
		w.Pending(context.Background(), nil)
	}
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if n := strings.Count(out, `AGENT RESUMABLE "`); n != 1 {
		t.Errorf("%d AGENT RESUMABLE lines:\n%s", n, out)
	}
	if !strings.Contains(out, `AGENT RESUMABLE "Agent one": parked on #3: Alex's answer; resumable since`) {
		t.Errorf("line:\n%s", out)
	}
	assertFreshQueue(t, w, *woke, parkAnswer)
}

// Without agents.autoResume the watch only says the park resumable; a
// merged pull request settles a park on it, and agents resume wakes it with
// the merge.
func TestMergedPullRequestMakesTheParkResumable(t *testing.T) {
	refs := map[string]string{refOne: github.Open}
	w := parkingWatch(t, refs)
	woke := stubWakes(t)
	if err := park(t, w, "--on", "https://github.com/o/r/pull/1", "the merge ahead of mine"); err != nil {
		t.Fatal(err)
	}
	w.Pending(context.Background(), nil)
	refs[refOne] = github.Merged
	w.Pending(context.Background(), nil)
	w.Pending(context.Background(), nil)
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if n := strings.Count(out, `AGENT RESUMABLE "`); n != 1 || !strings.Contains(out, `o/r#1 merged; beekeeper agents resume "Agent one"`) {
		t.Errorf("%d lines:\n%s", n, out)
	}
	if len(*woke) > 0 {
		t.Fatalf("resumed without agents.autoResume: %q", *woke)
	}
	if err := watchApp(w.Host).resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err != nil {
		t.Fatal(err)
	}
	assertFreshQueue(t, w, *woke, "o/r#1 merged")
	if err := watchApp(w.Host).resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err != nil {
		t.Fatalf("idempotent continuation delivery: %v", err)
	}
	assertFreshQueue(t, w, *woke, "o/r#1 merged")
}

// --as <name> acts as the registered agent of that name for agents park and
// agents idle, though a session registered it: a session of another harness
// or a wrapper script parks and reports as its agent without a harness's
// session variables. A managed SSH execution's entry is not reached by name.
func TestAsNamesTheRegisteredAgent(t *testing.T) {
	const session = "5f0c2a7e-0000-4000-8000-000000000001"
	w := parkingWatch(t, nil)
	agent := state.Party{Session: session, HostSession: "local_" + session, Name: agentOne}
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Party = agent
		st.Records = []state.Record{{Session: agent}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := park(t, w, "--on", "3", "Alex's answer on the rollout"); err != nil {
		t.Fatalf("park --as %s: %v", agentOne, err)
	}
	st, _ := watchApp(w.Host).store.Read()
	if ag := st.Agents[0]; ag.Park == nil || ag.Park.On != "#3" || len(st.Parks) != 0 {
		t.Fatalf("agent %+v park %+v, parks off the roster %+v", ag, ag.Park, st.Parks)
	}
	if st.Records[0].Waits != "Alex's answer on the rollout" {
		t.Errorf("the agent's session record waits %q", st.Records[0].Waits)
	}
	c := watchApp(w.Host).agentsCmd()
	c.SetOut(watchApp(w.Host).out)
	c.SetErr(watchApp(w.Host).out)
	c.SetArgs([]string{stateIdle, flagDone, flagReport, "https://github.com/giantswarm/beekeeper/issues/86 done", flagProblem, noProblems})
	if err := c.Execute(); err != nil {
		t.Fatalf("idle --done --as %s: %v", agentOne, err)
	}
	if ag := agentOf(t, w); !ag.Done || ag.Task != "" || ag.Session != session {
		t.Errorf("agent after idle --done %+v", ag)
	}

	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: agent, Task: parkTask, AssignedAt: relayNow, Execution: &execution.Ref{}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := park(t, w, "--on", "3", "an answer"); err == nil || !strings.Contains(err.Error(), "no session id") {
		t.Errorf("park --as an SSH execution's entry: %v", err)
	}
}

// A park without --on is resumed by hand, its turn naming who resumed it.
func TestResumeByHand(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if err := park(t, w, "the supervisor's go"); err != nil {
		t.Fatal(err)
	}
	if err := watchApp(w.Host).resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err != nil {
		t.Fatal(err)
	}
	assertFreshQueue(t, w, *woke, "resumed you by hand")
}

// resume runs agents resume as the calling session (agentOne) with args.
func resume(t *testing.T, w *Watcher, args ...string) error {
	t.Helper()
	c := watchApp(w.Host).agentResumeCmd()
	c.SetArgs(args)
	c.SetOut(watchApp(w.Host).out)
	c.SetErr(watchApp(w.Host).out)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

// A parked agent leaves its own park once the wait settled: agents resume
// without an agent, or naming itself, ends the park, wakes nobody and prints
// what settled it.
func TestParkedAgentLeavesItsOwnPark(t *testing.T) {
	for _, args := range [][]string{nil, {agentOne}} {
		w := parkingWatch(t, nil)
		woke := stubWakes(t)
		if err := park(t, w, "--on", "#3", "Alex's answer"); err != nil {
			t.Fatal(err)
		}
		answer(t, w)
		w.Pending(context.Background(), nil)
		watchApp(w.Host).out.(*bytes.Buffer).Reset()
		if err := resume(t, w, args...); err != nil {
			t.Fatalf("resume %q: %v", args, err)
		}
		out := watchApp(w.Host).out.(*bytes.Buffer).String()
		want := agentOne + ": your park ended, nobody is woken. Your park on #3 (Alex's answer) is over: note #3 answered by Alex: " + parkAnswer
		if !strings.Contains(out, want) {
			t.Errorf("resume %q said:\n%s\nwant %q", args, out, want)
		}
		if len(*woke) > 0 {
			t.Errorf("resume %q woke %q", args, *woke)
		}
		if ag := agentOf(t, w); ag.Park != nil || ag.Task != parkTask {
			t.Errorf("resume %q: task %q park %+v", args, ag.Task, ag.Park)
		}
		if err := resume(t, w, args...); err == nil || !strings.Contains(err.Error(), "is not parked") {
			t.Errorf("second resume %q: %v", args, err)
		}
	}
}

// An agent of a harness beekeeper has no message route to (one that
// registered itself with agents register --harness, not started under its
// harness's host prefix) is never woken headless: its park line and the
// watch's AGENT RESUMABLE say so, agents.autoResume leaves it resumable,
// and agents resume by another ends its park and says so.
func TestResumeOfAnUnroutedHarnessEndsTheParkUnwoken(t *testing.T) {
	w := parkingWatch(t, nil)
	watchApp(w.Host).cfg.Agents.AutoResume = true
	woke := stubWakes(t)
	watchApp(w.Host).as = ""
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-omp-rpc")
	t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_NAME", agentOne)
	register := func(args ...string) error {
		c := watchApp(w.Host).agentsCmd()
		c.SetArgs(append([]string{registerVerb}, args...))
		c.SetOut(watchApp(w.Host).out)
		c.SetErr(watchApp(w.Host).out)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	if err := register("--harness", "pi"); Code(err) != ExitUsage {
		t.Fatalf("register --harness pi: %v", err)
	}
	if err := register("--harness", omp.Harness); err != nil {
		t.Fatal(err)
	}
	if ag := agentOf(t, w); ag.Harness != omp.Harness || ag.Session != "s-omp-rpc" || ag.Task != parkTask {
		t.Fatalf("registered %+v", ag)
	}
	if err := park(t, w, "--on", "#3", "Alex's answer"); err != nil {
		t.Fatal(err)
	}
	if out := watchApp(w.Host).out.(*bytes.Buffer).String(); !strings.Contains(out, unroutedResume) || strings.Contains(out, "the watch resumes it") {
		t.Errorf("park said:\n%s", out)
	}
	answer(t, w)
	watchApp(w.Host).out.(*bytes.Buffer).Reset()
	for range 2 {
		w.Pending(context.Background(), nil)
	}
	if out := watchApp(w.Host).out.(*bytes.Buffer).String(); !strings.Contains(out, `AGENT RESUMABLE "`+agentOne+`"`) || !strings.Contains(out, unroutedResume) ||
		strings.Contains(out, "the watch resumes it") {
		t.Errorf("watch said:\n%s", out)
	}
	if ag := agentOf(t, w); len(*woke) > 0 || ag.Park == nil || ag.Park.Resumable.IsZero() {
		t.Fatalf("autoResume of an unrouted harness: woke %q park %+v", *woke, ag.Park)
	}
	watchApp(w.Host).out.(*bytes.Buffer).Reset()
	if err := watchApp(w.Host).resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err != nil {
		t.Fatal(err)
	}
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if !strings.Contains(out, agentOne+": its park ended; it runs on omp, which beekeeper has no message route to, so nobody is woken") ||
		!strings.Contains(out, parkAnswer) {
		t.Errorf("resume said:\n%s", out)
	}
	if ag := agentOf(t, w); len(*woke) > 0 || ag.Park != nil {
		t.Errorf("after the resume: woke %q park %+v", *woke, ag.Park)
	}
}

// A session off the roster leaves its own park too, waking nobody.
func TestOffRosterSessionLeavesItsOwnPark(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = nil
		st.Parks = []state.Park{{By: state.Party{Name: agentOne}, At: relayNow, Waits: "a reply on my branch"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := resume(t, w); err != nil {
		t.Fatal(err)
	}
	if out := watchApp(w.Host).out.(*bytes.Buffer).String(); !strings.Contains(out, agentOne+": your park ended, nobody is woken.") {
		t.Errorf("resume said:\n%s", out)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(*woke) > 0 || len(st.Parks) > 0 {
		t.Errorf("woke %q parks %+v", *woke, st.Parks)
	}
}

// A park on a hold is refused while nothing holds the target, and settles
// once the hold is lifted: a gate's exit 77 waits there, not on a pull
// request that never settles while the hold lifts.
func TestLiftedHoldMakesTheParkResumable(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := park(t, w, "--on", "hold:merges", "the devctl release window"); err == nil || !strings.Contains(err.Error(), "nothing holds merges") {
		t.Fatalf("park on a hold nothing holds: %v", err)
	}
	watchApp(w.Host).out.(*bytes.Buffer).Reset()
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = []state.Hold{{Target: merge.AllMerges, Except: devctlRepo, By: state.Party{Name: aheadName}, At: relayNow, Reason: "the devctl release window"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := park(t, w, "--on", "hold:merges", "the devctl release window"); err != nil {
		t.Fatal(err)
	}
	if ag := agentOf(t, w); ag.Park == nil || ag.Park.On != litHoldMerges {
		t.Fatalf("park %+v", ag.Park)
	}
	watchApp(w.Host).out.(*bytes.Buffer).Reset()
	w.Pending(context.Background(), nil)
	if out := watchApp(w.Host).out.(*bytes.Buffer).String(); strings.Contains(out, `AGENT RESUMABLE "`) {
		t.Fatalf("resumable while held:\n%s", out)
	}
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.Pending(context.Background(), nil)
	w.Pending(context.Background(), nil)
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if n := strings.Count(out, `AGENT RESUMABLE "`); n != 1 || !strings.Contains(out, "nothing holds merges any more: run the refused command again") {
		t.Errorf("%d lines:\n%s", n, out)
	}
}

func TestPublishedParkHead(t *testing.T) {
	const branch = "fix/example"
	const repairBranch = "repair/pr-7"
	const repo = "o/r"
	for _, tc := range []struct {
		name, localBranch, remoteBranch, localHead, remoteHead, remoteRepo string
		readErr, draft                                                     bool
		want                                                               string
	}{
		{name: "draft published branch", draft: true, localBranch: branch, remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: repo},
		{name: "published branch", localBranch: branch, remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: repo, want: loopHead},
		{name: "repair branch same published commit", localBranch: repairBranch, remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: repo, want: loopHead},
		{name: "repair branch unpushed commit", localBranch: repairBranch, remoteBranch: branch, localHead: loopFixed, remoteHead: loopHead, remoteRepo: repo},
		{name: "repair branch fork", localBranch: repairBranch, remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: "other/r"},
		{name: "unpushed head", localBranch: branch, remoteBranch: branch, localHead: loopFixed, remoteHead: loopHead, remoteRepo: repo},
		{name: "fork same branch", localBranch: branch, remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: "other/r"},
		{name: "detached", remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: repo},
		{name: "unreachable", localBranch: branch, remoteBranch: branch, localHead: loopHead, remoteHead: loopHead, remoteRepo: repo, readErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				var out string
				switch args[0] {
				case "remote":
					out = "git@github.com:o/r.git"
				case "symbolic-ref":
					out = tc.localBranch
				case "rev-parse":
					out = tc.localHead
				default:
					t.Fatalf("unexpected git call %v", args)
				}
				return []byte(out), nil
			}
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				if github.Repo(ctx) != repo || !slices.Equal(args, []string{workerGitHubAPIGroup, "repos/o/r/pulls/7"}) {
					t.Fatalf("gh call %v", args)
				}
				if tc.readErr {
					return nil, fmt.Errorf("unreachable")
				}
				return []byte(fmt.Sprintf(`{"draft":%t,"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}}}`, tc.draft, tc.remoteBranch, tc.remoteHead, tc.remoteRepo)), nil
			}
			got, reason := publishedParkVerification(context.Background(), loopPR, ".", git, gh)
			if (got == "") != (reason != "") {
				t.Fatalf("head %q, reason %q", got, reason)
			}
			if got != tc.want {
				t.Fatalf("head %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParkSaysReviewRequestOrPlainWait(t *testing.T) {
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	for _, head := range []string{"", loopHead} {
		w := parkingWatch(t, map[string]string{refOne: github.Open})
		parkReviewHead = func(string) (string, string) { return head, parkVerificationUnavailable }
		if err := park(t, w, "--on", refOne, deskReviewWait); err != nil {
			t.Fatal(err)
		}
		if ag := agentOf(t, w); ag.Park.ReviewHead != head {
			t.Fatalf("park %+v", ag.Park)
		}
		want := parkReviewMode(head)
		if head == "" {
			want = "review request saved; published HEAD verification pending"
		}
		if !strings.Contains(watchApp(w.Host).out.(*bytes.Buffer).String(), want) {
			t.Fatalf("output %s, want %q", watchApp(w.Host).out, want)
		}
	}
}

func TestPlainPullWaitWakesOnMergeOrClose(t *testing.T) {
	for _, status := range []string{github.Merged, github.Closed} {
		t.Run(status, func(t *testing.T) {
			refs := map[string]string{refOne: github.Open}
			w := parkingWatch(t, refs)
			watchApp(w.Host).cfg.Agents.AutoResume = true
			woke := stubWakes(t)
			if err := park(t, w, "--on", refOne, "overlap with another worker's pull request"); err != nil {
				t.Fatal(err)
			}
			if agentOf(t, w).Park.ReviewHead != "" {
				t.Fatal("plain wait requested review")
			}
			w.Pending(context.Background(), nil)
			if len(*woke) != 0 {
				t.Fatalf("woke while open: %v", *woke)
			}
			refs[refOne] = status
			w.Pending(context.Background(), nil)
			w.Pending(context.Background(), nil)
			assertFreshQueue(t, w, *woke, strings.ToLower(status))
		})
	}
}

// reviewerLogin reviews the pull request a session parked on.
const reviewerLogin = "a-reviewer"

// offSession is a desktop session no roster entry names.
var offSession = state.Party{Session: "0b7c2a52-6a8e-4d3e-9a51-2f8f4f8b1c11", Name: "a hand-started session"}

// replyingRefs has GitHub answer o/r#1 open, with reply as its newest
// review or comment by anyone but its author.
func replyingRefs(t *testing.T, reply *github.Reply) {
	t.Helper()
	prev := refStates
	t.Cleanup(func() { refStates = prev })
	refStates = func(_ context.Context, rs []github.PR) (map[github.PR]github.RefState, error) {
		m := map[github.PR]github.RefState{}
		for _, r := range rs {
			m[r] = github.RefState{Kind: github.RefPullRequest, State: github.Open, Reply: *reply}
		}
		return m, nil
	}
}

// A session off the roster parks on a pull request; a review by anyone but
// its author after the park settles it, and with agents.autoResume the watch
// wakes the session by its id, once, and drops the park.
func TestAReviewResumesASessionParkedOffTheRoster(t *testing.T) {
	w := parkingWatch(t, nil)
	watchApp(w.Host).cfg.Agents.AutoResume = true
	reply := &github.Reply{By: reviewerLogin, At: relayNow.Add(-time.Hour), Kind: "review COMMENTED"}
	replyingRefs(t, reply)
	woke := stubWakes(t)
	line, err := watchApp(w.Host).park(offSession, "o/r#1", "the desk review")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "off the roster, woken by its session id") || !strings.Contains(line, "the watch resumes it with what settles o/r#1") {
		t.Errorf("line %q", line)
	}
	if st, _ := watchApp(w.Host).store.Read(); len(st.Parks) != 1 || len(st.Agents) != 1 || st.Agents[0].Park != nil {
		t.Fatalf("parks %+v agents %+v", st.Parks, st.Agents)
	}
	w.Pending(context.Background(), nil)
	if len(*woke) > 0 {
		t.Fatalf("a review older than the park resumed it: %q", *woke)
	}
	*reply = github.Reply{By: reviewerLogin, At: relayNow.Add(time.Minute), Kind: "review CHANGES_REQUESTED"}
	for range 3 {
		w.Pending(context.Background(), nil)
	}
	if len(*woke) != 0 {
		t.Fatalf("old off-roster transcript woke: %v", *woke)
	}
	st, _ := watchApp(w.Host).store.Read()
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(offSession) })
	if i < 0 || st.Agents[i].Park == nil || len(st.Agents[i].Park.Continuation) != 1 || !strings.Contains(st.Agents[i].Park.Continuation[0], "changes_requested") {
		t.Fatal("off-roster task lost its review event")
	}

	if out := watchApp(w.Host).out.(*bytes.Buffer).String(); strings.Count(out, `AGENT RESUMABLE "`+offSession.Session+`"`) != 1 {
		t.Errorf("out:\n%s", out)
	}
	if st, _ := watchApp(w.Host).store.Read(); len(st.Parks) != 0 {
		t.Errorf("park left: %+v", st.Parks)
	}
}

// A roster agent's park on a pull request settles on a review too, never on
// one older than the park.
func TestAReviewMakesAnAgentsParkResumable(t *testing.T) {
	w := parkingWatch(t, nil)
	reply := &github.Reply{By: reviewerLogin, At: relayNow.Add(time.Minute), Kind: "comment"}
	replyingRefs(t, reply)
	if err := park(t, w, "--on", "o/r#1", "the desk review"); err != nil {
		t.Fatal(err)
	}
	w.Pending(context.Background(), nil)
	if ag := agentOf(t, w); ag.Park == nil || ag.Park.Resumable.IsZero() || !strings.Contains(ag.Park.Answer, "o/r#1 has a new comment by "+reviewerLogin) {
		t.Errorf("park %+v", ag.Park)
	}
}

// A session parked off the roster is resumed by hand by its id; one whose
// session has no task context keeps its park and is refused without a resume.
func TestResumeOffTheRosterRequiresRecordedTask(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if _, err := watchApp(w.Host).park(offSession, "", "the supervisor's go"); err != nil {
		t.Fatal(err)
	}
	if err := watchApp(w.Host).resumeParked(context.Background(), state.Party{Name: resumer}, offSession.Session); err == nil || !strings.Contains(err.Error(), "no recorded task reference") {
		t.Fatalf("anonymous park resumed an old transcript: %v", err)
	}
	st, _ := watchApp(w.Host).store.Read()
	if len(*woke) != 0 || len(st.Parks) != 1 {
		t.Fatal("unknown task lost its park")
	}
}

// A session that parked off the roster and then joins it ends that park: a
// later settle wakes nobody, through registerAgent or any other way onto the
// roster.
func TestJoiningTheRosterEndsAParkOffIt(t *testing.T) {
	for _, viaRegister := range []bool{true, false} {
		w := parkingWatch(t, nil)
		watchApp(w.Host).cfg.Agents.AutoResume = true
		reply := &github.Reply{}
		replyingRefs(t, reply)
		woke := stubWakes(t)
		if _, err := watchApp(w.Host).park(offSession, "o/r#1", "the desk review"); err != nil {
			t.Fatal(err)
		}
		if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
			if viaRegister {
				_, err := registerAgent(st, offSession, func(state.Party) bool { return false }, relayNow)
				return nil, err
			}
			st.Agents = append(st.Agents, state.Agent{Party: offSession, Task: parkTask})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if st, _ := watchApp(w.Host).store.Read(); viaRegister && len(st.Parks) != 0 {
			t.Errorf("the park off the roster outlived the registration: %+v", st.Parks)
		}
		*reply = github.Reply{By: reviewerLogin, At: relayNow.Add(time.Minute), Kind: "comment"}
		for range 2 {
			w.Pending(context.Background(), nil)
		}
		if len(*woke) != 0 {
			t.Errorf("register %v: a stale park woke the agent: %q", viaRegister, *woke)
		}
		if st, _ := watchApp(w.Host).store.Read(); len(st.Parks) != 0 {
			t.Errorf("register %v: parks %+v", viaRegister, st.Parks)
		}
	}
}

// Publication evidence and its mode survive the shared park path for a
// session off the roster, as they do for registered workers.
func TestOffRosterParkRetainsReviewMode(t *testing.T) {
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	for _, head := range []string{"", loopHead} {
		w := parkingWatch(t, map[string]string{refOne: github.Open})
		parkReviewHead = func(string) (string, string) { return head, parkVerificationUnavailable }
		line, err := watchApp(w.Host).park(offSession, refOne, deskReviewWait)
		if err != nil {
			t.Fatal(err)
		}
		st, err := watchApp(w.Host).store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Parks) != 1 || st.Parks[0].ReviewHead != head || st.Parks[0].RefKind != github.RefPullRequest || strings.Contains(line, "review request recorded") {
			t.Fatalf("parks %+v, line %q", st.Parks, line)
		}
	}
}

func TestLoopAnsweredLocalNoteResumesUnderHeldGraphQL(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprint(exhausted), func(t *testing.T) {
			w := parkingWatch(t, nil)
			w.LoopOwned = true
			watchApp(w.Host).cfg.GitHub.Floor = 2500
			watchApp(w.Host).cfg.Agents.AutoResume = true
			woke := stubWakes(t)
			if err := park(t, w, "--on", "#3", "Alex's answer"); err != nil {
				t.Fatal(err)
			}
			if exhausted {
				if err := watchApp(w.Host).store.WriteFile(loopGraphQLFile, loopGraphQLSpend{Reset: watchApp(w.Host).now.Add(time.Hour), Spent: loopGraphQLLimit}); err != nil {
					t.Fatal(err)
				}
				if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
					st.Budget = &state.Budget{Remaining: 4900, At: watchApp(w.Host).now, Reset: watchApp(w.Host).now.Add(time.Hour), GraphQL: &state.GraphQL{Remaining: 4900, At: watchApp(w.Host).now, Reset: watchApp(w.Host).now.Add(time.Hour)}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			} // Otherwise GraphQL is unknown and must fail closed.
			previous := readLoopGraphQLBudget
			t.Cleanup(func() { readLoopGraphQLBudget = previous })
			readLoopGraphQLBudget = func(*app, context.Context) (github.Budget, error) {
				t.Fatal("local-note settlement probed GitHub under a held budget")
				return github.Budget{}, nil
			}
			answer(t, w)
			w.Pending(t.Context(), nil)
			if p := agentOf(t, w).Park; p == nil || p.Resumable.IsZero() || !strings.Contains(p.Answer, parkAnswer) {
				t.Fatalf("answered local note did not become resumable: %+v", p)
			}
			w.Pending(t.Context(), nil)
			assertFreshQueue(t, w, *woke, parkAnswer)
		})
	}
}

func TestSettledParksKeepsPartialBatchResultsOnError(t *testing.T) {
	w := parkingWatch(t, nil)
	if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park = &state.Park{At: watchApp(w.Host).now, On: refOne}
		st.Parks = []state.Park{{By: offSession, At: watchApp(w.Host).now, On: refTwo}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	prev := refStates
	t.Cleanup(func() { refStates = prev })
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		return map[github.PR]github.RefState{
			refs[0]: {Kind: github.RefPullRequest, State: github.Merged},
		}, errParkLookup
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	settled := w.SettledParks(t.Context(), st)
	if len(settled) != 1 || settled[0].On != refOne {
		t.Fatalf("partial batch result did not settle %s: %+v", refOne, settled)
	}
	st, err = watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[0].Park == nil || st.Agents[0].Park.RefKind != github.RefPullRequest {
		t.Fatalf("partial batch result did not cache RefKind: %+v", st.Agents[0].Park)
	}
}

func TestOverlapParkRequiresTrackedTarget(t *testing.T) {
	for _, on := range []string{"", "#3", "hold:merges", "alert:Down"} {
		w := parkingWatch(t, nil)
		_, err := watchApp(w.Host).park(state.Party{Name: agentOne}, on, "Supervisor OVERLAP coordination")
		if err == nil || !strings.Contains(err.Error(), "--on <owner/repo#n>") {
			t.Fatalf("target %q: %v", on, err)
		}
		if agentOf(t, w).Park != nil {
			t.Fatalf("refused overlap recorded a park on %q", on)
		}
	}
	for _, on := range []string{refOne, "https://github.com/o/r/pull/1"} {
		if err := validateOverlapPark(on, "overlap coordination"); err != nil {
			t.Fatalf("tracked target %q: %v", on, err)
		}
	}
	if err := validateOverlapPark("", "waiting for a manual resume"); err != nil {
		t.Fatal(err)
	}
}

// TestParkReasonMaySayAnything: --on alone says what a park waits on. A
// park on a pull request whose reason speaks of a hold, something held, a
// release window or a gate run is recorded on that pull request, by the
// command and directly, and refuses nothing.
func TestParkReasonMaySayAnything(t *testing.T) {
	for _, waits := range []string{
		"the editor's held-still source for the opening shot",
		"waiting for the merge hold to lift",
		"merges are HELD for the release",
		"the devctl release window",
		"waiting for gate-run completion",
		"the gated run's outcome",
	} {
		t.Run(waits, func(t *testing.T) {
			for _, direct := range []bool{false, true} {
				w := parkingWatch(t, map[string]string{refOne: github.Open})
				var err error
				if direct {
					_, err = watchApp(w.Host).park(state.Party{Name: agentOne}, refOne, waits)
				} else {
					err = park(t, w, "--on", refOne, waits)
				}
				if err != nil {
					t.Fatalf("direct %t: park refused: %v", direct, err)
				}
				if p := agentOf(t, w).Park; p == nil || p.On != refOne || p.Waits != waits {
					t.Fatalf("direct %t: park %+v, want on %s waiting for %q", direct, p, refOne, waits)
				}
			}
		})
	}
}

// TestMergedPRSettlesAParkWhateverItsReason: the watch settles a park on a
// pull request by that pull request, whatever its reason says, roster and
// off-roster parks alike.
func TestMergedPRSettlesAParkWhateverItsReason(t *testing.T) {
	for _, waits := range []string{"the held-still source", "the merge hold lifting", "gate run completion"} {
		t.Run(waits, func(t *testing.T) {
			w := parkingWatch(t, nil)
			old := refStates
			t.Cleanup(func() { refStates = old })
			refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
				out := map[github.PR]github.RefState{}
				for _, r := range refs {
					out[r] = github.RefState{Kind: github.RefPullRequest, State: github.Merged}
				}
				return out, nil
			}
			if err := watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
				st.Agents[0].Park = &state.Park{On: refOne, Waits: waits, At: relayNow}
				st.Parks = []state.Park{{By: state.Party{Name: "other-worker"}, On: refOne, Waits: waits, At: relayNow}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			w.Pending(context.Background(), nil)
			if out := watchApp(w.Host).out.(*bytes.Buffer).String(); !strings.Contains(out, "AGENT RESUMABLE") {
				t.Fatalf("the merge did not settle the park:\n%s", out)
			}
			st, err := watchApp(w.Host).store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if st.Agents[0].Park == nil || st.Agents[0].Park.Resumable.IsZero() {
				t.Fatalf("roster park not resumable: %+v", st.Agents[0].Park)
			}
			if len(st.Parks) != 1 || st.Parks[0].Resumable.IsZero() {
				t.Fatalf("off-roster park not resumable: %+v", st.Parks)
			}
		})
	}
}

func TestDeskReviewParkSaysVerificationPending(t *testing.T) {
	w := parkingWatch(t, map[string]string{refOne: github.Open})
	oldHead, oldWorktree := parkReviewHead, parkWorktree
	t.Cleanup(func() { parkReviewHead, parkWorktree = oldHead, oldWorktree })
	parkReviewHead = func(string) (string, string) { return "", parkVerificationUnavailable }
	parkWorktree = func(string) string { return pendingReviewWorktree }
	if err := park(t, w, "--on", refOne, "--post-merge", "prove", deskReviewWait); err != nil {
		t.Fatal(err)
	}
	p := agentOf(t, w).Park
	if p.Worktree != pendingReviewWorktree || p.ReviewHead != "" || p.PostMerge == nil || *p.PostMerge != "prove" {
		t.Fatalf("pending park: %+v", p)
	}
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if !strings.Contains(out, "verification pending") || !strings.Contains(out, "retries automatically") || strings.Contains(out, "plain wait recorded") {
		t.Fatalf("output: %s", out)
	}
}

// A settled wait is a durable event for the next fresh worker, never a turn
// of the session that parked. Repeated settlement must not grow the brief.
func assertFreshQueue(t *testing.T, w *Watcher, woke []string, contains string) {
	t.Helper()
	ag := agentOf(t, w)
	if len(woke) != 0 || ag.Park == nil || len(ag.Park.Continuation) != 1 || !strings.Contains(ag.Park.Continuation[0], contains) {
		t.Fatalf("want fresh continuation containing %q, woke=%v park=%+v", contains, woke, ag.Park)
	}
}

func TestDeskReviewParkJSONExplainsPendingVerification(t *testing.T) {
	w := parkingWatch(t, map[string]string{refOne: github.Open})
	old := parkReviewHead
	t.Cleanup(func() { parkReviewHead = old })
	parkReviewHead = func(string) (string, string) { return "", "GitHub pull request lookup failed" }
	watchApp(w.Host).json = true
	if err := park(t, w, "--on", refOne, deskReviewWait); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(watchApp(w.Host).out.(*bytes.Buffer).Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result["message"], "GitHub pull request lookup failed") || !strings.Contains(result["message"], "retries automatically") {
		t.Fatalf("JSON: %v", result)
	}
}

func TestMatchingHeadWithNonReviewReasonStaysPlain(t *testing.T) {
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	parkReviewHead = func(string) (string, string) { t.Fatal("plain wait verified review head"); return loopHead, "" }
	w := parkingWatch(t, map[string]string{refOne: github.Open})
	if err := park(t, w, "--on", refOne, "image rebuild completion timer 3155"); err != nil {
		t.Fatal(err)
	}
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if agentOf(t, w).Park.ReviewHead != "" || len(st.ReviewParks) != 0 {
		t.Fatal("plain park requested review")
	}
	if !strings.Contains(watchApp(w.Host).out.(*bytes.Buffer).String(), "plain wait recorded") {
		t.Fatalf("output %s", watchApp(w.Host).out)
	}
}

func TestDraftReviewParkExplainsPlainWait(t *testing.T) {
	was := parkReviewHead
	t.Cleanup(func() { parkReviewHead = was })
	parkReviewHead = func(string) (string, string) { return "", parkDraftReason }
	w := parkingWatch(t, map[string]string{refOne: github.Open})
	if err := park(t, w, "--on", refOne, deskReviewWait); err != nil {
		t.Fatal(err)
	}
	out := watchApp(w.Host).out.(*bytes.Buffer).String()
	if !strings.Contains(out, "plain wait recorded") || !strings.Contains(out, parkDraftReason) || strings.Contains(out, "verification pending") {
		t.Fatalf("output %s", out)
	}
}
