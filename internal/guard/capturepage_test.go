package guard

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// pageKey is shaped like a console's API key; no key was ever in it.
const (
	pageKey     = "sk-ant-made-" + "upMadeUpMadeUpMadeUpMadeUpMadeUpMadeUpMadeUpMadeUpMadeUpMadeUpMadeUp0000"
	pageMatch   = `sk-ant-[a-z0-9]+-[A-Za-z0-9_-]{60,}`
	captureNote = "beekeeper took the value"
	deniedCall  = `"permissionDecision":"deny"`
)

// captureEvent is a capture turn's hook event of a Chrome call, its input
// and response as JSON.
func captureEvent(t *testing.T, event, tool, input, response string) []byte {
	t.Helper()
	ev := struct {
		Event    string          `json:"hook_event_name"`
		Tool     string          `json:"tool_name"`
		Input    json.RawMessage `json:"tool_input"`
		Response json.RawMessage `json:"tool_response,omitempty"`
	}{event, browserTools + tool, json.RawMessage(input), nil}
	if response != "" {
		ev.Response = json.RawMessage(response)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// quoted is s as a JSON string.
func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A capture turn sees no pixels and no page text beyond find's: the hook
// refuses a screenshot, a zoom, a page read, a batch and every script but
// the reader; a click, a find and the reader itself pass.
func TestCapturePageRefusesReads(t *testing.T) {
	c := Capture{Pattern: regexp.MustCompile(pageMatch)}
	for _, call := range []struct {
		tool, input string
		deny        bool
	}{
		{chromeComputer, `{"action":"screenshot"}`, true},
		{chromeComputer, `{"action":"zoom","region":[0,0,9,9]}`, true},
		{"get_page_text", `{}`, true},
		{"read_page", `{"filter":"interactive"}`, true},
		{"read_network_requests", `{}`, true},
		{chromeBatch, `{"actions":[{"name":"computer","input":{"action":"screenshot"}}]}`, true},
		{chromeScript, `{"text":"document.body.innerText"}`, true},
		{chromeScript, `{"text":` + quoted(" "+ReaderScript(pageMatch)+"\n") + `}`, false},
		{chromeComputer, `{"action":"left_click","ref":"ref_3"}`, false},
		{"find", `{"query":"Create Key button"}`, false},
		{chromeNavigation, `{"url":"https://console.example/"}`, false},
	} {
		out := CapturePage(captureEvent(t, PreToolUseEvent, call.tool, call.input, ""), c)
		if denied := strings.Contains(string(out), deniedCall); denied != call.deny {
			t.Errorf("%s %s: answer %s, want deny %v", call.tool, call.input, out, call.deny)
		}
	}
	// a call of no Chrome tool is none of the hook's business
	if out := CapturePage([]byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`), c); out != nil {
		t.Errorf("a Bash call got %s", out)
	}
}

// After a call the hook hands the one match to beekeeper and answers the
// result with the match masked, every long token masked and every image
// dropped; several different matches deliver none.
func TestCapturePageDeliversAndMasks(t *testing.T) {
	var got []string
	c := Capture{Pattern: regexp.MustCompile(pageMatch), Deliver: func(v string) error { got = append(got, v); return nil }}
	resp := `[{"type":"text","text":` + quoted(`{"beekeeperCapture":1} ["`+pageKey+`"]`) + `},` +
		`{"type":"image","source":{"type":"base64","data":"iVBORw0KGgo="}}]`
	out := CapturePage(captureEvent(t, PostToolUseEvent, chromeScript, `{}`, resp), c)
	if len(got) != 1 || got[0] != pageKey {
		t.Fatalf("delivered %q", got)
	}
	s := string(out)
	if strings.Contains(s, pageKey) || strings.Contains(s, "iVBORw0KGgo") || !strings.Contains(s, CaptureMask) ||
		!strings.Contains(s, `"updatedToolOutput"`) || !strings.Contains(s, captureNote) {
		t.Errorf("the answer: %s", s)
	}

	// a find whose result shows a token of another shape: masked, nothing
	// delivered
	got = nil
	const hex = "0123456789abcdef0123456789abcdef01234567"
	out = CapturePage(captureEvent(t, PostToolUseEvent, "find", `{}`, quoted("ref_7 code '"+hex+"'")), c)
	if len(got) != 0 || strings.Contains(string(out), hex) || !strings.Contains(string(out), CaptureMask) {
		t.Errorf("a token run: delivered %q, answer %s", got, out)
	}

	// two different keys on the page: none taken
	other := strings.Replace(pageKey, "0000", "1111", 1)
	out = CapturePage(captureEvent(t, PostToolUseEvent, chromeScript, `{}`, quoted(`["`+pageKey+`","`+other+`"]`)), c)
	if len(got) != 0 || !strings.Contains(string(out), "2 different values") || strings.Contains(string(out), pageKey) {
		t.Errorf("two values: delivered %q, answer %s", got, out)
	}

	// a delivery that fails says so, the value still masked
	c.Deliver = func(string) error { return errors.New("not listening") }
	out = CapturePage(captureEvent(t, PostToolUseEvent, chromeScript, `{}`, quoted(pageKey)), c)
	if !strings.Contains(string(out), "did not take the value: not listening") || strings.Contains(string(out), pageKey) {
		t.Errorf("a failed delivery: %s", out)
	}

	// a plain result with nothing to mask gets no answer
	if out := CapturePage(captureEvent(t, PostToolUseEvent, chromeComputer, `{}`, quoted("Clicked on element ref_3")), c); out != nil {
		t.Errorf("a plain click got %s", out)
	}
}
