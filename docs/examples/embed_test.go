package examples_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/docs/examples"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
)

func TestConfigProtectsHomeFolder(t *testing.T) {
	const (
		writeTool   = "Write"
		filePathKey = "file_path"
		draftFile   = "draft.md"
	)
	home := t.TempDir()
	t.Setenv("BEEKEEPER_EXECUTION_HOME", home)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(examples.Config), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.Hooks.PersonFolders, home) {
		t.Errorf("example omits home from the protected folders: %q", cfg.Hooks.PersonFolders)
	}
	hook := guard.Hook{PersonFolders: cfg.Hooks.PersonFolders}
	for _, tc := range []struct {
		name, tool string
		input      map[string]any
		deny       bool
	}{
		{"home file", writeTool, map[string]any{filePathKey: filepath.Join(home, draftFile)}, true},
		{"home redirection", "Bash", map[string]any{"command": "printf draft > draft.md"}, true},
		{"projects file", writeTool, map[string]any{filePathKey: filepath.Join(home, "projects", draftFile)}, true},
		{"scratch file", writeTool, map[string]any{filePathKey: filepath.Join(home, ".local", "state", draftFile)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event, err := json.Marshal(map[string]any{
				"session_id": "example-config-regression",
				"cwd":        home,
				"tool_name":  tc.tool,
				"tool_input": tc.input,
			})
			if err != nil {
				t.Fatal(err)
			}
			var answer struct {
				Output struct {
					Decision string `json:"permissionDecision"`
					Reason   string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if out := hook.Decide(event); len(out) > 0 {
				if err := json.Unmarshal(out, &answer); err != nil {
					t.Fatal(err)
				}
			}
			if denied := answer.Output.Decision == "deny"; denied != tc.deny {
				t.Fatalf("denied = %t, want %t; reason: %s", denied, tc.deny, answer.Output.Reason)
			}
			if tc.deny && !strings.Contains(answer.Output.Reason, "hooks.personFolders") {
				t.Errorf("refused for another reason: %s", answer.Output.Reason)
			}
		})
	}
}

func TestConfigSweepKeepsHomeWithAdditionalRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BEEKEEPER_EXECUTION_HOME", home)
	// A desk can add a sweep root while retaining the example's home entry.
	var document map[string]any
	if err := yaml.Unmarshal([]byte(examples.Config), &document); err != nil {
		t.Fatal(err)
	}
	outbound := document["outbound"].(map[string]any)
	roots := outbound["sweepRoots"].([]any)
	outbound["sweepRoots"] = append(roots, "~/shared")
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{home, filepath.Join(home, "shared")}
	if !slices.Equal(cfg.Outbound.SweepRoots, want) {
		t.Errorf("sweep roots = %q, want %q", cfg.Outbound.SweepRoots, want)
	}
}
