package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestWatchSettlesALostMerge(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	stubGitHub(t, "", "") // GitHub does not answer: nothing can judge it
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			// Its gate died with the machine.
			{Repo: "o/lost", PR: 1, Lane: "scratch", By: four, PID: 0, Phase: state.Running, Started: relayNow.Add(-2 * time.Hour)},
			{Repo: "o/live", PR: 2, Lane: "other", By: four, PID: os.Getpid(), Phase: state.Running, Started: relayNow},
			{Repo: "o/next", PR: 3, Lane: "scratch", By: four, Phase: state.Waiting, Joined: relayNow, Seen: relayNow},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w.lostMerges(context.Background())
	w.lostMerges(context.Background())
	if n := strings.Count(out.String(), "MERGE LOST: o/lost#1 in lane scratch"); n != 1 {
		t.Fatalf("MERGE LOST said %d times:\n%s", n, out)
	}
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Merges {
		want := map[string]string{"o/lost": state.Settling, "o/live": state.Running, "o/next": state.Waiting}[m.Repo]
		if m.Phase != want {
			t.Errorf("%s is %s, want %s", m.Key(), m.Phase, want)
		}
	}
	evs, err := w.store.Events(10, func(e state.Event) bool { return e.Verb == "merge.lost" })
	if err != nil || len(evs) != 1 {
		t.Errorf("merge.lost events: %d, %v", len(evs), err)
	}
}

func TestWatchRecordsAMergeWhoseGateIsGone(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	w.now = relayNow
	stubGitHub(t, github.Merged, "")
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Merges = []state.Merge{
			// Killed with its caller; devctl finished and left its document.
			{Repo: "o/doc", PR: 1, Lane: "doc", By: four, PID: 0, Child: 0, Phase: state.Running, Started: relayNow},
			// Killed with its caller; its runner too, GitHub reports it merged.
			{Repo: "o/gh", PR: 2, Lane: "gh", By: four, PID: 0, Phase: state.Running, Started: relayNow},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	base := mergeBase(w.store.Dir(), "o/doc", 1)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(base+".json", []byte(`{"mergeCommitSha":"abc","release":{"verdict":"available","tag":"v1.0.1"}}`), 0o600)
	_ = os.WriteFile(base+".rc", []byte("0"), 0o600)
	w.lostMerges(context.Background())
	l := out.String()
	if !strings.Contains(l, `MERGE RECORDED: o/doc#1 exit 0, release v1.0.1 (for "Agent four", whose gate, pid 0, is gone)`) ||
		!strings.Contains(l, "MERGE RECORDED: o/gh#2 exit 137, release unconfirmed (merged per GitHub)") || strings.Contains(l, "MERGE LOST") {
		t.Fatalf("watch lines:\n%s", l)
	}
	// The merge GitHub judged, its release unconfirmed, settles its lane.
	if st, _ := w.store.Read(); len(st.Merges) != 1 || st.Merges[0].Repo != "o/gh" || st.Merges[0].Phase != state.Settling {
		t.Errorf("merges left: %+v", st.Merges)
	}
	if _, err := os.Stat(base + ".json"); err == nil {
		t.Error("the merge's files are left")
	}
}

func TestStoppedAgentsAreListedOnce(t *testing.T) {
	bg := state.Party{Session: "bg-1", Name: "worker one"}
	agents := []state.Agent{
		{Party: bg, Task: "fix beekeeper#1"},
		{Party: four, Task: "fix beekeeper#2"},
		{Party: state.Party{Session: "s5", Name: "idle one"}},
	}
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	st := &state.State{Agents: agents}
	w.stoppedAgents(st, nil)
	w.stoppedAgents(st, nil)
	l := out.String()
	if strings.Count(l, "AGENTS STOPPED") != 1 || !strings.Contains(l, `"worker one" (claude --bg --resume bg-1 "`) ||
		!strings.Contains(l, `"Agent four" (open claude://code/continue?session=local_4)`) || strings.Contains(l, "idle one") {
		t.Fatalf("stopped agents:\n%s", l)
	}
	// Resumed, then stopped again: said again.
	w.stoppedAgents(st, []*claude.Session{{ID: "bg-1", Name: "worker one"}})
	out.Reset()
	w.stoppedAgents(st, nil)
	if l := out.String(); !strings.Contains(l, "worker one") || strings.Contains(l, "Agent four") {
		t.Errorf("a stop after the resume:\n%s", l)
	}

	var prompt bytes.Buffer
	a := &app{out: &prompt, now: relayNow}
	a.promptAgents(func(f string, args ...any) { prompt.WriteString(strings.TrimSpace(fmt.Sprintf(f, args...)) + "\n") }, agents, nil)
	if p := prompt.String(); !strings.Contains(p, `"worker one" on fix beekeeper#1 since`) ||
		!strings.Contains(p, "its CLI is not running (stopped by a reboot or a crash): resume it with `claude --bg --resume bg-1") ||
		!strings.Contains(p, `"idle one" idle since`) {
		t.Errorf("handover prompt:\n%s", p)
	}
}
