package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	tailAccessWorker   = "tail-access-worker"
	tailAccessPeer     = "tail-access-peer"
	tailAccessOther    = "tail-access-other"
	tailAccessPersonal = "tail-access-personal"
	tailAccessText     = "synthetic conversation text"
)

func tailAccessApp(t *testing.T) (*app, []*claude.Session) {
	t.Helper()
	for _, key := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_NAME", transcriptHostEnv, codex.EnvAgent, agy.EnvAgent, omp.EnvAgent} {
		t.Setenv(key, "")
	}
	a, _ := newTranscriptTestApp(t)
	parties := []state.Party{
		{Session: transcriptTestGuideSession, Name: guideName},
		{Session: tailAccessWorker, Name: tailAccessWorker},
		{Session: tailAccessPeer, Name: tailAccessPeer},
		{Session: tailAccessOther, Name: tailAccessOther},
		{Session: tailAccessPersonal, Name: tailAccessPersonal},
	}
	seed(t, a, func(st *state.State) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: parties[0]}}
		for i, p := range parties[1:4] {
			task := transcriptTestIssue
			if i == 2 {
				task = "https://github.com/example/other/issues/400"
			}
			st.Agents = append(st.Agents, state.Agent{Party: p, Task: task})
			st.Starts = append(st.Starts, state.Start{Party: p})
		}
	})
	var sessions []*claude.Session
	for _, p := range parties {
		path := filepath.Join(a.cfg.StateDir, p.Session+".jsonl")
		line := `{"type":"assistant","timestamp":"2026-11-03T09:00:00Z","message":{"content":[{"type":"text","text":"` + tailAccessText + `"}]}}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, &claude.Session{ID: p.Session, Name: p.Name, Transcript: path})
	}
	return a, sessions
}

func TestSessionTailAppliesTranscriptAccess(t *testing.T) {
	for _, tools := range []bool{false, true} {
		for _, tc := range []struct {
			name, caller, target, as string
			grant                    bool
			elapsed                  time.Duration
			allowed                  bool
		}{
			{name: "calling worker conversation", caller: tailAccessWorker, target: tailAccessWorker, allowed: true},
			{name: "same repository worker", caller: tailAccessWorker, target: tailAccessPeer, allowed: true},
			{name: "guide denied", caller: tailAccessWorker, target: guideName},
			{name: "other repository denied", caller: tailAccessWorker, target: tailAccessOther},
			{name: "personal session denied", caller: tailAccessWorker, target: tailAccessPersonal},
			{name: "guide override denied", caller: tailAccessWorker, target: guideName, as: guideName},
			{name: "active session grant", caller: tailAccessWorker, target: guideName, grant: true, allowed: true},
			{name: "expired grant", caller: tailAccessWorker, target: guideName, grant: true, elapsed: 2 * time.Hour},
			{name: "current guide", caller: transcriptTestGuideSession, target: tailAccessOther, allowed: true},
			{name: "outside a harness", target: guideName, allowed: true},
		} {
			t.Run(fmt.Sprintf("%s/tools=%v", tc.name, tools), func(t *testing.T) {
				a, sessions := tailAccessApp(t)
				t.Setenv("CLAUDE_CODE_SESSION_ID", tc.caller)
				a.as = tc.as
				if tc.grant {
					seed(t, a, func(st *state.State) {
						st.TranscriptGrants = []state.TranscriptGrant{{To: state.Party{Session: tailAccessWorker}, Session: transcriptTestGuideSession, Expires: a.now.Add(time.Hour)}}
					})
				}
				turns, err := a.sessionTail(sessions, tc.target, 3, tools, a.now.Add(tc.elapsed))
				if tc.allowed {
					if err != nil || len(turns) != 1 || turns[0].Text != tailAccessText {
						t.Fatalf("allowed tail: turns=%v err=%v", turns, err)
					}
				} else if Code(err) != ExitRefused || len(turns) != 0 {
					t.Fatalf("private transcript was returned: turns=%v err=%v", turns, err)
				}
			})
		}
	}
}

func TestTailCommandAndUIRefuseGuideTranscriptToWorker(t *testing.T) {
	a, _ := tailAccessApp(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tailAccessWorker)
	command := a.tailCmd()
	command.SetArgs([]string{guideName})
	if err := command.Execute(); Code(err) != ExitRefused {
		t.Error("tail command returned the guide's transcript to a worker")
	}
	if turns, err := (&collector{a: a}).Tail(context.Background(), guideName, 3); Code(err) != ExitRefused || len(turns) != 0 {
		t.Errorf("UI returned the guide's transcript to a worker: %v, %v", turns, err)
	}
}
