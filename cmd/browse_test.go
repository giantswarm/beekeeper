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
	"time"
)

// A browse turn is one headless turn in dontAsk on the CLI's own Chrome
// connection, a session of its own: none of its caller's session variables
// reach it. A declared deploy is the turn's settings, an auto mode allow
// rule for that one action after the shipped rules, and its prompt's word
// between the preamble and the steps; without one the turn gets no settings.
func TestBrowseTurn(t *testing.T) {
	argv := browseArgv("id-1", "", "steps", "")
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
		case "bypassPermissions", "--dangerously-skip-permissions", "--mcp-config", "--settings":
			t.Errorf("browseArgv carries %q", arg)
		}
	}
	if argv := browseArgv("id-1", "sonnet", "-steps", ""); !slices.Contains(argv, "sonnet") || argv[len(argv)-2] != "--" {
		t.Errorf("browseArgv with a model = %q", argv)
	}
	const deploy = "the demo agent to the demo cluster"
	argv = browseArgv("id-1", "", "steps", deploy)
	i := slices.Index(argv, "--settings")
	if i < 0 || i+1 >= len(argv) || argv[len(argv)-2] != "--" || slices.Index(argv, "--") < i {
		t.Fatalf("browseArgv with a deploy = %q, want --settings before --", argv)
	}
	var settings struct {
		AutoMode struct {
			Allow                           []string
			Environment, SoftDeny, HardDeny []string `json:",omitempty"`
		}
	}
	if err := json.Unmarshal([]byte(argv[i+1]), &settings); err != nil {
		t.Fatalf("--settings %q: %v", argv[i+1], err)
	}
	allow := settings.AutoMode.Allow
	if len(allow) != 2 || allow[0] != "$defaults" || allow[1] != allowDeployRule(deploy) {
		t.Errorf("autoMode.allow = %q, want the shipped rules then the deploy's", allow)
	}
	if rule := allow[1]; !strings.Contains(rule, "exactly one deploy or create action in a portal: "+deploy+".") ||
		!strings.Contains(rule, "Production Deploy") || !strings.Contains(rule, "Not covered:") {
		t.Errorf("the deploy's rule = %q", rule)
	}
	if n := strings.Count(argv[i+1], "$defaults"); n != 1 || strings.Contains(argv[i+1], "soft_deny") || strings.Contains(argv[i+1], "environment") {
		t.Errorf("--settings touches more than the allow list: %s", argv[i+1])
	}
	if p := browsePrompt("steps", ""); p != browsePreamble+browseSteps+"steps" {
		t.Errorf("browsePrompt without a deploy = %q", p)
	}
	if p := browsePrompt("steps", deploy); !strings.HasPrefix(p, browsePreamble+"The person's task covers one deploy: "+deploy+".") ||
		!strings.HasSuffix(p, browseSteps+"steps") {
		t.Errorf("browsePrompt with a deploy = %q", p)
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
	got := f.lines(steps, "")
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
	if l := other.lines(steps, ""); len(l) != 1 || !strings.HasSuffix(l[0], "[Destructive]") {
		t.Errorf("lines of another reason = %q", l)
	}
	grant := browseFindings{Refused: []refusal{{Call: "tabs_context_mcp", Reason: reasonGrant}}}
	if l := grant.lines("Open the page and take a screenshot", ""); len(l) != 2 || l[1] != consentHint {
		t.Errorf("lines without a consent step = %q", l)
	}
	if l := (browseFindings{}).lines(steps, ""); l != nil {
		t.Errorf("lines of no findings = %q", l)
	}
	// A long step is quoted bounded.
	long := "Then click Authorize on the page " + strings.Repeat("x", 200)
	if p := consentPhrase(long); len([]rune(p)) != phraseRunes+1 || !strings.HasSuffix(p, "…") || !strings.HasPrefix(p, "Then click Authorize") {
		t.Errorf("consentPhrase(long) = %q", p)
	}
	if n := (browseFindings{NotRendered: 3}).lines("", ""); len(n) != 1 || !strings.HasPrefix(n[0], "not rendered: 3 screenshots timed out") {
		t.Errorf("lines of 3 not rendered = %q", n)
	}
	// A deploy refusal names the refused click and, without a declared
	// deploy, the flag; with one, the declaration that did not cover it. A
	// declared deploy is the first line, refusal or not.
	click := `browser_batch computer left_click "Deploys the demo agent to the demo cluster", computer wait`
	refusedDeploy := browseFindings{Refused: []refusal{{Call: click, Reason: "Production Deploy"}}}
	if l := refusedDeploy.lines(steps, ""); len(l) != 2 || l[0] != "refused: "+click+": the auto mode classifier denied it as [Production Deploy]" || l[1] != deployHint {
		t.Errorf("lines of a deploy refusal = %q", l)
	}
	const deploy = "the demo agent to the demo cluster"
	if l := refusedDeploy.lines(steps, deploy); len(l) != 3 || l[0] != "allowed deploy: "+deploy+": the turn's auto mode allowed that one deploy or create click (--allow-deploy)" ||
		!strings.HasPrefix(l[1], "refused: ") || l[2] != `the declared deploy "`+deploy+`" did not cover it: declare the refused call's own action and target` {
		t.Errorf("lines of a deploy refusal under a declared deploy = %q", l)
	}
	if l := (browseFindings{}).lines(steps, deploy); len(l) != 1 || !strings.HasPrefix(l[0], "allowed deploy: "+deploy) {
		t.Errorf("lines of a declared deploy without findings = %q", l)
	}
	for _, reason := range deployReasons {
		if l := (browseFindings{Refused: []refusal{{Call: click, Reason: reason}}}).lines(steps, ""); len(l) != 2 || l[1] != deployHint {
			t.Errorf("lines of a [%s] refusal = %q, want the deploy hint", reason, l)
		}
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

// The turn runs the claude on PATH with the browse argv: dontAsk, the Chrome
// tools alone, and with a declared deploy the settings whose auto mode
// allows it and a prompt that names it; the report and the lines below it
// come from the turn's output and its transcript.
func TestBrowseRunsTheTurn(t *testing.T) {
	a, out := stubApp(t)
	bin := t.TempDir()
	argvFile, transcript := filepath.Join(bin, "argv"), filepath.Join(bin, "transcript.jsonl")
	// The fake claude records its arguments NUL-separated (a prompt holds
	// newlines), writes the transcript under the session id it was given and
	// prints its report.
	script := "#!/bin/sh\n: > \"$BROWSE_ARGV\"\nid=; prev=\nfor a in \"$@\"; do\n  printf '%s\\0' \"$a\" >> \"$BROWSE_ARGV\"\n" +
		"  [ \"$prev\" = --session-id ] && id=$a\n  prev=$a\ndone\nmkdir -p \"$BROWSE_PROJECT\" && cp \"$BROWSE_TRANSCRIPT\" \"$BROWSE_PROJECT/$id.jsonl\"\n" +
		"echo \"the turn's report\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSE_ARGV", argvFile)
	t.Setenv("BROWSE_TRANSCRIPT", transcript)
	t.Setenv("BROWSE_PROJECT", filepath.Join(a.cfg.Claude.ProjectsDir, "p"))
	click := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__claude-in-chrome__computer","input":{"action":"left_click","ref":"ref_9","action_summary":"Deploys the demo agent to the demo cluster"}}]}}` + "\n"
	denial := "Permission for this action was denied by the Claude Code auto mode classifier. Reason: [Production Deploy]. If you have other tasks, continue."
	argvOf := func() []string {
		b, err := os.ReadFile(argvFile) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	const steps = "Open the demo page. Click Deploy agent once. Report what the page says."

	// Without the flag the turn gets no settings, and its refused click is
	// named below the report with the flag to declare it.
	writeFile(t, transcript, click+`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"`+denial+`"}]}}`+"\n")
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, ""); err != nil {
		t.Fatalf("browse: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) < 5 || got[0] != "the turn's report" ||
		got[1] != `refused: computer left_click "Deploys the demo agent to the demo cluster": the auto mode classifier denied it as [Production Deploy]` ||
		got[2] != deployHint || !strings.HasPrefix(got[3], "transcript: ") || got[4] != "screenshot: none taken" {
		t.Errorf("browse without the flag printed %q", got)
	}
	argv := argvOf()
	if i := slices.Index(argv, permissionModeFlag); i < 0 || argv[i+1] != modeDontAsk || slices.Contains(argv, settingsFlag) ||
		argv[len(argv)-1] != browsePrompt(steps, "") || !slices.Contains(argv, chromeFlag) {
		t.Errorf("the turn without the flag ran with %q", argv)
	}

	// With the flag the turn gets the settings whose auto mode allows the
	// declared deploy and a prompt that names it, and the report says so.
	const deploy = "the demo agent to the demo cluster"
	writeFile(t, transcript, click+`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"Clicked"}]}}`+"\n")
	out.Reset()
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, deploy); err != nil {
		t.Fatalf("browse --allow-deploy: %v\n%s", err, out.String())
	}
	got = strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 4 || got[0] != "the turn's report" || !strings.HasPrefix(got[1], "allowed deploy: "+deploy+": ") ||
		!strings.HasPrefix(got[2], "transcript: ") || got[3] != "screenshot: none taken" {
		t.Errorf("browse --allow-deploy printed %q", got)
	}
	argv = argvOf()
	i := slices.Index(argv, settingsFlag)
	if i < 0 || argv[i+1] != allowDeploySettings(deploy) || argv[len(argv)-1] != browsePrompt(steps, deploy) {
		t.Errorf("the turn with the flag ran with %q", argv)
	}
	if i := slices.Index(argv, permissionModeFlag); i < 0 || argv[i+1] != modeDontAsk {
		t.Errorf("the flag changed the turn's mode: %q", argv)
	}
}
