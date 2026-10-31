package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestNonClaudeDryRunProjectInstructions(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, ".claude", "rules")
	if err := os.MkdirAll(rules, 0o700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{filepath.Join(dir, "CLAUDE.md"): "project rule", filepath.Join(rules, "merge.md"): "squash every merge"} {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []Harness{codexHarness{}, agyHarness{}, ompHarness{}} {
		t.Run(h.Name(), func(t *testing.T) {
			var out bytes.Buffer
			a := &app{cfg: &config.Config{}, out: &out}
			sp := agentStart{dir: dir, name: "worker", brief: "TASK ONLY", model: "test-model"}
			if err := h.DryRun(context.Background(), a, sp, nil); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			for _, want := range []string{"project rule", "squash every merge", projectPrompt(dir, sp.brief)} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %s", want, got)
				}
			}
			if strings.Index(got, "squash every merge") > strings.Index(got, sp.brief) {
				t.Fatal("instructions must precede task")
			}
		})
	}
}

func TestProjectPromptNativeAgentsUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("native rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := projectPrompt(dir, "task"); got != "task" {
		t.Fatal(got)
	}
}
