package cmd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The session id of the argv tests, the beekeeper binary their Stop hook
// names, and the Chrome call that reads the tabs.
const (
	turnID      = "id-1"
	turnSelf    = "/bin/beekeeper"
	tabsContext = "tabs_context_mcp"
)

// selfSettings are the settings of a turn browse runs in this test binary.
func selfSettings(t *testing.T, rules []string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return browseSettings(self, rules)
}

// A browse turn is one headless turn in dontAsk on the CLI's own Chrome
// connection, a session of its own: none of its caller's session variables
// reach it, nor the caller's stage root. A declared deploy is the turn's settings, an auto mode allow
// rule for that one action after the shipped rules, and its prompt's word
// between the preamble and the steps; without one the turn's settings carry
// its Stop hook alone, which has it close the tabs it opened.
func TestBrowseTurn(t *testing.T) {
	argv := browseArgv(turnID, "", "steps", turnSelf, nil, false)
	want := []string{"-p", chromeFlag, toolsFlag, "", strictMCPConfigFlag, permissionModeFlag, "dontAsk",
		allowedToolsFlag, "mcp__claude-in-chrome__*", "--session-id", turnID, settingsFlag,
		`{"hooks":{"Stop":[{"hooks":[{"command":"/bin/beekeeper hook browsestop","timeout":10,"type":"command"}]}]}}`, "--", "steps"}
	if !slices.Equal(argv, want) {
		t.Errorf("browseArgv = %q, want %q", argv, want)
	}
	// The reconnect turn resumes the session with the same tools and mode.
	want[9], want[10] = resumeFlag, turnID
	if argv := browseArgv(turnID, "", "steps", turnSelf, nil, true); !slices.Equal(argv, want) {
		t.Errorf("browseArgv of the reconnect = %q, want %q", argv, want)
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
	if argv := browseArgv(turnID, "sonnet", "-steps", turnSelf, nil, false); !slices.Contains(argv, "sonnet") || argv[len(argv)-2] != "--" {
		t.Errorf("browseArgv with a model = %q", argv)
	}
	const deploy = "the demo agent to the demo cluster"
	argv = browseArgv(turnID, "", "steps", turnSelf, browseRules(deploy, nil, false), false)
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
	// Every prompt has the connection words: the check before each step,
	// the one reconnect in the turn, the lost line and the steps not done.
	if p := browsePrompt("steps", "", nil, false, nil); p != browsePreamble+connectionWords+frontWords+drawWords+leaveWords+windowWords+browseSteps+"steps" ||
		!strings.Contains(connectionWords, "list_connected_browsers before each step") || !strings.Contains(connectionWords, "select_browser") ||
		!strings.Contains(connectionWords, "'"+lostLine+"' followed by the steps not done") {
		t.Errorf("browsePrompt without a deploy = %q", p)
	}
	if p := browsePrompt("steps", deploy, nil, false, nil); !strings.HasPrefix(p, browsePreamble+connectionWords+frontWords+drawWords+leaveWords+windowWords+"The person's task covers one deploy: "+deploy+".") ||
		!strings.HasSuffix(p, browseSteps+"steps") {
		t.Errorf("browsePrompt with a deploy = %q", p)
	}
	env := browseEnv([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=claude-desktop", "CLAUDE_CODE_SESSION_ID=x", "CLAUDE_PID=1", stageRootEnv + "=/s", "CLAUDE_CONFIG_DIR=/c", "HOME=/h"})
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
		{Call: tabsContext, Reason: "Permission Grant"},
		{Call: `computer left_click "Clicks Authorize for the App"`, Reason: "Permission Grant"},
	}, NotRendered: 1, Tabs: []int64{1}}
	if err != nil || !reflect.DeepEqual(f, want) {
		t.Fatalf("readFindings = %+v, %v; want %+v", f, err, want)
	}
	steps := "Open https://example.test/sign-in. On GitHub's page, click Authorize once; report the final URL."
	got := f.lines(steps, "", nil, false)
	wantLines := []string{
		"refused: tabs_context_mcp: the auto mode classifier denied it as [Permission Grant]",
		`refused: computer left_click "Clicks Authorize for the App": the auto mode classifier denied it as [Permission Grant]`,
		`the steps' "On GitHub's page, click Authorize once" reads as a grant: ` + consentHint,
		"not rendered: 1 screenshot timed out on a frozen renderer: the tab was not drawn (behind another tab, or its window occluded or " +
			"minimised, which the compositor does not draw), where a consent page keeps its Authorize button disabled; the hook refused every " +
			"later screenshot and the turn read the page's text instead of its picture; bring the Chrome window forward and run the steps again for a screenshot",
		"windows: the turn worked in tab 1",
	}
	if !slices.Equal(got, wantLines) {
		t.Errorf("lines = %q, want %q", got, wantLines)
	}
	// Another reason gets no hint; a grant refused with steps that name no
	// grant gets the bare hint; nothing found prints nothing.
	other := browseFindings{Refused: []refusal{{Call: "navigate https://example.test/", Reason: "Destructive"}}}
	if l := other.lines(steps, "", nil, false); len(l) != 1 || !strings.HasSuffix(l[0], "[Destructive]") {
		t.Errorf("lines of another reason = %q", l)
	}
	grant := browseFindings{Refused: []refusal{{Call: tabsContext, Reason: reasonGrant}}}
	if l := grant.lines("Open the page and take a screenshot", "", nil, false); len(l) != 2 || l[1] != consentHint {
		t.Errorf("lines without a consent step = %q", l)
	}
	if l := (browseFindings{}).lines(steps, "", nil, false); l != nil {
		t.Errorf("lines of no findings = %q", l)
	}
	// A long step is quoted bounded.
	long := "Then click Authorize on the page " + strings.Repeat("x", 200)
	if p := consentPhrase(long); len([]rune(p)) != phraseRunes+1 || !strings.HasSuffix(p, "…") || !strings.HasPrefix(p, "Then click Authorize") {
		t.Errorf("consentPhrase(long) = %q", p)
	}
	if n := (browseFindings{NotRendered: 3}).lines("", "", nil, false); len(n) != 1 || !strings.HasPrefix(n[0], "not rendered: 3 screenshots timed out") {
		t.Errorf("lines of 3 not rendered = %q", n)
	}
	// A deploy refusal names the refused click and, without a declared
	// deploy, the flag; with one, the declaration that did not cover it. A
	// declared deploy is the first line, refusal or not.
	click := `browser_batch computer left_click "Deploys the demo agent to the demo cluster", computer wait`
	refusedDeploy := browseFindings{Refused: []refusal{{Call: click, Reason: "Production Deploy"}}}
	if l := refusedDeploy.lines(steps, "", nil, false); len(l) != 2 || l[0] != "refused: "+click+": the auto mode classifier denied it as [Production Deploy]" || l[1] != deployHint {
		t.Errorf("lines of a deploy refusal = %q", l)
	}
	const deploy = "the demo agent to the demo cluster"
	if l := refusedDeploy.lines(steps, deploy, nil, false); len(l) != 3 || l[0] != "allowed deploy: "+deploy+": the turn's auto mode allowed that one deploy or create click (--allow-deploy)" ||
		!strings.HasPrefix(l[1], "refused: ") || l[2] != `the declared deploy "`+deploy+`" did not cover it: declare the refused call's own action and target` {
		t.Errorf("lines of a deploy refusal under a declared deploy = %q", l)
	}
	if l := (browseFindings{}).lines(steps, deploy, nil, false); len(l) != 1 || !strings.HasPrefix(l[0], "allowed deploy: "+deploy) {
		t.Errorf("lines of a declared deploy without findings = %q", l)
	}
	for _, reason := range deployReasons {
		if l := (browseFindings{Refused: []refusal{{Call: click, Reason: reason}}}).lines(steps, "", nil, false); len(l) != 2 || l[1] != deployHint {
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

// chromeUse and chromeAnswer are one transcript line each: a Chrome call by
// its tool after the prefix, and the plain text its result carries, no tool
// error.
func chromeUse(id, tool, input string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + chromeToolPrefix + tool + `","input":` + input + `}]}}` + "\n"
}

func chromeAnswer(id, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":false,"content":` + string(b) + `}]}}` + "\n"
}

// What the CLI answers a Chrome call with while the extension's connection
// is gone, the list of connected browsers then, and the click of the
// connection tests with its words.
const (
	goneAnswer   = "⚠️ **Claude in Chrome is not connected**\n\nThe Chrome extension isn't reachable right now. If browser tools were working earlier in this session, this is usually transient — retry in a moment."
	noBrowsers   = "[]"
	oneBrowser   = `[{"deviceId":"3998c3cb","name":"Browser 1","osPlatform":"Linux","isLocal":true,"inUse":true}]`
	deployClick  = `{"action":"left_click","ref":"ref_9","action_summary":"Clicks Deploy agent"}`
	deployCall   = `computer left_click "Clicks Deploy agent"`
	notDoneLines = "- Click Deploy agent once\n- Report what the page says"
)

// A Chrome call answered "not connected", in the CLI's or the desktop's
// words, or a list of no browser, is a lost call, no tool error needed; a
// Chrome call answered after the last such one has the connection back, a
// classifier denial does not; the lines count the lost calls, name the
// first and say what came of the reconnect; the report's lines after its
// lost line are the steps not done, which the error names.
func TestReadFindingsConnection(t *testing.T) {
	read := func(t *testing.T, lines string) browseFindings {
		t.Helper()
		transcript := filepath.Join(t.TempDir(), "t.jsonl")
		writeFile(t, transcript, lines)
		f, err := readFindings(transcript)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// Lost twice, then the list names a browser and a call is answered: the
	// connection is back, the turn went on by itself.
	back := read(t, chromeUse("t1", "navigate", `{"url":"https://example.test/"}`)+chromeAnswer("t1", "Navigated to https://example.test/")+
		chromeUse("t2", "computer", `{"action":"screenshot"}`)+chromeAnswer("t2", goneAnswer)+
		chromeUse("t3", "list_connected_browsers", `{}`)+chromeAnswer("t3", noBrowsers)+
		chromeUse("t4", "list_connected_browsers", `{}`)+chromeAnswer("t4", oneBrowser)+
		chromeUse("t5", "computer", `{"action":"screenshot"}`)+chromeAnswer("t5", "Successfully captured screenshot"))
	if !slices.Equal(back.Lost, []string{"computer screenshot", "list_connected_browsers"}) || !back.Recovered || back.lost() || back.lostErr("report") != nil {
		t.Errorf("findings of a connection that came back = %+v, %v", back, back.lostErr("report"))
	}
	if l := back.lines("steps", "", nil, false); len(l) != 1 ||
		l[0] != "connection lost: 2 calls found Claude in Chrome not connected, the first computer screenshot; the connection answered again and the turn went on" {
		t.Errorf("lines of a connection that came back = %q", l)
	}
	// Lost at the end, the list empty and a denial after it: gone; the
	// desktop's words and the account-wide list are lost calls too.
	denial := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","is_error":true,"content":"Permission for this action was denied by the Claude Code auto mode classifier. Reason: [Destructive]."}]}}` + "\n"
	gone := read(t, chromeUse("t1", "navigate", `{"url":"https://example.test/"}`)+chromeAnswer("t1", "Navigated to https://example.test/")+
		chromeUse("t2", "computer", deployClick)+chromeAnswer("t2", goneAnswer)+
		chromeUse("t3", "list_connected_browsers", `{}`)+chromeAnswer("t3", noBrowsers)+
		chromeUse("t4", "computer", deployClick)+denial)
	if !slices.Equal(gone.Lost, []string{deployCall, "list_connected_browsers"}) || gone.Recovered || !gone.lost() || len(gone.Refused) != 1 {
		t.Errorf("findings of a connection that stayed gone = %+v", gone)
	}
	for _, text := range []string{"Browser extension is not connected. Please ensure the Claude browser extension is installed", "No browsers connected to the account."} {
		if f := read(t, chromeUse("t1", tabsContext, `{}`)+chromeAnswer("t1", text)); !slices.Equal(f.Lost, []string{tabsContext}) || !f.lost() {
			t.Errorf("findings of %q = %+v", text, f)
		}
	}
	if f := read(t, chromeUse("t1", "list_connected_browsers", `{}`)+chromeAnswer("t1", oneBrowser)); f.Lost != nil || f.lost() || f.Recovered {
		t.Errorf("findings of a listed browser = %+v", f)
	}
	// Before a reconnect the line says the turn ended without the
	// connection; after one that found no browser, that; after one that
	// found it, that it went on.
	if l := gone.lines("steps", "", nil, false); len(l) != 2 ||
		l[1] != "connection lost: 2 calls found Claude in Chrome not connected, the first "+deployCall+"; the turn ended without it, before a reconnect" {
		t.Errorf("lines of a connection gone before a reconnect = %q", l)
	}
	gone.Reconnected = true
	if l := gone.lines("steps", "", nil, false); len(l) != 2 || l[1] != "connection lost: 2 calls found Claude in Chrome not connected, the first "+deployCall+
		"; the reconnect turn, a fresh connection, found no browser: the steps not done are the report's, to run again once the extension is connected (list_connected_browsers)" {
		t.Errorf("lines of a reconnect that found no browser = %q", l)
	}
	back.Reconnected = true
	if l := back.lines("steps", "", nil, false); len(l) != 1 || !strings.HasSuffix(l[0], "; the reconnect turn, a fresh connection, found the browser and went on with the steps not done") {
		t.Errorf("lines of a reconnect that found the browser = %q", l)
	}
	if l := (browseFindings{Lost: []string{tabsContext}}).lines("steps", "", nil, false); len(l) != 1 || !strings.HasPrefix(l[0], "connection lost: 1 call found ") {
		t.Errorf("lines of one lost call = %q", l)
	}
	// The error names the lost call, the reconnect's outcome and the steps
	// the report lists after its lost line, list marks dropped; a report
	// without the line says so.
	report := "Opened the page, the connection went at the click.\n\n**" + lostLine + "**\n" + notDoneLines + "\n"
	if err := gone.lostErr(report); err == nil || err.Error() != lostLine+" at "+deployCall+": the reconnect turn found no browser; steps not done, to run again once the extension is connected (list_connected_browsers): "+
		"Click Deploy agent once; Report what the page says" {
		t.Errorf("lostErr after the reconnect = %v", err)
	}
	gone.Reconnected = false
	if err := gone.lostErr("Opened the page."); err == nil || err.Error() != lostLine+" at "+deployCall+": the turn did not get it back; the report lists no steps not done: run the steps again once the extension is connected (list_connected_browsers)" {
		t.Errorf("lostErr without the steps = %v", err)
	}
	for report, want := range map[string][]string{
		"Browser connection lost:\n1. Open the page\n2) Click Deploy\n\n• Report":                            {"Open the page", "Click Deploy", "Report"},
		"Done: the page opened.\n\n" + lostLine + "\n- Click Deploy agent once\n- Report what the page says": {"Click Deploy agent once", "Report what the page says"},
		"The browser connection lost its tab; all steps done.":                                               nil,
		"": nil,
	} {
		if got := stepsNotDone(report); !slices.Equal(got, want) {
			t.Errorf("stepsNotDone(%q) = %q, want %q", report, got, want)
		}
	}
}

// What the fake claude of the turn tests prints, and the line of a turn that
// took no screenshot.
const (
	turnReport = "the turn's report"
	noShots    = "screenshot: none taken"
	theGuide   = "the guide"
)

// A connection the turn lost and did not get back is reconnected once: a
// second claude resumes the session (the same tools, mode and rules, the
// reconnect prompt naming the lost call), its report is printed after the
// first and its findings are read with the first turn's. A reconnect that
// finds the browser ends the run well; one that finds none ends it with the
// lost error naming the steps the report lists; a connection the turn got
// back by itself runs no reconnect.
func TestBrowseReconnects(t *testing.T) {
	a, out := stubApp(t)
	bin := t.TempDir()
	argvFile, transcript, reportFile, count := filepath.Join(bin, "argv"), filepath.Join(bin, "transcript"), filepath.Join(bin, "report"), filepath.Join(bin, "count")
	// The fake claude counts its runs, records each run's arguments, appends
	// that run's transcript under the session it starts or resumes and
	// prints that run's report.
	script := "#!/bin/sh\nn=$(cat \"$BROWSE_COUNT\" 2>/dev/null || echo 0); n=$((n+1)); echo \"$n\" > \"$BROWSE_COUNT\"\n" +
		": > \"$BROWSE_ARGV.$n\"\nid=; prev=\nfor a in \"$@\"; do\n  printf '%s\\0' \"$a\" >> \"$BROWSE_ARGV.$n\"\n" +
		"  case \"$prev\" in --session-id|--resume) id=$a;; esac\n  prev=$a\ndone\n" +
		"mkdir -p \"$BROWSE_PROJECT\" && cat \"$BROWSE_TRANSCRIPT.$n\" >> \"$BROWSE_PROJECT/$id.jsonl\"\ncat \"$BROWSE_REPORT.$n\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSE_ARGV", argvFile)
	t.Setenv("BROWSE_TRANSCRIPT", transcript)
	t.Setenv("BROWSE_REPORT", reportFile)
	t.Setenv("BROWSE_COUNT", count)
	t.Setenv("BROWSE_PROJECT", filepath.Join(a.cfg.Claude.ProjectsDir, "p"))
	argvOf := func(n string) []string {
		b, err := os.ReadFile(argvFile + "." + n) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	runs := func() string {
		b, _ := os.ReadFile(count) //nolint:gosec // the test's own file
		return strings.TrimSpace(string(b))
	}
	const steps = "Open the demo page. Click Deploy agent once. Report what the page says."
	lostTurn := chromeUse("t1", "navigate", `{"url":"https://example.test/"}`) + chromeAnswer("t1", "Navigated to https://example.test/") +
		chromeUse("t2", "computer", deployClick) + chromeAnswer("t2", goneAnswer) +
		chromeUse("t3", "list_connected_browsers", `{}`) + chromeAnswer("t3", noBrowsers)
	const lostReport = "Opened the page; the connection went at the click.\n" + lostLine + "\n" + notDoneLines + "\n"

	// The reconnect finds the browser and goes on.
	writeFile(t, transcript+".1", lostTurn)
	writeFile(t, reportFile+".1", lostReport)
	writeFile(t, transcript+".2", chromeUse("t4", "list_connected_browsers", `{}`)+chromeAnswer("t4", oneBrowser)+
		chromeUse("t5", "computer", deployClick)+chromeAnswer("t5", "Clicked on element ref_9"))
	writeFile(t, reportFile+".2", "Clicked Deploy agent; the page says deployed.\n")
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, "", nil, nil); err != nil {
		t.Fatalf("browse with a reconnect that found the browser: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 8 || got[0] != "Opened the page; the connection went at the click." || got[1] != lostLine || got[3] != "- Report what the page says" ||
		got[4] != "Clicked Deploy agent; the page says deployed." ||
		got[5] != "connection lost: 2 calls found Claude in Chrome not connected, the first "+deployCall+"; the reconnect turn, a fresh connection, found the browser and went on with the steps not done" ||
		!strings.HasPrefix(got[6], "transcript: ") || got[7] != noShots {
		t.Errorf("browse with a reconnect printed %q", got)
	}
	if runs() != "2" {
		t.Errorf("the fake claude ran %s times, want 2", runs())
	}
	first, second := argvOf("1"), argvOf("2")
	id := first[slices.Index(first, sessionIDFlag)+1]
	if i := slices.Index(second, resumeFlag); i < 0 || second[i+1] != id || slices.Contains(second, sessionIDFlag) ||
		second[len(second)-1] != fmt.Sprintf(reconnectWords, deployCall) || !slices.Contains(second, chromeFlag) ||
		!slices.Equal(second[:i], first[:slices.Index(first, sessionIDFlag)]) {
		t.Errorf("the reconnect ran with %q, the first turn with %q", second, first)
	}

	// The reconnect finds no browser: the lost error names the steps the
	// reconnect's report lists, and the line says so.
	out.Reset()
	if err := os.Remove(count); err != nil {
		t.Fatal(err)
	}
	writeFile(t, transcript+".2", chromeUse("t4", "list_connected_browsers", `{}`)+chromeAnswer("t4", noBrowsers))
	writeFile(t, reportFile+".2", lostLine+"\n"+notDoneLines+"\n")
	err := a.browse(t.Context(), steps, bin, "", time.Minute, "", nil, nil)
	if err == nil || err.Error() != lostLine+" at "+deployCall+": the reconnect turn found no browser; steps not done, to run again once the extension is connected (list_connected_browsers): "+
		"Click Deploy agent once; Report what the page says" {
		t.Errorf("browse with a reconnect that found no browser: %v", err)
	}
	got = strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 10 || got[4] != lostLine || got[6] != "- Report what the page says" ||
		!strings.HasPrefix(got[7], "connection lost: 3 calls found Claude in Chrome not connected, the first "+deployCall+"; the reconnect turn, a fresh connection, found no browser: ") ||
		!strings.HasPrefix(got[8], "transcript: ") || got[9] != noShots {
		t.Errorf("browse with a reconnect that found no browser printed %q", got)
	}
	if runs() != "2" {
		t.Errorf("the fake claude ran %s times, want 2", runs())
	}

	// A connection the turn got back by itself runs no reconnect.
	out.Reset()
	if err := os.Remove(count); err != nil {
		t.Fatal(err)
	}
	writeFile(t, transcript+".1", lostTurn+chromeUse("t4", "list_connected_browsers", `{}`)+chromeAnswer("t4", oneBrowser)+
		chromeUse("t5", "computer", deployClick)+chromeAnswer("t5", "Clicked on element ref_9"))
	writeFile(t, reportFile+".1", "Clicked Deploy agent after a reconnect; the page says deployed.\n")
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, "", nil, nil); err != nil {
		t.Fatalf("browse with a connection that came back: %v\n%s", err, out.String())
	}
	got = strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 4 || got[1] != "connection lost: 2 calls found Claude in Chrome not connected, the first "+deployCall+"; the connection answered again and the turn went on" {
		t.Errorf("browse with a connection that came back printed %q", got)
	}
	if runs() != "1" {
		t.Errorf("the fake claude ran %s times, want 1", runs())
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
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, "", nil, nil); err != nil {
		t.Fatalf("browse: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) < 5 || got[0] != turnReport ||
		got[1] != `refused: computer left_click "Deploys the demo agent to the demo cluster": the auto mode classifier denied it as [Production Deploy]` ||
		got[2] != deployHint || !strings.HasPrefix(got[3], "transcript: ") || got[4] != noShots {
		t.Errorf("browse without the flag printed %q", got)
	}
	argv := argvOf()
	if i := slices.Index(argv, permissionModeFlag); i < 0 || argv[i+1] != modeDontAsk || argv[slices.Index(argv, settingsFlag)+1] != selfSettings(t, nil) ||
		argv[len(argv)-1] != browsePrompt(steps, "", nil, false, nil) || !slices.Contains(argv, chromeFlag) {
		t.Errorf("the turn without the flag ran with %q", argv)
	}

	// With the flag the turn gets the settings whose auto mode allows the
	// declared deploy and a prompt that names it, and the report says so.
	const deploy = "the demo agent to the demo cluster"
	writeFile(t, transcript, click+`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"Clicked"}]}}`+"\n")
	out.Reset()
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, deploy, nil, nil); err != nil {
		t.Fatalf("browse --allow-deploy: %v\n%s", err, out.String())
	}
	got = strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 4 || got[0] != turnReport || !strings.HasPrefix(got[1], "allowed deploy: "+deploy+": ") ||
		!strings.HasPrefix(got[2], "transcript: ") || got[3] != noShots {
		t.Errorf("browse --allow-deploy printed %q", got)
	}
	argv = argvOf()
	i := slices.Index(argv, settingsFlag)
	if i < 0 || argv[i+1] != selfSettings(t, []string{allowDeployRule(deploy)}) || argv[len(argv)-1] != browsePrompt(steps, deploy, nil, false, nil) {
		t.Errorf("the turn with the flag ran with %q", argv)
	}
	if i := slices.Index(argv, permissionModeFlag); i < 0 || argv[i+1] != modeDontAsk {
		t.Errorf("the flag changed the turn's mode: %q", argv)
	}
}

// Steps that name an App on record, by name or one of its callback hosts, give the turn
// that App's auto mode rule and prompt words; steps that name a loopback
// host give it the lab's; the rules stand after the shipped ones beside a
// declared deploy's; the report says what was allowed, and a grant refusal
// under a record says the record did not cover it.
func TestBrowseConsent(t *testing.T) {
	rec := []state.App{{Name: recordedApp, ClientID: recordedClientID, Callbacks: []string{recordedCallback, recordedCallback + ":8443"}, Declared: recordedDeclared,
		Word: personsWord, By: state.Party{Name: theGuide}, At: time.Date(2026, 10, 10, 8, 16, 0, 0, time.UTC)}}
	for steps, want := range map[string]int{
		"Open https://example.org/signin. On GitHub's page '" + recordedTitle + "' click Authorize once.": 1,
		"Open https://" + recordedCallback + "/connect/github and click Authorize.":                       1,
		"Open https://" + recordedCallback + ":8443/connect/github and click Authorize.":                  1,
		"Open https://example.org/ and report the title.":                                                 0,
	} {
		if got := appsNamed(guardApps(rec), steps); len(got) != want || (want == 1 && got[0].Name != recordedApp) {
			t.Errorf("appsNamed(%q) = %+v, want %d", steps, got, want)
		}
	}
	for steps, want := range map[string]bool{
		"Open https://dex.127.0.0.1.nip.io/dex/auth and sign in as admin@lab.local":  true,
		"Open https://localhost:32000/dex/auth":                                      true,
		"Open https://github-bearer.lab2.127.0.0.2.nip.io/login and click Authorize": true,
		"Open https://dex.10.0.0.2.nip.io/dex/auth":                                  false,
		"Open http://127.0.0.2:8080/.":                                               true,
		"Open http://127.0.0.1:8080/":                                                true,
		"Open https://example.org/ and https://portal.example.org/":                  false,
		"Open https://my-localhost.example.org/":                                     false,
	} {
		if got := namesLoopback(steps); got != want {
			t.Errorf("namesLoopback(%q) = %v, want %v", steps, got, want)
		}
	}
	change := state.App{Name: deviceApp, Org: fixtureOrg, Permission: deviceChange, Declared: deviceDeclared, Word: personsWord, By: state.Party{Name: theGuide}, At: rec[0].At}
	if got := appsNamed(guardApps(append(rec, change)), "Open the permissions page of "+deviceApp+" and set Members to Read-only."); len(got) != 1 || got[0].Permission != deviceChange {
		t.Errorf("appsNamed of a permission record = %+v", got)
	}
	both := guardApps(append(rec, change))
	if rules := browseRules("", both, false); len(rules) != 2 || rules[0] != consentRule(both[0]) || rules[1] != permissionRule(both[1]) ||
		!strings.Contains(rules[1], "Own App Permission: The person's organisation declares the App "+deviceApp+" in "+deviceDeclared) ||
		!strings.Contains(rules[1], "one change of its permissions: "+deviceChange+".") || !strings.Contains(rules[1], "organizations/example/settings/apps/"+deviceApp+"/permissions") ||
		!strings.Contains(rules[1], "Not covered: any other permission") {
		t.Errorf("browseRules with a permission record = %q", rules)
	}
	if p := browsePrompt("steps", "", both, false, nil); !strings.Contains(p, "The person's own App "+deviceApp+" is declared and on record for one permission change, "+deviceChange+": once the steps reach its permissions page (organizations/example/settings/apps/"+deviceApp+"/permissions)") ||
		!strings.Contains(p, "The person's own App "+recordedApp+" is declared and on record (callback ") {
		t.Errorf("browsePrompt with a permission record = %q", p)
	}
	callback := state.App{Name: deviceApp, Org: fixtureOrg, CallbackURL: "https://agents.example.org/callback", Declared: deviceDeclared, Word: personsWord, By: state.Party{Name: theGuide}, At: rec[0].At}
	cb := guardApps([]state.App{callback})
	if rules := browseRules("", cb, false); len(rules) != 1 || rules[0] != callbackRule(cb[0]) || !strings.Contains(rules[0], "Own App Callback URL: The person's organisation declares the App "+deviceApp) ||
		!strings.Contains(rules[0], "organizations/example/settings/apps/"+deviceApp+")") || !strings.Contains(rules[0], "Not covered: any other URL, a removal") {
		t.Errorf("browseRules with a callback URL record = %q", rules)
	}
	if p := browsePrompt("steps", "", cb, false, nil); !strings.Contains(p, "The person's own App "+deviceApp+" is declared and on record for one callback URL, \"https://agents.example.org/callback\": ") {
		t.Errorf("browsePrompt with a callback URL record = %q", p)
	}
	if l := (browseFindings{}).lines("steps", "", cb, false); len(l) != 1 || !strings.HasPrefix(l[0], "allowed callback URL: "+deviceApp+" (https://agents.example.org/callback, declared in "+deviceDeclared+")") {
		t.Errorf("lines with a callback URL record = %q", l)
	}
	if l := (browseFindings{}).lines("steps", "", both, false); len(l) != 2 || !strings.HasPrefix(l[0], "allowed consent: ") ||
		!strings.HasPrefix(l[1], "allowed permission: "+deviceApp+" ("+deviceChange+", declared in "+deviceDeclared+"): the hook answers the navigation to its permissions page, the clicks of that change and its installation's Review request and Accept new permissions") ||
		!strings.Contains(l[1], "on record since 2026-10-10 by the guide: "+personsWord) {
		t.Errorf("lines with a permission record = %q", l)
	}
	consents := guardApps(rec)
	rules := browseRules("", consents, true)
	if len(rules) != 2 || rules[0] != consentRule(consents[0]) || rules[1] != labRule {
		t.Errorf("browseRules = %q", rules)
	}
	if r := rules[0]; !strings.Contains(r, "the App "+recordedApp+" (OAuth client id "+recordedClientID+") in "+recordedDeclared) ||
		!strings.Contains(r, "callback host "+recordedCallback+" or "+recordedCallback+":8443") || !strings.Contains(r, "Not covered:") {
		t.Errorf("the consent rule = %q", r)
	}
	if rules := browseRules("", nil, false); rules != nil {
		t.Errorf("browseRules of nothing = %q", rules)
	}
	const deploy = "the demo agent to the demo cluster"
	argv := browseArgv(turnID, "", "steps", turnSelf, browseRules(deploy, consents, false), false)
	i := slices.Index(argv, settingsFlag)
	if i < 0 || argv[i+1] != browseSettings(turnSelf, []string{allowDeployRule(deploy), consentRule(consents[0])}) || strings.Count(argv[i+1], "$defaults") != 1 {
		t.Errorf("--settings with a deploy and a record = %q", argv)
	}
	p := browsePrompt("steps", "", consents, true, nil)
	if !strings.HasPrefix(p, browsePreamble+connectionWords+frontWords+drawWords+leaveWords+windowWords+"The person's own App "+recordedApp+" is declared and on record (callback "+recordedCallback+" or "+recordedCallback+":8443):") ||
		!strings.Contains(p, labWords) || !strings.HasSuffix(p, browseSteps+"steps") {
		t.Errorf("browsePrompt with a record and the lab = %q", p)
	}
	l := (browseFindings{}).lines("steps", "", consents, true)
	if len(l) != 2 || !strings.HasPrefix(l[0], "allowed consent: "+recordedApp+" (callback "+recordedCallback+" or "+recordedCallback+":8443, declared in "+recordedDeclared+"): ") || !strings.Contains(l[0], "on record since 2026-10-10 by the guide: "+personsWord) ||
		!strings.HasPrefix(l[1], "allowed lab sign-in: ") {
		t.Errorf("lines with a record and the lab = %q", l)
	}
	refused := browseFindings{Refused: []refusal{{Call: `computer left_click "Clicks Authorize"`, Reason: reasonGrant}}}
	if l := refused.lines("On the page click Authorize once.", "", consents, false); len(l) != 3 || l[2] != `the steps' "On the page click Authorize once" reads as a grant: the App on record (`+recordedApp+`) did not cover it: `+
		"the page named another App, callback host or permission, or the hook saw no page for the call's tab; the refused call says which" {
		t.Errorf("lines of a grant refusal under a record = %q", l)
	}
	if l := refused.lines("On the page click Authorize once.", "", nil, false); len(l) != 2 || !strings.HasSuffix(l[1], consentHint) || !strings.Contains(consentHint, "beekeeper app allow") ||
		!strings.Contains(consentHint, "--permission") {
		t.Errorf("lines of a grant refusal without a record = %q", l)
	}
}

// A browse turn reads the Apps on record: steps naming one run with its rule
// and prompt words and the report says so; the turn's start record, which
// has the hook act in the turn wherever it runs, exists while the turn runs
// and is gone after it, the turn logged.
func TestBrowseTurnWithARecord(t *testing.T) {
	a, out := stubApp(t)
	bin := t.TempDir()
	argvFile, transcript, startsFile := filepath.Join(bin, "argv"), filepath.Join(bin, "transcript.jsonl"), filepath.Join(bin, "starts")
	// The fake claude records its arguments, copies the state's starts as it
	// sees them while it runs, writes the transcript under its session id
	// and prints its report.
	script := "#!/bin/sh\n: > \"$BROWSE_ARGV\"\nid=; prev=\nfor a in \"$@\"; do\n  printf '%s\\0' \"$a\" >> \"$BROWSE_ARGV\"\n" +
		"  [ \"$prev\" = --session-id ] && id=$a\n  prev=$a\ndone\ncp \"$BROWSE_STATE\" \"$BROWSE_STARTS\"\n" +
		"mkdir -p \"$BROWSE_PROJECT\" && cp \"$BROWSE_TRANSCRIPT\" \"$BROWSE_PROJECT/$id.jsonl\"\necho \"the turn's report\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSE_ARGV", argvFile)
	t.Setenv("BROWSE_TRANSCRIPT", transcript)
	t.Setenv("BROWSE_STARTS", startsFile)
	t.Setenv("BROWSE_STATE", filepath.Join(a.cfg.StateDir, "state.json"))
	t.Setenv("BROWSE_PROJECT", filepath.Join(a.cfg.Claude.ProjectsDir, "p"))
	writeFile(t, transcript, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__claude-in-chrome__computer","input":{"action":"left_click","action_summary":"Clicks Authorize"}}]}}`+"\n"+
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"Clicked"}]}}`+"\n")
	declareApps(t, a, map[string]string{"apps/" + recordedApp + "/manifest.json": recordedManifest})
	if _, err := runApp(t, a, allowArgs(personsWord)...); err != nil {
		t.Fatal(err)
	}
	steps := "Open https://" + recordedCallback + "/connect/github. On GitHub's page '" + recordedTitle + "' click Authorize once; report the final URL."
	if err := a.browse(t.Context(), steps, bin, "", time.Minute, "", nil, nil); err != nil {
		t.Fatalf("browse: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 5 || got[0] != turnReport || !strings.HasPrefix(got[1], "allowed consent: "+recordedApp+" (callback "+recordedCallback+" or "+recordedCallback+":8443, declared in "+recordedDeclared+"): ") ||
		!strings.HasPrefix(got[2], "allowed lab sign-in: ") || !strings.HasPrefix(got[3], "transcript: ") || got[4] != noShots {
		t.Errorf("browse with a record printed %q", got)
	}
	b, err := os.ReadFile(argvFile) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	i := slices.Index(argv, settingsFlag)
	if i < 0 || !strings.Contains(argv[i+1], "Own App Consent: The person's organisation declares the App "+recordedApp) || !strings.Contains(argv[i+1], "Lab Identity Provider:") ||
		!strings.Contains(argv[len(argv)-1], "The person's own App "+recordedApp+" is declared and on record") {
		t.Errorf("the turn with a record ran with %q", argv)
	}
	id := argv[slices.Index(argv, sessionIDFlag)+1]
	var seen state.State
	if raw, err := os.ReadFile(startsFile); err != nil || json.Unmarshal(raw, &seen) != nil || !slices.ContainsFunc(seen.Starts, func(s state.Start) bool { //nolint:gosec // the test's own file
		return s.Session == id && s.Name == browseStart && s.Mode == modeDontAsk && s.Dir == bin && s.By.Name == agentOne
	}) {
		t.Errorf("the state while the turn ran held no start record of it: %+v, %v", seen.Starts, err)
	}
	st, err := a.store.Read()
	if err != nil || slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Session == id }) {
		t.Errorf("the start record outlived the turn: %+v, %v", st.Starts, err)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "browse.turn" }); len(evs) != 1 || !strings.HasPrefix(evs[0].Detail, id+" in "+bin+": Open https://") {
		t.Errorf("browse.turn events = %+v", evs)
	}
}

// A file shared with --file reaches the turn as a copy under the CLI's stage
// root, named in the prompt with the path file_upload takes, while the turn
// keeps no file tool and no settings; the copy goes with the turn, and a line
// below the report names the file.
func TestBrowseSharesFiles(t *testing.T) {
	a, out := stubApp(t)
	bin := t.TempDir()
	argvFile, transcript, seen := filepath.Join(bin, "argv"), filepath.Join(bin, "transcript.jsonl"), filepath.Join(bin, "seen")
	// The fake claude records its arguments and what it finds under the stage
	// root it was given.
	script := "#!/bin/sh\n: > \"$BROWSE_ARGV\"\nid=; prev=\nfor a in \"$@\"; do\n  printf '%s\\0' \"$a\" >> \"$BROWSE_ARGV\"\n" +
		"  [ \"$prev\" = --session-id ] && id=$a\n  prev=$a\ndone\n" +
		"{ echo \"$" + stageRootEnv + "\"; echo \"$" + guard.SharedEnv + "\"; cat \"$" + stageRootEnv + "\"/*/*; } > \"$BROWSE_SEEN\"\n" +
		"mkdir -p \"$BROWSE_PROJECT\" && cp \"$BROWSE_TRANSCRIPT\" \"$BROWSE_PROJECT/$id.jsonl\"\necho \"the turn's report\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSE_ARGV", argvFile)
	t.Setenv("BROWSE_TRANSCRIPT", transcript)
	t.Setenv("BROWSE_SEEN", seen)
	t.Setenv("BROWSE_PROJECT", filepath.Join(a.cfg.Claude.ProjectsDir, "p"))
	t.Setenv(stageRootEnv, "/the/caller's/stage")
	t.Setenv(guard.SharedEnv, "/the/caller's/shared")
	writeFile(t, transcript, "")
	files := filepath.Join(bin, "out")
	shot, log := filepath.Join(files, "shot.png"), filepath.Join(files, "run.log")
	writeFile(t, shot, "png bytes\n")
	writeFile(t, log, "log lines\n")
	const steps = "Open the demo page. Attach shot.png and run.log through its file input."

	if err := a.browse(t.Context(), steps, bin, "", time.Minute, "", []string{shot, log}, nil); err != nil {
		t.Fatalf("browse --file: %v\n%s", err, out.String())
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != 5 || got[0] != turnReport || got[1] != "shared for upload: "+shot || got[2] != "shared for upload: "+log ||
		!strings.HasPrefix(got[3], "transcript: ") || got[4] != noShots {
		t.Errorf("browse --file printed %q", got)
	}
	b, err := os.ReadFile(seen) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	root, rest, _ := strings.Cut(string(b), "\n")
	sharedEnv, copies, _ := strings.Cut(rest, "\n")
	if !strings.HasPrefix(root, filepath.Join(a.cfg.StateDir, "browse")+string(filepath.Separator)) || copies != "png bytes\nlog lines\n" {
		t.Errorf("the turn found the stage root %q holding %q", root, copies)
	}
	// The hook in the turn reads the copies' paths, and only those.
	if want := filepath.Join(root, "1", "shot.png") + string(os.PathListSeparator) + filepath.Join(root, "2", "run.log"); sharedEnv != want {
		t.Errorf("the turn's %s = %q, want %q", guard.SharedEnv, sharedEnv, want)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // the stage root the fake claude reported
		t.Errorf("the stage root %s outlived the turn: %v", root, err)
	}
	argv := strings.Split(strings.TrimSuffix(readFile(t, argvFile), "\x00"), "\x00")
	shared := []sharedFile{{shot, filepath.Join(root, "1", "shot.png")}, {log, filepath.Join(root, "2", "run.log")}}
	if argv[len(argv)-1] != browsePrompt(steps, "", nil, false, shared) || argv[slices.Index(argv, settingsFlag)+1] != selfSettings(t, nil) ||
		argv[slices.Index(argv, toolsFlag)+1] != "" || argv[slices.Index(argv, allowedToolsFlag)+1] != browseTools {
		t.Errorf("the turn with shared files ran with %q", argv)
	}
	p := argv[len(argv)-1]
	if !strings.Contains(p, "and only these: "+shot+" as "+shared[0].Staged+"; "+log+" as "+shared[1].Staged+". ") ||
		!strings.Contains(p, "file_upload refuses every other path") || !strings.Contains(p, guard.UploadTargetScript) || !strings.HasSuffix(p, browseSteps+steps) {
		t.Errorf("the prompt with shared files = %q", p)
	}
	if browsePrompt(steps, "", nil, false, nil) != browsePreamble+connectionWords+frontWords+drawWords+leaveWords+windowWords+browseSteps+steps {
		t.Error("a prompt without shared files names uploads")
	}

	// A file the turn cannot upload is refused before the turn starts: one
	// outside the worker's directories too, directly or through a link.
	elsewhere := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, elsewhere, "not the worker's")
	link := filepath.Join(files, "link.txt")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(files, "big.bin")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, uploadLimit+1); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		files []string
		want  string
	}{
		"missing":  {[]string{shot, filepath.Join(files, "gone.png")}, "gone.png: no such file"},
		"dir":      {[]string{files}, files + ": not a regular file"},
		"too big":  {[]string{big}, "over file_upload's 10485760"},
		"together": {[]string{shot, big}, "over file_upload's 10485760"},
		"outside":  {[]string{shot, elsewhere}, elsewhere + ": outside the worker's directories"},
		"a link":   {[]string{link}, link + ": outside the worker's directories"},
	} {
		if err := os.Remove(argvFile); err != nil {
			t.Fatal(err)
		}
		err := a.browse(t.Context(), steps, bin, "", time.Minute, "", c.files, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("browse --file %s: %v, want %q", name, err, c.want)
		}
		if _, err := os.Stat(argvFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("browse --file %s started the turn", name)
		}
		writeFile(t, argvFile, "")
	}
}

// The CLI ignores a stage root inside the turn's directory or around it, so
// such a root moves to a temporary directory; elsewhere it stays.
func TestStageUploads(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.pdf")
	writeFile(t, src, "pdf")
	dir := t.TempDir()
	if shared, root, err := stageUploads(nil, dir, filepath.Join(dir, "x")); shared != nil || root != "" || err != nil {
		t.Errorf("stageUploads without files = %v, %q, %v", shared, root, err)
	}
	for name, root := range map[string]string{"inside": filepath.Join(dir, "state", "upload"), "same": dir, "around": filepath.Dir(dir)} {
		_, got, err := stageUploads([]string{src}, dir, root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(got) })
		if nested(dir, got) || !strings.HasPrefix(filepath.Base(got), "beekeeper-upload-") {
			t.Errorf("stage root %s of the turn in %s: %s", name, dir, got)
		}
	}
	root := filepath.Join(t.TempDir(), "upload")
	shared, got, err := stageUploads([]string{src}, dir, root)
	if err != nil || got != root || len(shared) != 1 || shared[0] != (sharedFile{src, filepath.Join(root, "1", "a.pdf")}) || readFile(t, shared[0].Staged) != "pdf" {
		t.Errorf("stageUploads = %v, %q, %v", shared, got, err)
	}
	if fi, err := os.Stat(shared[0].Staged); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the copy's mode: %v, %v", fi, err)
	}
}

// The hook's refusal of a file_upload of a path the caller did not share is
// a refusal of the report, the call named with its path, and a line names
// --file.
func TestReadFindingsNotShared(t *testing.T) {
	reason, _ := json.Marshal("PreToolUse:" + chromeToolPrefix + "file_upload hook error: " + guard.NotShared + "/home/x/.ssh/id_ed25519. This browse turn uploads only …")
	lines := chromeUse("u1", "file_upload", `{"paths":["/home/x/.ssh/id_ed25519"],"ref":"ref_42","tabId":3}`) +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","is_error":true,"content":` + string(reason) + `}]}}` + "\n"
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, transcript, lines)
	f, err := readFindings(transcript)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"refused: file_upload /home/x/.ssh/id_ed25519: beekeeper's hook refused it as [Not Shared]",
		sharedHint,
		"windows: the turn worked in tab 3",
	}
	if got := f.lines("Upload the key to the form", "", nil, false); !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

// A file shared for upload lies in one of the worker's directories: the
// working directory, the turn's, beekeeper's state directory or the calling
// session's scratchpad, found by its session id.
func TestUploadRoots(t *testing.T) {
	session := "s-" + strconv.Itoa(os.Getpid())
	pad := filepath.Join(os.TempDir(), "claude-"+strconv.Itoa(os.Getuid()), "-project-"+session, session)
	if err := os.MkdirAll(pad, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(pad)) })
	if got := uploadRoots("/w", "", "/s", session); !slices.Equal(got, []string{"/w", "/s", pad}) {
		t.Errorf("uploadRoots = %q", got)
	}
	if got := uploadRoots("/w", "/d", "/s", "*"); !slices.Equal(got, []string{"/w", "/d", "/s"}) {
		t.Errorf("uploadRoots of a pattern session = %q", got)
	}
	f := filepath.Join(pad, "scratchpad", "out.pdf")
	writeFile(t, f, "pdf")
	if err := checkUploads([]string{f}, uploadRoots("/w", "", "", session)); err != nil {
		t.Errorf("a file in the session's scratchpad: %v", err)
	}
	if err := checkUploads([]string{f}, uploadRoots("/w", "", "", "another")); err == nil || !strings.Contains(err.Error(), "outside the worker's directories (/w)") {
		t.Errorf("a file in another session's scratchpad: %v", err)
	}
}

