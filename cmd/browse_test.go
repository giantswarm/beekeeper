package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A browse turn is one headless turn in bypass on the CLI's own Chrome
// connection, a session of its own: none of its caller's session variables
// reach it.
func TestBrowseTurn(t *testing.T) {
	argv := browseArgv("id-1", "", "steps")
	want := []string{"-p", chromeFlag, "--tools", "", "--strict-mcp-config", permissionModeFlag, "dontAsk",
		"--allowedTools", "mcp__claude-in-chrome__*", "--session-id", "id-1", "--", "steps"}
	if !slices.Equal(argv, want) {
		t.Errorf("browseArgv = %q, want %q", argv, want)
	}
	// The child has the Chrome tools and nothing else: no built-in tool, no
	// other MCP server, no allow rule beyond Chrome's, never bypass.
	for i, arg := range argv {
		switch arg {
		case "--tools":
			if argv[i+1] != "" {
				t.Errorf("--tools %q, want none", argv[i+1])
			}
		case "--allowedTools", "--allowed-tools":
			if argv[i+1] != browseTools {
				t.Errorf("allowed tools %q, want only %q", argv[i+1], browseTools)
			}
		case "bypassPermissions", "--dangerously-skip-permissions", "--mcp-config":
			t.Errorf("browseArgv carries %q", arg)
		}
	}
	if argv := browseArgv("id-1", "sonnet", "-steps"); !slices.Contains(argv, "sonnet") || argv[len(argv)-2] != "--" {
		t.Errorf("browseArgv with a model = %q", argv)
	}
	env := browseEnv([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=claude-desktop", "CLAUDE_CODE_SESSION_ID=x", "CLAUDE_PID=1", "CLAUDE_CONFIG_DIR=/c", "HOME=/h"})
	if !slices.Equal(env, []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/c", "HOME=/h"}) {
		t.Errorf("browseEnv = %q", env)
	}
}

// Every inline image of a tool result becomes a file, in order, with the
// extension of its media type; other content is skipped.
func TestSaveScreenshots(t *testing.T) {
	dir := t.TempDir()
	png, jpg := base64.StdEncoding.EncodeToString([]byte("png-bytes")), base64.StdEncoding.EncodeToString([]byte("jpg-bytes"))
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__claude-in-chrome__computer"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"ID: ss_1"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}]}}`,
		`{"type":"user","message":{"content":"plain text"}}`,
		`not json`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + jpg + `"}}]}]}}`,
	}
	transcript := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shots, err := saveScreenshots(transcript, filepath.Join(dir, "out"))
	if err != nil || len(shots) != 2 || !strings.HasSuffix(shots[0], "shot-1.png") || !strings.HasSuffix(shots[1], "shot-2.jpg") {
		t.Fatalf("saveScreenshots = %q, %v", shots, err)
	}
	if b, _ := os.ReadFile(shots[1]); string(b) != "jpg-bytes" {
		t.Errorf("shot 2 holds %q", b)
	}
}
