package cmd

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	parkTask   = "beekeeper#86: agents park"
	resumer    = "the supervisor"
	parkAnswer = "yes, roll it after 18:00, not before"
)

// parkingWatch is a running watch over a roster of agentOne with a task and
// note #3 open for Timo, the calling session agentOne; GitHub answers refs.
func parkingWatch(t *testing.T, refs map[string]string) *watcher {
	t.Helper()
	w, _ := overtakingWatch(t, refs)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: parkTask, AssignedAt: relayNow}}
		st.Notes = []state.Note{{ID: 3, Kind: noteDecision, For: personTimo, Text: askedQ, By: state.Party{Name: agentOne}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func park(t *testing.T, w *watcher, args ...string) error {
	t.Helper()
	c := w.agentParkCmd()
	c.SetArgs(args)
	c.SetOut(w.out)
	c.SetErr(w.out)
	return c.Execute()
}

func agentOf(t *testing.T, w *watcher) state.Agent {
	t.Helper()
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	return st.Agents[0]
}

// answer closes note #3 with Timo's answer, as note answer does.
func answer(t *testing.T, w *watcher) {
	t.Helper()
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
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
	if err := park(t, w, "--on", "3", "Timo's answer on the rollout"); err != nil {
		t.Fatal(err)
	}
	out := w.out.(*bytes.Buffer).String()
	if want := fmt.Sprintf("beekeeper agents resume %q", agentOne); !strings.Contains(out, want) {
		t.Errorf("park output lacks the copy-pasteable %s: %q", want, out)
	}
	ag := agentOf(t, w)
	if ag.Task != parkTask || ag.Park == nil || ag.Park.On != "#3" {
		t.Fatalf("agent %+v park %+v", ag, ag.Park)
	}
	st, _ := w.store.Read()
	c := countAgents(st, nil, relayNow, nil)
	if len(c.Busy) != 0 || len(c.Parked) != 1 || !strings.Contains(c.Parked[0], "parked on #3: Timo's answer on the rollout") {
		t.Errorf("count %+v", c)
	}

	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
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
// word and ends the park.
func TestAnsweredNoteResumesTheParkedAgentOnce(t *testing.T) {
	w := parkingWatch(t, nil)
	w.cfg.Agents.AutoResume = true
	woke := stubWakes(t)
	if err := park(t, w, "--on", "#3", "Timo's answer"); err != nil {
		t.Fatal(err)
	}
	w.pending(context.Background(), nil)
	if strings.Contains(w.out.(*bytes.Buffer).String(), "AGENT RESUMABLE") || len(*woke) > 0 {
		t.Fatalf("resumable with its note open: %s %q", w.out, *woke)
	}
	answer(t, w)
	for range 3 {
		w.pending(context.Background(), nil)
	}
	out := w.out.(*bytes.Buffer).String()
	if n := strings.Count(out, `AGENT RESUMABLE "`); n != 1 {
		t.Errorf("%d AGENT RESUMABLE lines:\n%s", n, out)
	}
	if !strings.Contains(out, `AGENT RESUMABLE "Agent one": parked on #3: Timo's answer; resumable since`) {
		t.Errorf("line:\n%s", out)
	}
	want := watchParty.Name + " → " + agentOne + ": Your park on #3 (Timo's answer) is over: note #3 answered by Timo: " + parkAnswer +
		". Re-query the live state of your task and continue it."
	if !slices.Equal(*woke, []string{want}) {
		t.Errorf("woke %q\nwant %q", *woke, want)
	}
	if ag := agentOf(t, w); ag.Park != nil || ag.Task != parkTask {
		t.Errorf("after the resume: task %q park %+v", ag.Task, ag.Park)
	}
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
	w.pending(context.Background(), nil)
	refs[refOne] = github.Merged
	w.pending(context.Background(), nil)
	w.pending(context.Background(), nil)
	out := w.out.(*bytes.Buffer).String()
	if n := strings.Count(out, `AGENT RESUMABLE "`); n != 1 || !strings.Contains(out, `o/r#1 merged; beekeeper agents resume "Agent one"`) {
		t.Errorf("%d lines:\n%s", n, out)
	}
	if len(*woke) > 0 {
		t.Fatalf("resumed without agents.autoResume: %q", *woke)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err != nil {
		t.Fatal(err)
	}
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "Your park on o/r#1 (the merge ahead of mine) is over: o/r#1 merged.") {
		t.Errorf("woke %q", *woke)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err == nil || !strings.Contains(err.Error(), "is not parked") {
		t.Errorf("second resume: %v", err)
	}
}

// A park without --on is resumed by hand, its turn naming who resumed it.
func TestResumeByHand(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if err := park(t, w, "the supervisor's go"); err != nil {
		t.Fatal(err)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: resumer}, agentOne); err != nil {
		t.Fatal(err)
	}
	want := resumer + " → " + agentOne + ": Your park on the supervisor's go is over: " + resumer + " resumed you by hand. Re-query the live state of your task and continue it."
	if !slices.Equal(*woke, []string{want}) {
		t.Errorf("woke %q", *woke)
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
	w.out.(*bytes.Buffer).Reset()
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = []state.Hold{{Target: merge.AllMerges, Except: devctlRepo, By: state.Party{Name: aheadName}, At: relayNow, Reason: "the devctl release window"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := park(t, w, "--on", "hold:merges", "the devctl release window"); err != nil {
		t.Fatal(err)
	}
	if ag := agentOf(t, w); ag.Park == nil || ag.Park.On != "hold merges" {
		t.Fatalf("park %+v", ag.Park)
	}
	w.out.(*bytes.Buffer).Reset()
	w.pending(context.Background(), nil)
	if out := w.out.(*bytes.Buffer).String(); strings.Contains(out, `AGENT RESUMABLE "`) {
		t.Fatalf("resumable while held:\n%s", out)
	}
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Holds = nil
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.pending(context.Background(), nil)
	w.pending(context.Background(), nil)
	out := w.out.(*bytes.Buffer).String()
	if n := strings.Count(out, `AGENT RESUMABLE "`); n != 1 || !strings.Contains(out, "nothing holds merges any more: run the refused command again") {
		t.Errorf("%d lines:\n%s", n, out)
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
			m[r] = github.RefState{State: github.Open, Reply: *reply}
		}
		return m, nil
	}
}

// A session off the roster parks on a pull request; a review by anyone but
// its author after the park settles it, and with agents.autoResume the watch
// wakes the session by its id, once, and drops the park.
func TestAReviewResumesASessionParkedOffTheRoster(t *testing.T) {
	w := parkingWatch(t, nil)
	w.cfg.Agents.AutoResume = true
	reply := &github.Reply{By: reviewerLogin, At: relayNow.Add(-time.Hour), Kind: "review COMMENTED"}
	replyingRefs(t, reply)
	woke := stubWakes(t)
	line, err := w.park(offSession, "o/r#1", "the desk review")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "off the roster, woken by its session id") || !strings.Contains(line, "the watch resumes it with what settles o/r#1") {
		t.Errorf("line %q", line)
	}
	if st, _ := w.store.Read(); len(st.Parks) != 1 || len(st.Agents) != 1 || st.Agents[0].Park != nil {
		t.Fatalf("parks %+v agents %+v", st.Parks, st.Agents)
	}
	w.pending(context.Background(), nil)
	if len(*woke) > 0 {
		t.Fatalf("a review older than the park resumed it: %q", *woke)
	}
	*reply = github.Reply{By: reviewerLogin, At: relayNow.Add(time.Minute), Kind: "review CHANGES_REQUESTED"}
	for range 3 {
		w.pending(context.Background(), nil)
	}
	want := watchParty.Name + " → " + offSession.Session + ": Your park on o/r#1 (the desk review) is over: o/r#1 has a new review changes_requested by " + reviewerLogin + " at " +
		relayNow.Add(time.Minute).UTC().Format(time.RFC3339) + ": read and answer its reviews and comments since the park. Re-query the live state of your task and continue it."
	if !slices.Equal(*woke, []string{want}) {
		t.Errorf("woke %q\nwant %q", *woke, want)
	}
	if out := w.out.(*bytes.Buffer).String(); strings.Count(out, `AGENT RESUMABLE "`+offSession.Session+`"`) != 1 {
		t.Errorf("out:\n%s", out)
	}
	if st, _ := w.store.Read(); len(st.Parks) != 0 {
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
	w.pending(context.Background(), nil)
	if ag := agentOf(t, w); ag.Park == nil || ag.Park.Resumable.IsZero() || !strings.Contains(ag.Park.Answer, "o/r#1 has a new comment by "+reviewerLogin) {
		t.Errorf("park %+v", ag.Park)
	}
}

// A session parked off the roster is resumed by hand by its id; one whose
// session is gone by then gets no wake and one line, and its park is gone.
func TestResumeOffTheRoster(t *testing.T) {
	w := parkingWatch(t, nil)
	woke := stubWakes(t)
	if _, err := w.park(offSession, "", "the supervisor's go"); err != nil {
		t.Fatal(err)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: resumer}, offSession.Session); err != nil {
		t.Fatal(err)
	}
	if len(*woke) != 1 || !strings.HasPrefix((*woke)[0], resumer+" → "+offSession.Session+": Your park on the supervisor's go is over") {
		t.Errorf("woke %q", *woke)
	}

	if _, err := w.park(offSession, "", "the supervisor's go"); err != nil {
		t.Fatal(err)
	}
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error {
		return goneRefusal("%s: session %s has no transcript under /p to resume", offSession.Session, offSession.Session)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: resumer}, offSession.Name); err != nil {
		t.Fatal(err)
	}
	evs, _ := w.store.Events(0, func(e state.Event) bool { return e.Verb == resumedEvent && strings.Contains(e.Detail, "is not woken") })
	st, _ := w.store.Read()
	if len(evs) != 1 || len(st.Parks) != 0 {
		t.Errorf("events %+v parks %+v", evs, st.Parks)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: resumer}, offSession.Session); err == nil {
		t.Error("resumed a session no longer parked")
	}
}

// A session that parked off the roster and then joins it ends that park: a
// later settle wakes nobody, through registerAgent or any other way onto the
// roster.
func TestJoiningTheRosterEndsAParkOffIt(t *testing.T) {
	for _, viaRegister := range []bool{true, false} {
		w := parkingWatch(t, nil)
		w.cfg.Agents.AutoResume = true
		reply := &github.Reply{}
		replyingRefs(t, reply)
		woke := stubWakes(t)
		if _, err := w.park(offSession, "o/r#1", "the desk review"); err != nil {
			t.Fatal(err)
		}
		if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
			if viaRegister {
				_, err := registerAgent(st, offSession, func(state.Party) bool { return false }, relayNow)
				return nil, err
			}
			st.Agents = append(st.Agents, state.Agent{Party: offSession, Task: parkTask})
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
		if st, _ := w.store.Read(); viaRegister && len(st.Parks) != 0 {
			t.Errorf("the park off the roster outlived the registration: %+v", st.Parks)
		}
		*reply = github.Reply{By: reviewerLogin, At: relayNow.Add(time.Minute), Kind: "comment"}
		for range 2 {
			w.pending(context.Background(), nil)
		}
		if len(*woke) != 0 {
			t.Errorf("register %v: a stale park woke the agent: %q", viaRegister, *woke)
		}
		if st, _ := w.store.Read(); len(st.Parks) != 0 {
			t.Errorf("register %v: parks %+v", viaRegister, st.Parks)
		}
	}
}
