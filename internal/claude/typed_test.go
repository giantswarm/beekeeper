package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A prompt typed in the desktop counts; the brief of a start, peer
// messages, notices, notifications, tool results and meta entries do not.
func TestPersonTyped(t *testing.T) {
	notTypedLines := []string{
		`{"type":"user","entrypoint":"sdk-cli","message":{"role":"user","content":"the brief"}}`,
		`{"type":"user","entrypoint":"claude-desktop","isMeta":true,"message":{"role":"user","content":"Continue from where you left off."}}`,
		`{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message>go</cross-session-message>"}}`,
		`{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":"[Cross-session delivery notice] held"}}`,
		`{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":[{"type":"text","text":"<task-notification>done</task-notification>"}]}}`,
		`{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":[{"type":"tool_result","content":"ok"},{"type":"text","text":"note"}]}}`,
		`{"type":"assistant","entrypoint":"claude-desktop","message":{"role":"assistant","content":"merge it"}}`,
	}
	dir := t.TempDir()
	write := func(name string, lines []string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if typed, err := PersonTyped(write("worker.jsonl", notTypedLines)); err != nil || typed {
		t.Errorf("a worker's transcript: typed %v, %v", typed, err)
	}
	typedLine := `{"type":"user","entrypoint":"claude-desktop","message":{"role":"user","content":[{"type":"text","text":"still no links. what went wrong?"}]}}`
	if typed, err := PersonTyped(write("typed.jsonl", append(notTypedLines, typedLine))); err != nil || !typed {
		t.Errorf("a typed prompt: typed %v, %v", typed, err)
	}
}
