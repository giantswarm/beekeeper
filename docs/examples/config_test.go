package examples_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/docs/examples"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
)

func loadExample(t *testing.T, body string) (*config.Config, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BEEKEEPER_EXECUTION_HOME", home)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, home
}

func TestConfigProtectsHomeFolder(t *testing.T) {
	cfg, home := loadExample(t, examples.Config)
	if !slices.Contains(cfg.Hooks.PersonFolders, home) {
		t.Errorf("person folders %q omit home", cfg.Hooks.PersonFolders)
	}
	h := guard.Hook{PersonFolders: cfg.Hooks.PersonFolders}
	for _, tc := range []struct {
		name, tool string
		input      map[string]any
		deny       bool
	}{
		{"home file", "Write", map[string]any{"file_path": filepath.Join(home, "draft.md"), "content": "draft"}, true},
		{"home redirection", "Bash", map[string]any{"command": "printf draft > draft.md"}, true},
		{"projects file", "Write", map[string]any{"file_path": filepath.Join(home, "projects", "draft.md"), "content": "draft"}, true},
		{"scratch file", "Write", map[string]any{"file_path": filepath.Join(home, ".local", "state", "notes", "draft.md"), "content": "draft"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tc.tool, "tool_input": tc.input, "cwd": home})
			if err != nil {
				t.Fatal(err)
			}
			out := h.Decide(input)
			var result struct {
				Output struct {
					Decision string `json:"permissionDecision"`
					Reason   string `json:"permissionDecisionReason"`
				} `json:"hookSpecificOutput"`
			}
			if len(out) > 0 {
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatal(err)
				}
			}
			if got := result.Output.Decision == "deny"; got != tc.deny {
				t.Fatalf("denied = %v, want %v; response %s", got, tc.deny, out)
			}
			if tc.deny && !strings.Contains(result.Output.Reason, "hooks.personFolders") {
				t.Errorf("refusal does not name policy: %s", out)
			}
		})
	}
}

func TestConfigSweepKeepsHomeWithAdditionalRoot(t *testing.T) {
	// Extend the shipped list without changing its existing home entry.
	const marker = "sweepRoots: ["
	before, after, ok := strings.Cut(examples.Config, marker)
	if !ok {
		t.Fatal("example sweepRoots list missing")
	}
	roots, rest, ok := strings.Cut(after, "]")
	if !ok {
		t.Fatal("example sweepRoots list is unterminated")
	}
	cfg, home := loadExample(t, before+marker+roots+", ~/shared]"+rest)
	want := []string{home, filepath.Join(home, "shared")}
	if !slices.Equal(cfg.Outbound.SweepRoots, want) {
		t.Errorf("sweep roots = %q, want %q", cfg.Outbound.SweepRoots, want)
	}
}
