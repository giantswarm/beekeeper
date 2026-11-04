package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const panicCWDKey = "cwd"

type guardPanicMachine struct{ platform.Machine }

func (guardPanicMachine) Processes() (*proc.Table, error) {
	return &proc.Table{ByPID: map[int]*proc.Process{}}, nil
}

func TestHookWorktreeMoveColdState(t *testing.T) {
	oldPlatform, oldMachine := plat, testMachine
	testMachine = guardPanicMachine{Machine: plat.Machine}
	t.Cleanup(func() { plat, testMachine = oldPlatform, oldMachine })
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "finished", true: holderLive}[live], func(t *testing.T) {
			var out bytes.Buffer
			a, dir := scopeApp(t, &out)
			source := filepath.Join(dir, "desk", "source")
			writeT(t, filepath.Join(source, ".git"), "gitdir: /unused/worktrees/source\n")
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if live {
				if err := store.Update(func(s *state.State) ([]state.Event, error) {
					party := state.Party{Session: "test-worker"}
					s.Starts = append(s.Starts, state.Start{Party: party, Dir: source})
					s.Agents = append(s.Agents, state.Agent{Party: party, Task: "active task"})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(map[string]any{transcriptHookEventKey: guard.PreToolUseEvent, panicCWDKey: source, transcriptToolName: tToolBash, transcriptInputKey: map[string]any{hookTypeCommand: "git worktree move " + source + " " + filepath.Join(dir, "aside")}})
			result := runHook(t, a, &out, "pretooluse", string(raw))
			if strings.Contains(result, "panic") {
				t.Fatalf("panic: %s", result)
			}
			if strings.Contains(result, "deny") != live {
				t.Fatalf("live=%v: %s", live, result)
			}
		})
	}
}

func TestRecordGuardPanicLog(t *testing.T) {
	oldPlatform := plat
	t.Cleanup(func() { plat = oldPlatform })
	var out bytes.Buffer
	a, dir := scopeApp(t, &out)
	if err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	a.recordGuardPanic("liveWorktreeRefusal", []byte("goroutine stack fixture"))
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(0, func(e state.Event) bool { return e.Verb == "hook.panic" })
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	raw, _ := json.Marshal(events[0])
	if !strings.Contains(string(raw), "liveWorktreeRefusal") || !strings.Contains(string(raw), "goroutine stack fixture") {
		t.Fatalf("event=%s", raw)
	}
}

func TestAnnotationHookPanicsLogAndStayOpen(t *testing.T) {
	oldPlatform := plat
	t.Cleanup(func() { plat = oldPlatform })
	for _, hook := range []string{"posttooluse", "permissionrequest"} {
		t.Run(hook, func(t *testing.T) {
			var out bytes.Buffer
			a, dir := scopeApp(t, &out)
			if err := a.loadConfig(); err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"tool_name":"Bash"}`)
			func() { defer a.recoverHookPanic(hook, &raw); panic("injected annotation failure") }()
			if out.Len() != 0 {
				t.Fatalf("panic produced response: %s", out.String())
			}
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.Events(0, func(e state.Event) bool { return e.Verb == "hook.panic" })
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%v err=%v", events, err)
			}
			for _, want := range []string{hook, tToolBash, "injected annotation failure", "goroutine"} {
				if !strings.Contains(events[0].Detail, want) {
					t.Fatalf("missing %q: %s", want, events[0].Detail)
				}
			}
			a.store = store
			a.now = events[0].At.Add(-time.Second)
			w := &watcher{app: a, last: map[string]time.Time{}, refusedSince: a.now}
			w.refusals()
			w.refusals()
			if strings.Count(out.String(), "HOOK PANIC") != 1 || !strings.Contains(out.String(), hook) {
				t.Fatalf("watch: %s", out.String())
			}
		})
	}
}

type panickingStdin struct{}

func (panickingStdin) Read([]byte) (int, error) { panic("injected setup failure") }

// A panic while recording a panic must not skip the deny: the guard stays
// closed through the real pretooluse hook.
func TestPreToolUseDeniesWhenRecordingThePanicPanics(t *testing.T) {
	oldPlatform, oldOpen := plat, openPanicLog
	t.Cleanup(func() { plat, openPanicLog = oldPlatform, oldOpen })
	var out bytes.Buffer
	a, _ := scopeApp(t, &out)
	if err := a.loadConfig(); err != nil {
		t.Fatal(err)
	}
	recorded := false
	openPanicLog = func(string) (*state.FileStore, error) {
		recorded = true
		panic("injected recording failure")
	}
	c := a.hookCmd()
	c.SetIn(panickingStdin{})
	c.SetArgs([]string{"pretooluse"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !recorded {
		t.Fatal("the recording never ran")
	}
	if !strings.Contains(out.String(), `"deny"`) || !strings.Contains(out.String(), "failing closed") {
		t.Fatalf("no deny: %q", out.String())
	}
}
