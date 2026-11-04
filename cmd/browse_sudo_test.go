package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBrowseSudoFindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn.jsonl")
	var transcript strings.Builder
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		transcript.Write(b)
		transcript.WriteByte('\n')
	}
	// The browser's tab metadata, rather than the agent's report, identifies the barrier.
	write(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "tabs", "name": chromeToolPrefix + "tabs_context_mcp", "input": map[string]any{}}}}})
	tabs := `{"availableTabs":[{"tabId":7,"title":"Confirm access","url":"https://github.com/organizations/example/settings/apps/demo"}]}`
	write(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tabs", "content": tabs}}}})
	// A later page on the same tab must not erase evidence of the barrier.
	write(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tabs", "content": strings.ReplaceAll(tabs, "Confirm access", "Other page")}}}})
	if err := os.WriteFile(path, []byte(transcript.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := readFindings(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "barrier: github_sudo setting=token_expiry,device_flow,callback_urls"
	if !slices.Contains(f.lines("steps", "", nil, false), want) {
		t.Fatalf("missing barrier: %+v", f)
	}
	if !strings.Contains(browsePrompt("steps", "", nil, false, nil), githubSudoWords) {
		t.Fatal("missing sudo stop instructions")
	}
}
