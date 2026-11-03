package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProjectInstructions(t *testing.T) {
	root := repoWith(t, t.TempDir(), map[string]string{
		claudeMD:                        "Claude rule\n@docs/import.md\nMust read `docs/required.md`\n@../outside.md\n@docs/credential.txt\n",
		agentsMD:                        "Agent rule overrides Claude rule",
		"docs/import.md":                "Imported rule\n@cycle.md",
		"docs/cycle.md":                 "@import.md",
		"docs/required.md":              "Required rule",
		".claude/rules/merge.md":        "Always squash merge",
		".claude/rules/nested/check.md": "Verify the rollout",
		"docs/credential.txt":           "SECRET CONTENT",
		".claude/settings.json":         `{"hooks":{},"env":{"KEY":"PRIVATE"}}`,
	})
	got := ProjectInstructions(root, 1<<20)
	for _, want := range []string{"Claude rule", "Agent rule overrides", "Imported rule", "Required rule", "Always squash merge", "Verify the rollout", "AGENTS.md takes precedence", "whether tracked by Git or not"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, avoid := range []string{"SECRET CONTENT", "PRIVATE", "your first change"} {
		if strings.Contains(got, avoid) {
			t.Errorf("unexpected %q in %s", avoid, got)
		}
	}
	if strings.Count(got, "=== docs/import.md ===") != 1 {
		t.Fatal("imports must be deduplicated")
	}
}

func TestProjectInstructionsNativeAgentsUnchanged(t *testing.T) {
	for _, files := range []map[string]string{nil, {agentsMD: "native rules"}} {
		root := repoWith(t, t.TempDir(), files)
		if got := ProjectInstructions(root, 1<<20); got != "" {
			t.Fatalf("native instructions changed: %s", got)
		}
	}
	root := repoWith(t, t.TempDir(), map[string]string{agentsMD: "native rules", ".claude/rules/a.md": "additional rule"})
	if got := ProjectInstructions(root, 1<<20); !strings.Contains(got, "native rules") || !strings.Contains(got, "additional rule") {
		t.Fatal(got)
	}
}

func TestProjectInstructionsDirectoryScope(t *testing.T) {
	root := repoWith(t, t.TempDir(), map[string]string{claudeMD: "root rule", "sub/CLAUDE.md": "sub rule", "sub/.claude/rules/a.md": "sub extra"})
	got := ProjectInstructions(filepath.Join(root, "sub"), 1<<20)
	if !strings.Contains(got, "root rule") || !strings.Contains(got, "sub rule") || !strings.Contains(got, "sub extra") {
		t.Fatal(got)
	}
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, claudeMD), []byte("plain rule"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProjectInstructions(plain, 1<<20); !strings.Contains(got, "plain rule") {
		t.Fatal(got)
	}
}

func TestProjectInstructionsBudget(t *testing.T) {
	root := repoWith(t, t.TempDir(), map[string]string{claudeMD: strings.Repeat("こんにちは\n", 30000), ".claude/rules/extra.md": "extra"})
	for _, budget := range []int{-1, 0, 1, 200, 700, 1500, 262144} {
		got := ProjectInstructions(root, budget)
		if len(got) > max(0, budget) || !utf8.ValidString(got) {
			t.Fatalf("budget %d: length %d, valid UTF-8 %v", budget, len(got), utf8.ValidString(got))
		}
		if budget >= 1500 && (!strings.Contains(got, "truncated") || !strings.Contains(got, "extra.md")) {
			t.Fatalf("budget %d: truncation must be visible: %s", budget, got)
		}
	}
}

func TestProjectInstructionsSymlink(t *testing.T) {
	root := repoWith(t, t.TempDir(), map[string]string{"docs/project.md": "linked Claude rule"})
	if err := os.Symlink(filepath.Join(root, "docs", "project.md"), filepath.Join(root, claudeMD)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := ProjectInstructions(root, 1<<20); !strings.Contains(got, "linked Claude rule") {
		t.Fatal(got)
	}
}