// readFile is the content of the test's own file.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The hook's refusal of a click on a subscription control is a refusal of
// the report, named in the call's own words, a scripted click's code among
// them; every prompt tells the turn to click only the element a step names
// and never a subscription control.
func TestReadFindingsSubscriptionControl(t *testing.T) {
	reason := "Refused: the click's ref_45 is " + guard.SubscriptionControls + ` ("Subscribe"). A browser call never clicks ` + guard.SubscriptionControls + "."
	refused := func(id string) string {
		b, _ := json.Marshal("PreToolUse:" + chromeToolPrefix + "computer hook error: " + reason)
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":true,"content":` + string(b) + `}]}}` + "\n"
	}
	code := `[...document.querySelectorAll('button')].find(b => b.textContent.includes('edited')).click()`
	script, _ := json.Marshal(map[string]any{"action": "javascript_exec", paramText: code, "tabId": 1})
	lines := chromeUse("c1", "computer", `{"action":"left_click","ref":"ref_45","tabId":1,"action_summary":"Clicks the edited control"}`) + refused("c1") +
		chromeUse("c2", scriptTool, string(script)) + refused("c2") +
		chromeUse("c3", "computer", `{"action":"left_click","ref":"ref_12","tabId":1,"action_summary":"Clicks the edited control"}`) + chromeAnswer("c3", "Clicked")
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := readFindings(transcript)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`refused: computer left_click "Clicks the edited control": beekeeper's hook refused it as [Subscription Control]`,
		`refused: javascript_tool javascript_exec ` + strconv.Quote(code) + `: beekeeper's hook refused it as [Subscription Control]`,
		"windows: the turn worked in tab 1",
	}
	if got := f.lines("Open the issue and read its edit history", "", nil, false); !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if p := browsePrompt("steps", "", nil, false, nil); !strings.Contains(p, "Click only the element a step names") ||
		!strings.Contains(p, "Never click "+guard.SubscriptionControls+" (GitHub's Subscribe, Unsubscribe, Watch, Star and Notifications buttons)") ||
		!strings.Contains(p, "never by a sweep over the page's buttons or links") {
		t.Errorf("browsePrompt = %q, want the control words", p)
	}
}

