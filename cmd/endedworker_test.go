package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
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

func TestEndedAgyWorkerWakesByNameInItsOwnConversation(t *testing.T) {
	for _, action := range []string{agentWakeName, agentResumeName} {
		t.Run(action, func(t *testing.T) {
			const (
				id    = "11223344-5566-7788-99aa-bbccddeeff00"
				model = "claude-opus-4-6-thinking"
			)
			writeFakeAgy(t, "Available models:\n  "+model+"\n")
			dir := t.TempDir()
			workDir := t.TempDir()
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a := &app{
				cfg:   &config.Config{StateDir: dir},
				store: store,
				now:   time.Now(),
				out:   &out,
			}
			var gotMsg string
			origLauncher, origMachine := plat.Launcher, plat.Machine
			t.Cleanup(func() { plat.Launcher, plat.Machine = origLauncher, origMachine })
			plat.Machine = tableMachine{plat.Machine}
			l := &agyTestLauncher{
				onStart: func(u platform.Unit) {
					go func() {
						inbox := agy.InboxPath(dir, id)
						logPath := agy.StreamPath(dir, id)
						f, oerr := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test FIFO
						if oerr != nil {
							return
						}
						defer func() { _ = f.Close() }()
						_, _ = bufio.NewReader(f).ReadString('\n')
						if msgs, derr := agy.Drain(inbox); derr == nil && len(msgs) > 0 {
							gotMsg = msgs[0]
						}
						stream := `{"event":"init","conversation_id":"` + endedConv + `"}` + "\n" +
							`{"event":"step_update","step_update":{"conversation_id":"` + endedConv + `","step_type":"user_input"}}` + "\n"
						_ = os.WriteFile(logPath, []byte(stream), 0o600)
					}()
				},
			}
			plat.Launcher = l

			p := state.Party{HostSession: agy.HostPrefix + id, Name: endedName}
			seed(t, a, func(st *state.State) {
				st.Starts = []state.Start{{Party: p, Harness: agy.Harness, Model: model, Dir: workDir, At: a.now.Add(-time.Hour), Conversation: endedConv}}
				st.Agents = []state.Agent{{Party: p, Registered: a.now.Add(-time.Hour), LastTask: endedTask, Harness: agy.Harness, Model: model, Done: true}}
				removeAgent(st, 0)
			})
			_ = a.endAgy(t.Context(), id)

			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			s, ok := a.endedWorker(st, endedName)
			if !ok || s.Conversation != endedConv || s.Dir != workDir {
				t.Fatalf("endedWorker = %+v, %v", s, ok)
			}
			msg := "continue agy"
			if action == agentResumeName {
				msg = endedResumeMessage
			}
			if err := a.wakeEnded(t.Context(), waker, s, msg, ""); err != nil {
				t.Fatalf("wakeEnded(agy): %v", err)
			}
			if len(l.started) != 1 || !slices.Contains(l.started[0].Argv, "--conversation") || !slices.Contains(l.started[0].Argv, endedConv) {
				t.Fatalf("started argv = %v, want --conversation %s", l.started, endedConv)
			}
			if !strings.Contains(gotMsg, endedTask) || !strings.Contains(gotMsg, msg) {
				t.Fatalf("delivered message = %q, want task %q and %q", gotMsg, endedTask, msg)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Agents) != 1 || st.Agents[0].Name != endedName || st.Agents[0].Task != endedTask {
				t.Fatalf("roster = %+v", st.Agents)
			}
		})
	}
}

