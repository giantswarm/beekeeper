package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	sudoKeyType    = "type"
	sudoKeyMessage = "message"
	sudoKeyContent = "content"
	sudoKeyName    = "name"
	sudoRoleUser   = "user"
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
	write(map[string]any{sudoKeyType: "assistant", sudoKeyMessage: map[string]any{sudoKeyContent: []any{map[string]any{sudoKeyType: "tool_use", "id": litTabs, sudoKeyName: chromeToolPrefix + "tabs_context_mcp", "input": map[string]any{}}}}})
	tabs := `{"availableTabs":[{"tabId":7,"title":"Confirm access","url":"https://github.com/organizations/example/settings/apps/demo"}]}`
	write(map[string]any{sudoKeyType: sudoRoleUser, sudoKeyMessage: map[string]any{sudoKeyContent: []any{map[string]any{sudoKeyType: "tool_result", "tool_use_id": "tabs", sudoKeyContent: tabs}}}})
	// A later page on the same tab must not erase evidence of the barrier.
	write(map[string]any{sudoKeyType: sudoRoleUser, sudoKeyMessage: map[string]any{sudoKeyContent: []any{map[string]any{sudoKeyType: "tool_result", "tool_use_id": "tabs", sudoKeyContent: strings.ReplaceAll(tabs, "Confirm access", "Other page")}}}})
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
