package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const panicCWDKey = "cwd"

func TestHookWorktreeMoveColdState(t *testing.T) {
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
			raw, _ := json.Marshal(map[string]any{"hook_event_name": guard.PreToolUseEvent, panicCWDKey: source, transcriptToolName: tToolBash, transcriptInputKey: map[string]any{hookTypeCommand: "git worktree move " + source + " " + filepath.Join(dir, "aside")}})
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