// A turn's calls name the tabs it worked in, a batch's actions too, and a
// read of a captured window names the window it followed, up to its query;
// the "windows:" line says both, and the prompt tells the turn to work in a
// tab in front and to capture a window the page opens.
func TestReadFindingsWindows(t *testing.T) {
	start := "https://portal.example.test/api/auth/oidc/start?scope=openid&flow=popup&env=production"
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"mcp__claude-in-chrome__navigate","input":{"url":"https://portal.example.test/","tabId":7}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"mcp__claude-in-chrome__javascript_tool","input":{"action":"javascript_exec","tabId":7,"text":"read"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"` + windowOpened + start + `"}]}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"mcp__claude-in-chrome__browser_batch","input":{"actions":[{"name":"navigate","input":{"url":"` + start + `","tabId":7}},{"name":"computer","input":{"action":"screenshot","tabId":9}}]}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t4","name":"mcp__claude-in-chrome__javascript_tool","input":{"action":"javascript_exec","tabId":9,"text":"x"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","content":"` + windowOpened + `none"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"` + windowOpened + start + `"}]}}`,
	}
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := readFindings(transcript)
	if err != nil || !slices.Equal(f.Tabs, []int64{7, 9}) || !slices.Equal(f.Windows, []string{"https://portal.example.test/api/auth/oidc/start"}) {
		t.Fatalf("readFindings = %+v, %v; want tabs 7, 9 and the start window without its query", f, err)
	}
	want := "windows: the turn worked in tab 7, 9; followed 1 window a page opened, in its own tab: https://portal.example.test/api/auth/oidc/start"
	if l := f.lines("steps", "", nil, false); !slices.Equal(l, []string{want}) {
		t.Errorf("lines = %q, want %q", l, want)
	}
	if l := (browseFindings{Windows: []string{"https://a.test/x", "https://b.test/y"}}).windowsLine(); l != "windows: the turn named no tab; followed 2 windows a page opened, in its own tab: https://a.test/x, https://b.test/y" {
		t.Errorf("windowsLine without tabs = %q", l)
	}
	p := browsePrompt("steps", "", nil, false, nil)
	for _, w := range []string{"document.visibilityState", "tabs_create_mcp (a new tab opens in front)", "take no further screenshot in this turn",
		"name the page 'not rendered'", "close every tab you opened, the blank ones included", "the group's first tab last",
		windowCapture, windowRead, "flow=redirect&redirectUrl=", "through the person's SSO button", "wait 2 seconds and read it again"} {
		if !strings.Contains(p, w) {
			t.Errorf("the prompt lacks %q", w)
		}
	}
}

// A turn that left tabs it opened open fails, naming them in its group and
// outside it; a turn that closed them all says so.
func TestReadFindingsTabs(t *testing.T) {
	result := func(text string) string {
		quoted, _ := json.Marshal(text)
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":` + string(quoted) + `}]}}` + "\n"
	}
	made := result(`{"availableTabs":[{"tabId":11,"title":"New Tab","url":"chrome://newtab/"}],"tabGroupId":7}` + "\n\nTab Context:\n- Available tabs:\n  • tabId 11: \"New Tab\" (\"chrome://newtab/\")")
	fresh := result("Created new tab. Tab ID: 12\n\nTab Context:\n- Available tabs:\n  • tabId 11: \"A\" (\"https://a/\")\n  • tabId 12: \"New Tab\" (\"chrome://newtab/\")")
	stranded := result("Closed tab 11. 1 tab(s) remain.")
	again := result(`{"availableTabs":[{"tabId":13,"title":"New Tab","url":"chrome://newtab/"}],"tabGroupId":8}` + "\n\nTab Context:\n- Available tabs:\n  • tabId 13: \"New Tab\" (\"chrome://newtab/\")")
	closed := func(id string) string {
		return result("Closed tab " + id + ". Group is now empty (auto-removed). \n\nTab Context:\n- Available tabs:\n")
	}
	transcript := filepath.Join(t.TempDir(), "t.jsonl")

	writeFile(t, transcript, made+fresh+stranded+again+closed("13"))
	f, err := readFindings(transcript)
	if err != nil {
		t.Fatal(err)
	}
	lines := f.lines("steps", "", nil, false)
	if err := f.tabsErr(); err == nil || err.Error() != "the browse turn left 1 tab it opened open in the shared Chrome: tab 12 outside its group, out of any turn's reach" ||
		!slices.Contains(lines, "tabs left open: tab 12 outside its group, out of any turn's reach") {
		t.Errorf("a stranded tab: %v, %q", err, lines)
	}

	writeFile(t, transcript, made+fresh)
	f, _ = readFindings(transcript)
	if err := f.tabsErr(); err == nil || !strings.Contains(err.Error(), "left 2 tabs it opened open in the shared Chrome: tab 11, 12 in its group") {
		t.Errorf("two tabs in the group: %v", err)
	}

	writeFile(t, transcript, made+fresh+result("Closed tab 12. 1 tab(s) remain.")+closed("11"))
	f, _ = readFindings(transcript)
	if err := f.tabsErr(); err != nil || !slices.Contains(f.lines("steps", "", nil, false), "tabs closed: the turn closed all 2 tabs it opened") {
		t.Errorf("all closed: %v, %q", err, f.lines("steps", "", nil, false))
	}
}

