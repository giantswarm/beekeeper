package cmd

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	parkTask   = "beekeeper#86: agents park"
	parkAnswer = "yes, roll it after 18:00, not before"
)

// parkingWatch is a running watch over a roster of agentOne with a task and
// note #3 open for Timo, the calling session agentOne; GitHub answers refs.
func parkingWatch(t *testing.T, refs map[string]string) *watcher {
	t.Helper()
	w, _ := overtakingWatch(t, refs)
	if err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Name: agentOne}, Task: parkTask, AssignedAt: relayNow}}
		st.Notes = []state.Note{{ID: 3, For: personTimo, Text: askedQ, By: state.Party{Name: agentOne}}}
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
		return []state.Event{answeredEvent(state.Party{Name: personTimo, Person: personTimo}, &n, "terminal", parkAnswer)}, nil
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
	ag := agentOf(t, w)
	if ag.Task != parkTask || ag.Park == nil || ag.Park.On != "#3" {
		t.Fatalf("agent %+v park %+v", ag, ag.Park)
	}
	st, _ := w.store.Read()
	c := countAgents(st, nil, relayNow)
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
	if err := w.resumeParked(context.Background(), state.Party{Name: "supervisor"}, agentOne); err != nil {
		t.Fatal(err)
	}
	if len(*woke) != 1 || !strings.Contains((*woke)[0], "Your park on o/r#1 (the merge ahead of mine) is over: o/r#1 merged.") {
		t.Errorf("woke %q", *woke)
	}
	if err := w.resumeParked(context.Background(), state.Party{Name: "supervisor"}, agentOne); err == nil || !strings.Contains(err.Error(), "is not parked") {
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
	if err := w.resumeParked(context.Background(), state.Party{Name: "supervisor"}, agentOne); err != nil {
		t.Fatal(err)
	}
	want := "supervisor → " + agentOne + ": Your park on the supervisor's go is over: supervisor resumed you by hand. Re-query the live state of your task and continue it."
	if !slices.Equal(*woke, []string{want}) {
		t.Errorf("woke %q", *woke)
	}
}
