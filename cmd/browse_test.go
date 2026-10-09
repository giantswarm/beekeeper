package cmd

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A browse turn is one headless turn in bypass on the CLI's own Chrome
// connection, a session of its own: none of its caller's session variables
// reach it.
func TestBrowseTurn(t *testing.T) {
	argv := browseArgv("id-1", "", "steps")
	want := []string{"-p", chromeFlag, toolsFlag, "", strictMCPConfigFlag, permissionModeFlag, "dontAsk",
		allowedToolsFlag, "mcp__claude-in-chrome__*", "--session-id", "id-1", "--", "steps"}
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

// A tool result carrying the classifier's denial names the call it answers
// in the call's own words with the classifier's reason, whether its content
// is a string or text blocks; a screenshot that timed out on a frozen
// renderer counts as not rendered; the lines quote the step that reads as a
// grant once, for a [Permission Grant] only.
func TestReadFindings(t *testing.T) {
	denial := "Permission for this action was denied by the Claude Code auto mode classifier. Reason: [Permission Grant]. If you have other tasks, continue."
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__claude-in-chrome__tabs_context_mcp","input":{"createIfEmpty":true}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"` + denial + `"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"mcp__claude-in-chrome__computer","input":{"action":"left_click","ref":"ref_45","tabId":1,"action_summary":"Clicks Authorize for the App"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"` + denial + `"}]}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"mcp__claude-in-chrome__browser_batch","input":{"actions":[{"name":"navigate","input":{"url":"https://example.test/"}},{"name":"computer","input":{"action":"screenshot","scale":0.5}}]}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","is_error":true,"content":"[navigate] Navigated to https://example.test/\n\nactions[1] (computer) failed: Error capturing screenshot: CDP sendCommand \"Page.captureScreenshot\" timed out after 30000ms on tab 1. The renderer may be frozen or unresponsive. (1 completed, 0 remaining)"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","is_error":false,"content":"Successfully captured screenshot"}]}}`,
		`{"type":"user","message":{"content":"plain text"}}`,
		`not json`,
	}
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := readFindings(transcript)
	want := browseFindings{Refused: []refusal{
		{Call: "tabs_context_mcp", Reason: "Permission Grant"},
		{Call: `computer left_click "Clicks Authorize for the App"`, Reason: "Permission Grant"},
	}, NotRendered: 1}
	if err != nil || !reflect.DeepEqual(f, want) {
		t.Fatalf("readFindings = %+v, %v; want %+v", f, err, want)
	}
	steps := "Open https://example.test/sign-in. On GitHub's page, click Authorize once; report the final URL."
	got := f.lines(steps)
	wantLines := []string{
		"refused: tabs_context_mcp: the auto mode classifier denied it as [Permission Grant]",
		`refused: computer left_click "Clicks Authorize for the App": the auto mode classifier denied it as [Permission Grant]`,
		`the steps' "On GitHub's page, click Authorize once" reads as a grant: ` + consentHint,
		"not rendered: 1 screenshot timed out on a frozen renderer: the page's Chrome window is not drawn (occluded, or on another workspace), " +
			"where a consent page keeps its Authorize button disabled; bring the window to the front and run the steps again",
	}
	if !slices.Equal(got, wantLines) {
		t.Errorf("lines = %q, want %q", got, wantLines)
	}
	// Another reason gets no hint; a grant refused with steps that name no
	// grant gets the bare hint; nothing found prints nothing.
	other := browseFindings{Refused: []refusal{{Call: "navigate https://example.test/", Reason: "Destructive"}}}
	if l := other.lines(steps); len(l) != 1 || !strings.HasSuffix(l[0], "[Destructive]") {
		t.Errorf("lines of another reason = %q", l)
	}
	grant := browseFindings{Refused: []refusal{{Call: "tabs_context_mcp", Reason: reasonGrant}}}
	if l := grant.lines("Open the page and take a screenshot"); len(l) != 2 || l[1] != consentHint {
		t.Errorf("lines without a consent step = %q", l)
	}
	if l := (browseFindings{}).lines(steps); l != nil {
		t.Errorf("lines of no findings = %q", l)
	}
	// A long step is quoted bounded.
	long := "Then click Authorize on the page " + strings.Repeat("x", 200)
	if p := consentPhrase(long); len([]rune(p)) != phraseRunes+1 || !strings.HasSuffix(p, "…") || !strings.HasPrefix(p, "Then click Authorize") {
		t.Errorf("consentPhrase(long) = %q", p)
	}
	if n := (browseFindings{NotRendered: 3}).lines(""); len(n) != 1 || !strings.HasPrefix(n[0], "not rendered: 3 screenshots timed out") {
		t.Errorf("lines of 3 not rendered = %q", n)
	}
	// A batch's words carry its actions' in order; a call without an input
	// is its tool's name.
	batch := json.RawMessage(`{"actions":[{"name":"navigate","input":{"url":"https://example.test/"}},{"name":"computer","input":{"action":"wait"}}]}`)
	if w := callWords("mcp__claude-in-chrome__browser_batch", batch); w != "browser_batch navigate https://example.test/, computer wait" {
		t.Errorf("callWords(batch) = %q", w)
	}
	if w := callWords("mcp__claude-in-chrome__find", nil); w != "find" {
		t.Errorf("callWords(find) = %q", w)
	}
}