func TestEndedCodexWorkerWakesByNameInItsOwnConversation(t *testing.T) {
	for _, action := range []string{agentWakeName, agentResumeName} {
		t.Run(action, func(t *testing.T) {
			const (
				id       = "22334455-6677-8899-aabb-ccddeeff0011"
				model    = "gpt-5.7"
				threadID = "thread-ended-42"
			)
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			workDir := filepath.Join(root, "work")
			userCodex := filepath.Join(root, "user-codex")
			fakeBin := filepath.Join(root, "bin")
			for _, d := range []string{workDir, userCodex, fakeBin} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			authFile := filepath.Join(userCodex, "auth.json")
			if err := os.WriteFile(authFile, []byte(`{"access_token":"test"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			userConfig := filepath.Join(userCodex, "config.toml")
			if err := os.WriteFile(userConfig, []byte("[features]\nplugins = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fakeBin, codex.Comm), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test executable
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

			store, err := state.Open(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a := &app{
				cfg: &config.Config{
					StateDir: stateDir,
					Codex:    config.Codex{Model: model, AuthFile: authFile, ConfigFile: userConfig},
				},
				store: store,
				now:   time.Now(),
				out:   &out,
			}

			var gotMsg string
			origLauncher, origSend := plat.Launcher, sendCodexWhenReady
			t.Cleanup(func() {
				plat.Launcher = origLauncher
				sendCodexWhenReady = origSend
			})
			l := &agyTestLauncher{
				onStart: func(u platform.Unit) {
					logPath := codex.LogPath(stateDir, id)
					stream := `{"type":"turn.started"}` + "\n"
					_ = os.WriteFile(logPath, []byte(stream), 0o600)
				},
			}
			plat.Launcher = l
			sendCodexWhenReady = func(_ context.Context, _ string, m string, _ time.Duration) error {
				gotMsg = m
				return nil
			}

			p := state.Party{HostSession: codex.HostPrefix + id, Name: endedName}
			seed(t, a, func(st *state.State) {
				st.Starts = []state.Start{{Party: p, Harness: codex.Harness, Model: model, Dir: workDir, At: a.now.Add(-time.Hour), Conversation: threadID}}
				st.Agents = []state.Agent{{Party: p, Registered: a.now.Add(-time.Hour), LastTask: endedTask, Harness: codex.Harness, Model: model, Done: true}}
				removeAgent(st, 0)
			})
			_ = a.endCodex(t.Context(), id)

			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			s, ok := a.endedWorker(st, endedName)
			if !ok || s.Conversation != threadID || s.Dir != workDir {
				t.Fatalf("endedWorker = %+v, %v", s, ok)
			}
			msg := "continue codex"
			if action == agentResumeName {
				msg = endedResumeMessage
			}
			if err := a.wakeEnded(t.Context(), waker, s, msg, ""); err != nil {
				t.Fatalf("wakeEnded(codex): %v", err)
			}
			if len(l.started) != 1 || !slices.Contains(l.started[0].Argv, codexRunCmdName) {
				t.Fatalf("started argv = %v, want %s", l.started, codexRunCmdName)
			}
			if gotThread := codex.ReadThread(stateDir, codex.InboxPath(stateDir, id)); gotThread != threadID {
				t.Fatalf("recorded thread = %q, want %q", gotThread, threadID)
			}
			if !strings.Contains(gotMsg, endedTask) || !strings.Contains(gotMsg, msg) {
				t.Fatalf("delivered message = %q, want task %q and %q", gotMsg, endedTask, msg)
			}
			st, err = a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Agents) != 1 || st.Agents[0].Name != endedName || st.Agents[0].Task != endedTask {
				t.Fatalf("roster = %+v", st.Agents)
			}
		})
	}
}

func TestEndedOmpWorkerRefusalNamesItsTranscript(t *testing.T) {
	a, _ := stubApp(t)
	p := state.Party{HostSession: omp.HostPrefix + "ended-omp", Name: endedName}
	endWorker(t, a, p, omp.Harness)
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	key := transcripts.SafeKey(omp.Harness, executionLocal, "omp-sess-1")
	if _, err := a.transcriptStore().Save(transcripts.Meta{
		Key:        key,
		ID:         "omp-sess-1",
		Harness:    omp.Harness,
		Host:       executionLocal,
		RosterName: endedName,
	}, []transcripts.Turn{{Role: transcripts.RoleUser, Text: endedTask}}); err != nil {
		t.Fatal(err)
	}
	s, ok := a.endedWorker(st, endedName)
	if !ok {
		t.Fatalf("endedWorker = %+v, %v", s, ok)
	}
	err = a.wakeEnded(t.Context(), waker, s, "continue", "")
	if !errors.Is(err, errGone) || !strings.Contains(err.Error(), "cannot continue") || !strings.Contains(err.Error(), key) {
		t.Fatalf("refusal = %v", err)
	}
	if st, err = a.store.Read(); err != nil || len(st.Agents) != 0 {
		t.Fatalf("the roster keeps an omp worker that did not come back: %+v, %v", st.Agents, err)
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

func TestOmpTranscriptDirectoryMatchUsesLatestStart(t *testing.T) {
	a, _ := stubApp(t)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	older := state.Start{Party: state.Party{Name: "older", HostSession: omp.HostPrefix + "older"}, Harness: omp.Harness, Dir: "/work", At: at}
	newer := state.Start{Party: state.Party{Name: "newer", HostSession: omp.HostPrefix + "newer"}, Harness: omp.Harness, Dir: "/work", At: at.Add(time.Hour)}
	st := &state.State{Starts: []state.Start{older, newer}}
	for _, started := range []time.Time{newer.At.Add(time.Second), {}} {
		meta := transcripts.Meta{ID: "omp-session", Harness: omp.Harness, Cwd: "/work", StartedAt: started}
		a.enrichMetaFromState(st, &meta, state.Party{Session: meta.ID}, nil)
		if started.IsZero() {
			if meta.HostSession != "" || meta.RosterName != "" {
				t.Fatalf("timestamp-free transcript acquired an identity: %+v", meta)
			}
		} else if meta.HostSession != newer.HostSession || meta.RosterName != newer.Name {
			t.Fatalf("later transcript acquired the wrong worker identity: %+v", meta)
		}
	}
}
