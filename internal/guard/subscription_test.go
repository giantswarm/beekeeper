package guard

import (
	"os"
	"strings"
	"testing"
)

// browseTurn is the session of the browse turn the guard tests decide in.
const browseTurn = "browse-turn"

// The page read of an issue the subscription tests click on: the edit
// history control, the thread's Subscribe button, and a ref read again as
// another element.
const issueRead = `- heading "browse: a scripted click" [ref_3]
- button "edited" [ref_12]
- button "Subscribe" [ref_45]
- button "Unwatch" [ref_51]
- button "Notifications" [ref_7]`

// A Chrome call that clicks a subscription, watch, star or notification
// control is refused in any session, the control named: a click whose
// summary names one, a click whose ref the page read names one, a form value
// on one, a batch with such a click, a scripted click whose code names one,
// and a scripted click that sweeps the page's buttons for its element. A
// click on the element the step names passes, and so does a typed text, a
// read and a script that clicks nothing.
func TestSubscriptionRefusal(t *testing.T) {
	transcript := drawsTranscript(t,
		drawsCall{chromeReadPage, map[string]any{tabKey: 7}, strings.ReplaceAll(issueRead, "Notifications", "Customize"), ""},
		drawsCall{chromeFind, map[string]any{tabKey: 7}, issueRead, ""},
	)
	click := func(ref, summary string) map[string]any {
		return tabInput(leftClick, map[string]any{refKey: ref, summaryKey: summary})
	}
	script := func(code string) map[string]any {
		return map[string]any{actionKey: "javascript_exec", textKey: code, tabKey: tab7}
	}
	for name, tc := range map[string]struct {
		tool   string
		input  map[string]any
		reason string
	}{
		"Subscribe by its summary":       {chromeComputer, click("", "Clicks the Subscribe button"), `the click's summary names ` + SubscriptionControls + ` ("Subscribe")`},
		"Unsubscribe by its summary":     {chromeComputer, tabInput("double_click", map[string]any{summaryKey: "Unsubscribes from the thread"}), `("Unsubscribes")`},
		"Watch by its summary":           {chromeComputer, click("", "Opens the Watch menu"), `("Watch")`},
		"Star by its summary":            {chromeComputer, click("", "Stars the repository"), `("Stars")`},
		"a ref the page read names":      {chromeComputer, click("ref_45", "Clicks the edited control"), `the click's ref_45 is ` + SubscriptionControls + ` ("Subscribe")`},
		"the last read of a ref":         {chromeComputer, click("ref_7", "Clicks the button"), `("Notifications")`},
		"a ref without a summary":        {chromeComputer, tabInput(leftClick, map[string]any{refKey: "ref_51"}), `("Unwatch")`},
		"a form value on a subscription": {chromeForm, map[string]any{refKey: "ref_45", valueKey: true, tabKey: tab7}, `("Subscribe")`},
		"a batch with the click":         {chromeBatch, map[string]any{actionsKey: []any{batchAction(chromeComputer, tabInput("screenshot", nil)), batchAction(chromeComputer, click("ref_45", ""))}}, `("Subscribe")`},
		"a scripted click naming one":    {chromeScript, script(`document.querySelector('button[aria-label="Subscribe"]').click()`), `the scripted click's code names ` + SubscriptionControls},
		"a scripted click on a sweep":    {chromeScript, script(`[...document.querySelectorAll('button')].find(b => b.textContent.includes('edited')).click()`), "a sweep over the page's buttons or links (querySelectorAll('button'))"},
		"a sweep by tag name":            {chromeScript, script(`for (const b of document.getElementsByTagName("button")) if (b.innerText === 'edited') b.click()`), "a sweep over the page's buttons"},
		"a sweep of buttons and links":   {chromeScript, script("document.querySelectorAll(`button, [role=\"button\"]`)[3].dispatchEvent(new MouseEvent('click'))"), "a sweep"},
		"the edit history by its ref":    {chromeComputer, click("ref_12", "Clicks the edited control of the issue body"), ""},
		"a click by coordinate":          {chromeComputer, tabInput(leftClick, map[string]any{"coordinate": []any{10, 20}, summaryKey: "Clicks Edit"}), ""},
		"Start is no star":               {chromeComputer, click("", "Clicks Start review"), ""},
		"a typed text naming one":        {chromeComputer, tabInput("type", map[string]any{textKey: "watch the subscription", summaryKey: "Types the comment"}), ""},
		"a screenshot":                   {chromeComputer, tabInput("screenshot", nil), ""},
		"a scoped scripted click":        {chromeScript, script(`document.querySelector('[data-testid="record-demo"] button').click()`), ""},
		"a script that only reads":       {chromeScript, script(`[...document.querySelectorAll('button')].map(b => b.textContent + ' subscribe?')`), ""},
		"a read of the notifications":    {"get_page_text", map[string]any{tabKey: tab7}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+tc.tool, tc.input)
			ev[sessionKey] = browseTurn
			ev["transcript_path"] = transcript
			d := decideEvent(t, Hook{}, ev)
			switch {
			case tc.reason == "":
				if d != nil && d.PermissionDecision != "" {
					t.Errorf("decided %+v, want no decision", d)
				}
			case d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, tc.reason) ||
				!strings.Contains(d.Reason, "Click only the element the step names"):
				t.Errorf("decided %+v, want a refusal with %q", d, tc.reason)
			}
		})
	}
	// The desktop's spelling of the tools is refused the same way.
	ev := toolEvent("mcp__Claude_in_Chrome__"+chromeComputer, click("", "Clicks Subscribe"))
	if d := decideEvent(t, Hook{}, ev); d == nil || d.PermissionDecision != decisionDeny {
		t.Errorf("the desktop's spelling decided %+v, want a refusal", d)
	}
}

