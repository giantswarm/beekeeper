package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/transcripts"
)

const (
	endedName = "ended worker"
	endedTask = "fix https://github.com/acme/app/issues/7"
	endedConv = "conv-ended"
)

// endWorker records a started worker of party p and takes it off the roster.
func endWorker(t *testing.T, a *app, p state.Party, harness string) {
	t.Helper()
	seed(t, a, func(st *state.State) {
		st.Starts = append(st.Starts, state.Start{Party: p, Harness: harness, Dir: "/work/app", At: a.now.Add(-48 * time.Hour), Conversation: map[bool]string{true: endedConv}[harness == agy.Harness]})
		st.Agents = []state.Agent{{Party: p, Registered: a.now.Add(-48 * time.Hour), LastTask: endedTask, Harness: harness, Done: true}}
		removeAgent(st, 0)
	})
}

func TestEndedClaudeWorkerWakesByNameInItsOwnConversation(t *testing.T) {
	for _, action := range []string{agentWakeName, agentResumeName} {
		t.Run(action, func(t *testing.T) {
			const id = "ended-claude"
			a, launcher, _ := wakeTwinSetup(t, id, false)
			path := filepath.Join(a.cfg.Claude.ProjectsDir, "ended-proof", id+".jsonl")
			dir := t.TempDir()
			writeFile(t, path, `{"type":"user","cwd":"`+filepath.ToSlash(dir)+`"}`+"\n")
			seed(t, a, func(st *state.State) { st.Agents = nil })
			endWorker(t, a, state.Party{Session: id, Name: endedName}, "")

			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			s, ok := a.endedWorker(st, strings.ToUpper(endedName))
			if !ok || s.Task != endedTask {
				t.Fatalf("endedWorker = %+v, %v", s, ok)
			}
			msg := "continue"
			if action == agentResumeName {
				msg = endedResumeMessage
			}
			if err := a.wakeEnded(t.Context(), waker, s, msg, ""); err != nil {
				t.Fatal(err)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(launcher.units) != 1 || len(st.Agents) != 1 || st.Agents[0].Name != endedName || st.Agents[0].Task != endedTask {
				t.Fatalf("units=%d agents=%+v", len(launcher.units), st.Agents)
			}
			if _, ok := a.endedWorker(st, endedName); ok {
				t.Fatal("a worker back on the roster still counts as ended")
			}
		})
	}
}

func TestEndedWorkerStaysOutOfOtherWakes(t *testing.T) {
	a, _ := stubApp(t)
	endWorker(t, a, state.Party{Session: "ended-timer", Name: endedName}, "")
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.wakeParty(st, endedName); err == nil {
		t.Fatal("a wake by a timer or a settled wait would bring a finished worker back")
	}
	if _, ok := a.endedWorker(st, "ended"); ok {
		t.Fatal("a part of a name brought a worker back")
	}
}

func TestEndedHarnessWorkerRefusalNamesItsTranscript(t *testing.T) {
	a, _ := stubApp(t)
	p := state.Party{HostSession: agy.HostPrefix + "ended-run", Name: endedName}
	endWorker(t, a, p, agy.Harness)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	key := transcripts.SafeKey(agy.Harness, executionLocal, endedConv)
	if _, err := a.transcriptStore().Save(transcripts.Meta{Key: key, ID: endedConv, Harness: agy.Harness, Host: executionLocal}, []transcripts.Turn{{Role: transcripts.RoleUser, Text: endedTask}}); err != nil {
		t.Fatal(err)
	}
	s, ok := a.endedWorker(st, endedName)
	if !ok || s.Conversation != endedConv {
		t.Fatalf("endedWorker = %+v, %v", s, ok)
	}
	err = a.wakeEnded(t.Context(), waker, s, "continue", "")
	if !errors.Is(err, errGone) || !strings.Contains(err.Error(), "cannot continue") || !strings.Contains(err.Error(), key) {
		t.Fatalf("refusal = %v", err)
	}
	if st, err = a.store.Read(); err != nil || len(st.Agents) != 0 {
		t.Fatalf("the roster keeps a worker that did not come back: %+v, %v", st.Agents, err)
	}
}

func TestEndedWorkerRespectsAStartsHold(t *testing.T) {
	a, _ := stubApp(t)
	endWorker(t, a, state.Party{Session: "ended-held", Name: endedName}, "")
	seed(t, a, func(st *state.State) {
		st.Holds = append(st.Holds, state.Hold{Target: holdStarts, By: waker, At: a.now})
	})
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.endedWorker(st, endedName)
	previous := wakeOwner
	t.Cleanup(func() { wakeOwner = previous })
	wakeOwner = func(*app, context.Context, state.Party, string, string, string) error {
		t.Fatal("a hold on starts did not stop the worker's return")
		return nil
	}
	if err := a.wakeEnded(t.Context(), waker, s, "continue", ""); err == nil {
		t.Fatal("a starts hold let an ended worker back")
	}
}

func TestEndedAgyTranscriptKeepsItsRosterName(t *testing.T) {
	a, _ := stubApp(t)
	endWorker(t, a, state.Party{HostSession: agy.HostPrefix + "ended-run", Name: endedName}, agy.Harness)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	// The ended session as the agy brain shows it: its conversation id alone.
	meta := transcripts.Meta{ID: endedConv, Harness: agy.Harness}
	a.enrichMetaFromState(st, &meta, state.Party{Session: endedConv}, nil)
	if meta.RosterName != endedName || meta.HostSession != agy.HostPrefix+"ended-run" || meta.Role != workerRole {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Ref != "acme/app#7" {
		t.Fatalf("the task's reference is lost: %+v", meta)
	}
	if err := transcriptNamed(meta, endedName); err != nil {
		t.Fatal(err)
	}
}

// transcriptNamed fails unless a read and a session grant by name reach meta.
func transcriptNamed(meta transcripts.Meta, name string) error {
	if m, err := transcripts.Resolve([]transcripts.Meta{meta}, name); err != nil || m.ID != meta.ID {
		return errors.Join(errors.New("read by name"), err)
	}
	if !transcripts.Covers(state.TranscriptGrant{Session: name}, meta) {
		return errors.New("a grant by name does not cover it")
	}
	return nil
}