// A turn whose steps name a Google OAuth client on record, by its callback
// host, runs with its auto mode rule, its prompt names the clicks of
// Google's sign-in for that callback, and the line below the report names
// the record.
func TestBrowseGoogleConsent(t *testing.T) {
	g := []guard.App{{Name: "example-portal", Google: true, Callbacks: []string{"portal.example.org"}, Account: "person@example.com", Declared: googleDeclared, Word: personsWord, By: agentOne}}
	consents := appsNamed(g, "Open https://portal.example.org/ and sign in with Google.")
	if len(consents) != 1 {
		t.Fatalf("appsNamed = %+v", consents)
	}
	if rules := browseRules("", consents, false); len(rules) != 1 || rules[0] != googleConsentRule(g[0]) ||
		!strings.Contains(rules[0], "Own Google App Consent") || !strings.Contains(rules[0], "callback host portal.example.org") ||
		!strings.Contains(rules[0], "choosing the account person@example.com") {
		t.Errorf("browseRules = %q", rules)
	}
	if p := browsePrompt("steps", "", consents, false, nil); !strings.Contains(p, "The person's own Google OAuth client example-portal is on record (callback portal.example.org)") ||
		!strings.Contains(p, "choose exactly the account person@example.com") || strings.Contains(p, "GitHub's page 'Authorize") {
		t.Errorf("browsePrompt = %q", p)
	}
	if l := (browseFindings{}).lines("steps", "", consents, false); len(l) != 1 ||
		!strings.HasPrefix(l[0], "allowed consent: example-portal (Google, callback portal.example.org, account person@example.com, "+googleDeclared+")") {
		t.Errorf("lines = %q", l)
	}
}