func TestAuditSubscriptionIgnoresUnrelatedResults(t *testing.T) {
	read := func(tool, tab, label string) turnCall {
		return turnCall{tool, map[string]any{tabKey: tab}, `- button "` + label + `" [ref_45]`}
	}
	subscribe := read(browserTools+chromeReadPage, "7", "Subscribe")
	ordinary := read(browserTools+chromeReadPage, "7", "Continue")
	for name, tc := range map[string]struct {
		calls []turnCall
		deny  bool
	}{
		"subscription baseline":                            {[]turnCall{subscribe}, true},
		"shell cannot replace subscription":                {[]turnCall{subscribe, read("Bash", "7", "Continue")}, true},
		"other browser result cannot replace subscription": {[]turnCall{subscribe, read(browserTools+chromeScript, "7", "Continue")}, true},
		"other tab cannot replace subscription":            {[]turnCall{subscribe, read(browserTools+chromeFind, "8", "Continue")}, true},
		"other tab cannot block ordinary control":          {[]turnCall{ordinary, read(browserTools+chromeReadPage, "8", "Subscribe")}, false},
		"shell cannot block ordinary control":              {[]turnCall{ordinary, read("Bash", "7", "Subscribe")}, false},
		"same tab can update label":                        {[]turnCall{subscribe, read(browserTools+chromeFind, "7", "Continue")}, false},
		"same tab can identify subscription":               {[]turnCall{ordinary, read(browserTools+chromeFind, "7", "Notifications")}, true},
		"unmatched result is ignored":                      {nil, false},
		"tabless read is ignored":                          {[]turnCall{read(browserTools+chromeReadPage, "", "Subscribe")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			transcript := turnTranscript(t, tc.calls...)
			if name == "unmatched result is ignored" {
				// A result without a matching tool call cannot supply a label.
				f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(resultLine(`"- button \"Subscribe\" [ref_45]"`) + "\n")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			click := tabInput(leftClick, map[string]any{refKey: "ref_45"})
			for _, batch := range []bool{false, true} {
				tool, input := browserTools+chromeComputer, click
				if batch {
					tool = browserTools + chromeBatch
					input = map[string]any{actionsKey: []any{batchAction(chromeComputer, tabInput(leftClick, map[string]any{refKey: "ref_99", tabKey: "8"})), batchAction(chromeComputer, click)}}
				}
				reason := subscriptionRefusal(event{ToolName: tool, ToolInput: input, TranscriptPath: transcript})
				// Each action uses its own tab when resolving refs.
				want := tc.deny
				if (reason != "") != want {
					t.Errorf("batch=%v: refusal %q, want deny=%v", batch, reason, want)
				}
			}
		})
	}
}
