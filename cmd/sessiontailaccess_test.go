package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

func tailAccessApp(t *testing.T) (*app, []*claude.Session) {
	t.Helper()
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.as = ""
	for _, key := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_NAME", transcriptHostEnv, codex.EnvAgent, agy.EnvAgent, omp.EnvAgent} {
		t.Setenv(key, "")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "tail-worker")
	var sessions []*claude.Session
	for _, id := range []string{"tail-worker", "tail-peer", "tail-other", "tail-personal", "tail-guide"} {
		path := filepath.Join(a.cfg.StateDir, id+".jsonl")
		body := `{"type":"assistant","timestamp":"2026-11-03T09:00:00Z","message":{"content":[{"type":"text","text":"synthetic conversation"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"synthetic"}}]}}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		repo := "example/service"
		if id == "tail-other" {
			repo = "example/other"
		}
		sessions = append(sessions, &claude.Session{ID: id, Name: id, Repo: repo, Transcript: path})
	}
	seed(t, a, func(st *state.State) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "tail-guide", Name: "Guide"}}}
		for _, id := range []string{"tail-worker", "tail-peer", "tail-other"} {
			repo := "example/service"
			if id == "tail-other" {
				repo = "example/other"
			}
			p := state.Party{Session: id, Name: id}
			st.Starts = append(st.Starts, state.Start{Party: p})
			st.Agents = append(st.Agents, state.Agent{Party: p, Task: "https://github.com/" + repo + "/issues/1"})
		}
	})
	return a, sessions
}

func TestSessionTailAppliesTranscriptAccess(t *testing.T) {
	for _, tc := range []struct {
		name, caller, target, as string
		grant                    time.Duration
		allow                    bool
	}{
		{name: "own", caller: "tail-worker", target: "tail-worker", allow: true},
		{name: "same repository", caller: "tail-worker", target: "tail-peer", allow: true},
		{name: "guide refused", caller: "tail-worker", target: "tail-guide"},
		{name: "other repository refused", caller: "tail-worker", target: "tail-other"},
		{name: "personal refused", caller: "tail-worker", target: "tail-personal"},
		{name: "as guide refused", caller: "tail-worker", target: "tail-guide", as: "guide"},
		{name: "expired grant", caller: "tail-worker", target: "tail-guide", grant: -time.Hour},
		{name: "active guide grant", caller: "tail-worker", target: "tail-guide", grant: time.Hour, allow: true},
		{name: "active other grant", caller: "tail-worker", target: "tail-other", grant: time.Hour, allow: true},
		{name: "current guide", caller: "tail-guide", target: "tail-personal", allow: true},
		{name: "operator", target: "tail-guide", allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, sessions := tailAccessApp(t)
			t.Setenv("CLAUDE_CODE_SESSION_ID", tc.caller)
			a.as = tc.as
			if tc.grant != 0 {
				seed(t, a, func(st *state.State) {
					st.TranscriptGrants = []state.TranscriptGrant{{To: state.Party{Session: tc.caller}, Session: tc.target, Expires: a.now.Add(tc.grant)}}
				})
			}
			for _, tools := range []bool{false, true} {
				turns, err := a.sessionTail(sessions, tc.target, 10, tools, a.now)
				if tc.allow {
					if err != nil || len(turns) == 0 {
						t.Fatalf("tools=%v: turns=%v err=%v", tools, turns, err)
					}
					if tools && len(turns) < 2 {
						t.Fatal("tool turn missing")
					}
				} else if Code(err) != ExitRefused || len(turns) != 0 {
					t.Fatalf("tools=%v: turns=%v err=%v", tools, turns, err)
				}
			}
		})
	}
}

func TestTailCommandAndUIRefuseGuideTranscriptToWorker(t *testing.T) {
	a, _ := tailAccessApp(t)
	// The guide can be resolved from state and its synthetic ended transcript.
	cmd := a.tailCmd()
	cmd.SetArgs([]string{"Guide"})
	if err := cmd.Execute(); Code(err) != ExitRefused || !strings.Contains(err.Error(), "requires access") {
		t.Fatalf("CLI: %v", err)
	}
	turns, err := (&collector{a: a}).Tail(context.Background(), "Guide", 10)
	if Code(err) != ExitRefused || len(turns) != 0 || !strings.Contains(err.Error(), "requires access") {
		t.Fatalf("UI: turns=%v err=%v", turns, err)
	}
}