// A turn whose steps name a third-party consent on record, by its sign-in
// host, runs with its auto mode rule, its prompt names the Continue on that
// client's consent page and the device code on its device-code page for
// the account, and the line below the report names the record.
func TestBrowseThirdPartyConsent(t *testing.T) {
	o := []guard.App{{Name: "openai-codex", OAuth: "auth.openai.com", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Account: "person@example.com",
		Declared: oauthDeclared, Word: personsWord, By: agentOne}}
	if named := appsNamed(o, "Open https://example.org and sign in."); len(named) != 0 {
		t.Errorf("appsNamed of other steps = %+v", named)
	}
	consents := appsNamed(o, "Open https://auth.openai.com/codex/device and enter the code.")
	if len(consents) != 1 {
		t.Fatalf("appsNamed = %+v", consents)
	}
	if rules := browseRules("", consents, false); len(rules) != 1 || rules[0] != oauthConsentRule(o[0]) ||
		!strings.Contains(rules[0], "Third-Party OAuth Consent") || !strings.Contains(rules[0], "account person@example.com") ||
		!strings.Contains(rules[0], "auth.openai.com/sign-in-with-chatgpt/codex/consent") ||
		!strings.Contains(rules[0], "on its device-code page auth.openai.com/deviceauth/callback the device code typed") {
		t.Errorf("browseRules = %q", rules)
	}
	if p := browsePrompt("steps", "", consents, false, nil); !strings.Contains(p, "consent to OpenAI's Codex sign-in with ChatGPT is on record as their own account person@example.com") ||
		!strings.Contains(p, "On its device-code page (auth.openai.com/deviceauth/callback) type only the device code") ||
		strings.Contains(p, "GitHub's page 'Authorize") {
		t.Errorf("browsePrompt = %q", p)
	}
	if l := (browseFindings{}).lines("steps", "", consents, false); len(l) != 1 ||
		!strings.HasPrefix(l[0], "allowed consent: openai-codex (OpenAI's Codex sign-in with ChatGPT, client app_EMoamEEZ73f0CkXaXp7hrann, account person@example.com, "+oauthDeclared+")") ||
		!strings.Contains(l[0], "and the device code on auth.openai.com/deviceauth/callback") {
		t.Errorf("lines = %q", l)
	}
}

