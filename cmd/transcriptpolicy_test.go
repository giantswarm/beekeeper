package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const transcriptToolName = "tool_name"
const transcriptPreToolUse = "pretooluse"

const transcriptInputKey = "tool_input"
const transcriptHookName = "hook"
const transcriptGranted = "transcript-grantee"

func TestTranscriptPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := filepath.Join(home, "custom-conversations")
	own := filepath.Join(store, "own.jsonl")
	cfg := filepath.Join(home, "config.yaml")
	text := "stateDir: " + filepath.Join(home, "state") + "\nclaude:\n  projectsDir: " + store + "\nhooks:\n  transcriptGrants:\n    transcript-grantee:\n      - " + own + "\n"
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		session, p string
		deny       bool
	}{
		{transcriptGranted, own, false}, {"transcript-outsider", own, true}, {transcriptGranted, filepath.Join(store, "other.jsonl"), true},
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
