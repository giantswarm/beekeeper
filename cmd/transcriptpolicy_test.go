package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

const transcriptEditingMode = "acceptEdits"

const transcriptToolName = "tool_name"
const transcriptPreToolUse = "pretooluse"

const transcriptInputKey = "tool_input"
const transcriptHookName = "hook"
const transcriptGranted = "transcript-grantee"

func TestTranscriptPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{transcriptHostEnv, codex.EnvAgent, agy.EnvAgent, omp.EnvAgent} {
		t.Setenv(key, "")
	}
	store := filepath.Join(home, "custom-conversations")
	own := filepath.Join(store, "own.jsonl")
	cfg := filepath.Join(home, "config.yaml")
	text := "stateDir: " + filepath.Join(home, "state") + "\nclaude:\n  projectsDir: " + store + "\nhooks:\n  transcriptGrants:\n    transcript-grantee:\n      - " + own + "\n"
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(home, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *state.State) ([]state.Event, error) {
		s.Starts = []state.Start{{Party: state.Party{Session: transcriptGranted}}, {Party: state.Party{Session: "transcript-outsider"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		session, p string
		deny       bool
	}{
		{transcriptGranted, own, false}, {"personal-session", filepath.Join(store, "past.jsonl"), false}, {"transcript-outsider", own, true}, {transcriptGranted, filepath.Join(store, "other.jsonl"), true},
	} {
		raw, err := json.Marshal(map[string]any{"session_id": tc.session, transcriptToolName: "Read", transcriptInputKey: map[string]any{"file_path": tc.p}})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		c := New()
		c.SetArgs([]string{"--" + configFlag, cfg, transcriptHookName, transcriptPreToolUse})
		c.SetIn(bytes.NewReader(raw))
		c.SetOut(&out)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		if denied := strings.Contains(out.String(), "\"deny\""); denied != tc.deny {
			t.Errorf("%+v: %s", tc, out.String())
		}
	}
}

func TestTranscriptSessionProvenance(t *testing.T) {
	a, _ := stubApp(t)
	for _, key := range []string{transcriptHostEnv, codex.EnvAgent, agy.EnvAgent, omp.EnvAgent} {
		t.Setenv(key, "")
	}
	seed(t, a, func(s *state.State) {
		s.Starts = []state.Start{{Party: state.Party{Session: transcriptGranted}, Mode: transcriptEditingMode}}
		s.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "transcript-guide"}}}
		s.Agents = []state.Agent{{Party: state.Party{Session: "transcript-person"}}}
	})
	for _, tc := range []struct {
		session string
		managed bool
	}{
		{transcriptGranted, true}, {"transcript-supervisor", false}, {"transcript-guide", true}, {"transcript-person", false}, {"", false},
	} {
		got, err := a.managedTranscriptSession(tc.session)
		if err != nil || got != tc.managed {
			t.Errorf("%s: managed=%v err=%v", tc.session, got, err)
		}
	}
}

func TestTranscriptRunnerIdentity(t *testing.T) {
	a, _ := stubApp(t)
	for _, key := range []string{transcriptHostEnv, codex.EnvAgent, agy.EnvAgent, omp.EnvAgent} {
		t.Setenv(key, "")
	}
	for _, h := range []struct{ env, prefix string }{{codex.EnvAgent, codex.HostPrefix}, {agy.EnvAgent, agy.HostPrefix}, {omp.EnvAgent, omp.HostPrefix}} {
		seed(t, a, func(s *state.State) {
			s.Starts = []state.Start{{Party: state.Party{HostSession: h.prefix + transcriptGranted}}}
		})
		t.Setenv(h.env, transcriptGranted)
		if got, err := a.managedTranscriptSession("new-harness-session"); !got || err != nil {
			t.Errorf("%s alias: managed=%v err=%v", h.env, got, err)
		}
		t.Setenv(h.env, "")
	}
	seed(t, a, func(s *state.State) { s.Starts = []state.Start{{Party: state.Party{HostSession: restartHost}}} })
	t.Setenv(transcriptHostEnv, restartHost)
	if got, err := a.managedTranscriptSession(restartedCLI); !got || err != nil {
		t.Errorf("restarted CLI: managed=%v err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(a.cfg.StateDir, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.managedTranscriptSession(restartedCLI); err == nil {
		t.Fatal("unreadable provenance did not fail closed")
	}
}