// A turn whose steps name a Slack app on record, by its app id or its
// workspace, runs with its auto mode rule, its prompt names Reinstall, the
// webhook channel to keep and Allow, and the line below the report names
// the record.
func TestBrowseSlackConsent(t *testing.T) {
	s := []guard.App{{Name: "Swarmgeist", Slack: "gigantic.slack.com", SlackApp: "A08A95JBNH1", ClientID: "2171500629.8349188396579",
		Channel: "team-bumblebee", Declared: slackDeclared, Word: personsWord, By: agentOne}}
	if named := appsNamed(s, "Open https://api.slack.com/apps/A0OTHER123 and look."); len(named) != 0 {
		t.Errorf("appsNamed of other steps = %+v", named)
	}
	consents := appsNamed(s, "Open https://api.slack.com/apps/A08A95JBNH1/install-on-team and reinstall the app.")
	if len(consents) != 1 {
		t.Fatalf("appsNamed = %+v", consents)
	}
	if rules := browseRules("", consents, false); len(rules) != 1 || rules[0] != slackConsentRule(s[0]) ||
		!strings.Contains(rules[0], "Own Slack App Install") || !strings.Contains(rules[0], "keeping the webhook channel #team-bumblebee") {
		t.Errorf("browseRules = %q", rules)
	}
	if p := browsePrompt("steps", "", consents, false, nil); !strings.Contains(p, "Slack app Swarmgeist (A08A95JBNH1) is on record for its install on gigantic.slack.com") ||
		!strings.Contains(p, "choose exactly #team-bumblebee") || !strings.Contains(p, "then click Allow once") {
		t.Errorf("browsePrompt = %q", p)
	}
	if l := (browseFindings{}).lines("steps", "", consents, false); len(l) != 1 ||
		!strings.HasPrefix(l[0], "allowed consent: Swarmgeist (Slack, app A08A95JBNH1 on gigantic.slack.com, client 2171500629.8349188396579, keeping the webhook channel #team-bumblebee, "+slackDeclared+")") {
		t.Errorf("lines = %q", l)
	}
}

