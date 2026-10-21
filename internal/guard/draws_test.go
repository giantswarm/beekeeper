package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// drawsTranscript writes a transcript of Chrome calls and their results:
// each pair is a call (tool after the server's prefix, its input) and the
// text of its result, which ends with the tab context of tab 7 at url.
type drawsCall struct {
	tool  string
	input map[string]any
	text  string
	url   string
}

func drawsTranscript(t *testing.T, calls ...drawsCall) string {
	t.Helper()
	var lines []string
	for i, c := range calls {
		id := "c" + strconv.Itoa(i)
		in, err := json.Marshal(c.input)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"`+id+`","name":"mcp__claude-in-chrome__`+c.tool+`","input":`+string(in)+`}]}}`)
		text := c.text + "\n\nTab Context:\n- Executed on tabId: 7\n- Available tabs:\n  • tabId 7: \"Page\" (\"" + c.url + "\")"
		lines = append(lines, resultLine(`[{"type":"text","text":`+strconv.Quote(text)+`}]`))
		lines[len(lines)-1] = strings.Replace(lines[len(lines)-1], `"tool_use_id":"t"`, `"tool_use_id":"`+id+`"`, 1)
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// scriptExec is javascript_tool's action.
const scriptExec = "javascript_exec"

// The GitLab of the draw tests and the key its consent is pressed with.
const (
	drawGitLab   = "gitlab.example.org"
	drawCallback = "muster.example.org"
	drawApp      = "workspace-manager"
	keyEnter     = "Enter"
)

// script is a javascript_tool call of code on tab 7.
func script(code string) map[string]any {
	return map[string]any{actionKey: scriptExec, tabKey: 7.0, textKey: code}
}

// drawResult is DrawCheck's result with verdict on tab 7 at url.
func drawResult(verdict, url string) drawsCall {
	return drawsCall{chromeScript, script(DrawCheck), drawOpen + verdict + ", visibility visible", url}
}

// The draw check's verdict holds for the tab at the URL it ran at: a later
// URL forgets it, a later check replaces it; another script's words naming a
// verdict, or the check in a batch, count for nothing. The leave-page disarm
// counts alone or in a batch.
func TestPageChecks(t *testing.T) {
	const page, later = "https://gitlab.example.org/oauth/authorize?x=1", "https://muster.example.org/callback"
	nav := drawsCall{chromeNavigation, map[string]any{urlKey: later, tabKey: 7}, "Navigated", later}
	for name, tc := range map[string]struct {
		calls []drawsCall
		want  pageCheck
	}{
		"drawn":                     {[]drawsCall{drawResult(Drawn, page)}, pageCheck{url: page, drawn: Drawn}},
		"not drawn":                 {[]drawsCall{drawResult(NotDrawn, page)}, pageCheck{url: page, drawn: NotDrawn}},
		"not drawn, then drawn":     {[]drawsCall{drawResult(NotDrawn, page), drawResult(Drawn, page)}, pageCheck{url: page, drawn: Drawn}},
		"drawn, then another page":  {[]drawsCall{drawResult(Drawn, page), nav}, pageCheck{url: later}},
		"another script's words":    {[]drawsCall{{chromeScript, script("'" + drawOpen + Drawn + "'"), drawOpen + Drawn, page}}, pageCheck{}},
		"the check in a batch":      {[]drawsCall{{chromeBatch, map[string]any{actionsKey: []any{batchAction(chromeScript, script(DrawCheck))}}, drawOpen + Drawn, page}}, pageCheck{}},
		"disarmed":                  {[]drawsCall{{chromeScript, script(LeaveDisarm), leaveAnswer, page}}, pageCheck{url: page, left: true}},
		"disarmed in a batch":       {[]drawsCall{{chromeBatch, map[string]any{actionsKey: []any{batchAction(chromeScript, script(LeaveDisarm))}}, leaveAnswer, page}}, pageCheck{url: page, left: true}},
		"disarmed, then a new page": {[]drawsCall{{chromeScript, script(LeaveDisarm), leaveAnswer, page}, nav}, pageCheck{url: later}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pageChecks(drawsTranscript(t, tc.calls...))[tab7]; got != tc.want {
				t.Errorf("pageChecks = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A click or key the record allows on a GitLab consent page passes only
// once a draw check at that page answered drawn: without one the hook names
// the check; not drawn, it names the fresh tab and the report's line; drawn
// at the earlier URL counts for nothing. The draw check between the form
// read and the click keeps the form read. A refused click stays refused,
// drawn or not; a form value needs no draw.
func TestDrawRefusal(t *testing.T) {
	apps := func() []App {
		return []App{{Name: drawApp, ClientID: gitlabUID, Callbacks: []string{drawCallback}, GitLab: drawGitLab, Declared: "the manifest"}}
	}
	authorize := "https://" + drawGitLab + "/oauth/authorize?client_id=REDACTED&redirect_uri=https%3A%2F%2Fmuster.example.org%2Foauth%2Fproxy%2Fcallback&response_type=code&state=REDACTED"
	form := drawsCall{chromeScript, script(GitLabFormScript), gitlabFormOpen + "application " + gitlabUID, authorize}
	otherForm := drawsCall{chromeScript, script(GitLabFormScript), gitlabFormOpen + "application uid-2", authorize}
	click := tabInput(leftClick, map[string]any{refKey: "ref_7", summaryKey: "Clicks GitLab's 'Authorize workspace-manager' button"})
	enter := tabInput(actionKeyPress, map[string]any{textKey: keyEnter, summaryKey: "Presses Enter on 'Authorize workspace-manager'"})
	const allowed = "own GitLab application workspace-manager"
	for name, tc := range map[string]struct {
		calls    []drawsCall
		tool     string
		input    map[string]any
		decision string
		reason   string
	}{
		"no draw check":                {[]drawsCall{form}, chromeComputer, click, decisionDeny, DrawCheck},
		"drawn, then the form read":    {[]drawsCall{drawResult(Drawn, authorize), form}, chromeComputer, click, decisionAllow, allowed},
		"the form read, then drawn":    {[]drawsCall{form, drawResult(Drawn, authorize)}, chromeComputer, click, decisionAllow, allowed},
		"Enter on a drawn page":        {[]drawsCall{drawResult(Drawn, authorize), form}, chromeComputer, enter, decisionAllow, allowed},
		"not drawn":                    {[]drawsCall{drawResult(NotDrawn, authorize), form}, chromeComputer, click, decisionDeny, NotDrawnLine},
		"Enter on a page not drawn":    {[]drawsCall{drawResult(NotDrawn, authorize), form}, chromeComputer, enter, decisionDeny, "tabs_create_mcp"},
		"drawn at the earlier URL":     {[]drawsCall{drawResult(Drawn, "https://"+drawGitLab+"/users/sign_in"), form}, chromeComputer, click, decisionDeny, DrawCheck},
		"another application, drawn":   {[]drawsCall{drawResult(Drawn, authorize), otherForm}, chromeComputer, click, decisionDeny, "client id uid-2"},
		"the click in a batch":         {[]drawsCall{form}, chromeBatch, map[string]any{actionsKey: []any{batchAction(chromeComputer, click)}}, decisionDeny, DrawCheck},
		"a form value, no draw needed": {[]drawsCall{form}, chromeForm, map[string]any{valueKey: "x", tabKey: json.Number(tab7)}, decisionAllow, allowed},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+tc.tool, tc.input)
			ev["permission_mode"] = dontAsk
			ev[sessionKey] = browseTurn
			ev["transcript_path"] = drawsTranscript(t, tc.calls...)
			d := decideEvent(t, Hook{Apps: apps}, ev)
			if d == nil || d.PermissionDecision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("decided %+v, want %s with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// The close of a tab on a web page passes once the leave-page disarm ran
// on it at that page, before the close in the same batch or in an earlier
// call; a tab on a blank or browser page closes as it is; a close without
// the disarm, or with it after the close, is refused naming it.
func TestLeaveRefusal(t *testing.T) {
	const portal = "https://backstage.example.org/workspaces/new"
	onPortal := drawsCall{chromeComputer, tabInput("wait", nil), "Waited", portal}
	closeTab := map[string]any{tabKey: 7.0}
	disarm := script(LeaveDisarm)
	for name, tc := range map[string]struct {
		calls  []drawsCall
		tool   string
		input  map[string]any
		refuse bool
	}{
		"a web page, no disarm":        {[]drawsCall{onPortal}, chromeTabsClose, closeTab, true},
		"disarmed before":              {[]drawsCall{onPortal, {chromeScript, disarm, leaveAnswer, portal}}, chromeTabsClose, closeTab, false},
		"disarmed at the earlier page": {[]drawsCall{{chromeScript, disarm, leaveAnswer, "https://example.org/"}, onPortal}, chromeTabsClose, closeTab, true},
		"disarm, then close, a batch":  {[]drawsCall{onPortal}, chromeBatch, map[string]any{actionsKey: []any{batchAction(chromeScript, disarm), batchAction(chromeTabsClose, closeTab)}}, false},
		"close, then disarm, a batch":  {[]drawsCall{onPortal}, chromeBatch, map[string]any{actionsKey: []any{batchAction(chromeTabsClose, closeTab), batchAction(chromeScript, disarm)}}, true},
		"a blank tab":                  {[]drawsCall{{chromeComputer, tabInput("wait", nil), "Waited", "chrome://newtab/"}}, chromeTabsClose, closeTab, false},
		"a tab the transcript lacks":   {[]drawsCall{onPortal}, chromeTabsClose, map[string]any{tabKey: 8.0}, false},
		"another call":                 {[]drawsCall{onPortal}, chromeNavigation, map[string]any{urlKey: portal, tabKey: 7.0}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := leaveRefusal(event{ToolName: browserTools + tc.tool, ToolInput: tc.input, TranscriptPath: drawsTranscript(t, tc.calls...)})
			if tc.refuse != (r != "") || (r != "" && (!strings.Contains(r, LeaveDisarm) || !strings.Contains(r, "backstage.example.org"))) {
				t.Errorf("leaveRefusal = %q, want refused %v", r, tc.refuse)
			}
		})
	}
	// The hook decides it.
	path, _ := json.Marshal(drawsTranscript(t, onPortal))
	in := []byte(`{"tool_name":"` + tabsCloseTool + `","tool_input":{"tabId":7},"transcript_path":` + string(path) + `}`)
	if out := (Hook{}).Decide(in); !strings.Contains(string(out), `"permissionDecision":"deny"`) {
		t.Errorf("the hook answered %s", out)
	}
}

// The Stop hook's words name the leave-page disarm; a capture turn runs the
// draw check and the disarm beside its reader, and no other script.
func TestDrawAndLeaveScriptsAllowed(t *testing.T) {
	in, _ := json.Marshal(map[string]any{"transcript_path": tabsTranscript(t, groupMade, freshTab)})
	var m map[string]string
	if err := json.Unmarshal(BrowseStop(in), &m); err != nil || !strings.Contains(m["reason"], LeaveDisarm) {
		t.Errorf("Stop = %v, %v", m, err)
	}
	c := Capture{Pattern: regexp.MustCompile(pageMatch)}
	for code, refused := range map[string]bool{DrawCheck: false, LeaveDisarm: false, "document.body.innerText": true} {
		input, _ := json.Marshal(script(code))
		if r := c.refusal(browserTools+chromeScript, input); refused != (r != "") {
			t.Errorf("capture refusal of %.30q = %q", code, r)
		}
	}
}