// A draw check that answered not drawn counts, and the turn fails naming
// the window not drawn; a later check that drew, in a fresh tab, is the
// recovery the line names, and the turn does not fail on it.
func TestReadFindingsNotDrawn(t *testing.T) {
	code, _ := json.Marshal(guard.DrawCheck)
	use := func(id string) string {
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"mcp__claude-in-chrome__javascript_tool",` +
			`"input":{"action":"javascript_exec","tabId":1,"text":` + string(code) + `}}]}}`
	}
	answer := func(id, verdict string) string {
		text, _ := json.Marshal("beekeeper draw check: " + verdict + ", visibility visible\n\nTab Context:\n- Executed on tabId: 1")
		return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":[{"type":"text","text":` + string(text) + `}]}]}}`
	}
	// notDrawn are the "not drawn:" lines of f.
	notDrawn := func(f browseFindings) []string {
		return slices.DeleteFunc(f.lines("", "", nil, false), func(l string) bool { return !strings.HasPrefix(l, "not drawn: ") })
	}
	write := func(lines ...string) browseFindings {
		t.Helper()
		path := filepath.Join(t.TempDir(), "t.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := readFindings(path)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	stuck := write(use("a"), answer("a", guard.NotDrawn), use("b"), answer("b", guard.NotDrawn))
	if stuck.NotDrawn != 2 || stuck.Drawn {
		t.Fatalf("findings of two checks not drawn = %+v", stuck)
	}
	if err := stuck.drawErr(); err == nil || !strings.HasPrefix(err.Error(), guard.NotDrawnLine+": ") || !strings.Contains(err.Error(), "clicked nothing there") {
		t.Errorf("drawErr = %v", err)
	}
	if l := notDrawn(stuck); len(l) != 1 || !strings.HasPrefix(l[0], "not drawn: 2 draw checks answered not drawn: the window was not on screen") ||
		!strings.HasSuffix(l[0], "the turn clicked nothing on that page") {
		t.Errorf("lines = %q", l)
	}
	recovered := write(use("a"), answer("a", guard.NotDrawn), use("b"), answer("b", guard.Drawn))
	if recovered.NotDrawn != 1 || !recovered.Drawn || recovered.drawErr() != nil {
		t.Errorf("findings of a recovered draw = %+v, %v", recovered, recovered.drawErr())
	}
	if l := notDrawn(recovered); len(l) != 1 || !strings.Contains(l[0], "recovered: a later check, in a fresh tab, answered drawn") {
		t.Errorf("lines of a recovered draw = %q", l)
	}
	if drawn := write(use("a"), answer("a", guard.Drawn)); drawn.NotDrawn != 0 || drawn.drawErr() != nil || len(notDrawn(drawn)) != 0 {
		t.Errorf("findings of a drawn page = %+v", drawn)
	}
}

// A turn stopped at its timeout with a tab of its own open is resumed once
// by a cleanup turn (the same session, the cleanup prompt), which closes it:
// the command fails on the timeout, the lines say a cleanup ran and that the
// turn closed its tab. A stopped turn with no tab open runs no cleanup.
func TestBrowseCleansUpAfterTimeout(t *testing.T) {
	a, out := stubApp(t)
	bin := t.TempDir()
	argvFile, transcript, count := filepath.Join(bin, "argv"), filepath.Join(bin, "transcript"), filepath.Join(bin, "count")
	// The fake claude counts its runs, records each run's arguments, appends
	// that run's transcript under its session and, on its first run, waits
	// past the turn's timeout.
	script := "#!/bin/sh\nn=$(cat \"$BROWSE_COUNT\" 2>/dev/null || echo 0); n=$((n+1)); echo \"$n\" > \"$BROWSE_COUNT\"\n" +
		": > \"$BROWSE_ARGV.$n\"\nid=; prev=\nfor a in \"$@\"; do\n  printf '%s\\0' \"$a\" >> \"$BROWSE_ARGV.$n\"\n" +
		"  case \"$prev\" in --session-id|--resume) id=$a;; esac\n  prev=$a\ndone\n" +
		"mkdir -p \"$BROWSE_PROJECT\" && cat \"$BROWSE_TRANSCRIPT.$n\" >> \"$BROWSE_PROJECT/$id.jsonl\"\n" +
		"if [ \"$n\" = 1 ]; then exec sleep 5; fi\necho cleaned up\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake claude
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSE_ARGV", argvFile)
	t.Setenv("BROWSE_TRANSCRIPT", transcript)
	t.Setenv("BROWSE_COUNT", count)
	t.Setenv("BROWSE_PROJECT", filepath.Join(a.cfg.Claude.ProjectsDir, "p"))
	group := `{"availableTabs":[{"tabId":5,"title":"Portal","url":"https://portal.example.test/"}],"tabGroupId":9}` +
		"\n\nTab Context:\n- Available tabs:\n  • tabId 5: \"Portal\" (\"https://portal.example.test/\")"
	writeFile(t, transcript+".1", chromeUse("t1", "tabs_context_mcp", `{"createIfEmpty":true}`)+chromeAnswer("t1", group))
	writeFile(t, transcript+".2", chromeUse("t2", "tabs_close_mcp", `{"tabId":5}`)+
		chromeAnswer("t2", "Closed tab 5. Group is now empty (auto-removed). \n\nTab Context:\n- Available tabs:\n"))
	err := a.browse(t.Context(), "Open the portal.", bin, "", time.Second, "", nil, nil)
	if !errors.Is(err, errTurnStopped) || strings.Contains(err.Error(), "left") {
		t.Errorf("browse of a stopped turn = %v", err)
	}
	b, _ := os.ReadFile(count) //nolint:gosec // the test's own file
	if strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("the fake claude ran %s times, want 2\n%s", b, out.String())
	}
	second, _ := os.ReadFile(argvFile + ".2") //nolint:gosec // the test's own file
	args := strings.Split(strings.TrimSuffix(string(second), "\x00"), "\x00")
	if !slices.Contains(args, resumeFlag) || args[len(args)-1] != cleanupWords {
		t.Errorf("the cleanup ran with %q", args)
	}
	if s := out.String(); !strings.Contains(s, "timed out: the turn was stopped at its timeout") || !strings.Contains(s, "tabs closed: the turn closed all 1 tab it opened") {
		t.Errorf("browse of a stopped turn printed %q", s)
	}
	// No tab open: no cleanup.
	out.Reset()
	if err := os.Remove(count); err != nil {
		t.Fatal(err)
	}
	writeFile(t, transcript+".1", chromeUse("t1", "list_connected_browsers", `{}`)+chromeAnswer("t1", oneBrowser))
	if err := a.browse(t.Context(), "Open the portal.", bin, "", time.Second, "", nil, nil); !errors.Is(err, errTurnStopped) {
		t.Errorf("browse of a stopped turn without tabs = %v", err)
	}
	if b, _ := os.ReadFile(count); strings.TrimSpace(string(b)) != "1" { //nolint:gosec // the test's own file
		t.Errorf("the fake claude ran %s times without tabs, want 1", b)
	}
}
