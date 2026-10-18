package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// BlindReads are the Claude in Chrome tools a capture turn goes without:
// each answers the page's text, its traffic, its pixels or a file, where a
// value the page shows once would reach the model, or (a batch) runs such
// a call past the hook. The turn finds its controls with find, clicks them
// by ref, and runs one script, the reader.
var BlindReads = []string{
	"get_page_text", "read_page", "read_network_requests", "read_console_messages",
	"gif_creator", "upload_image", "file_upload", "shortcuts_execute", chromeBatch,
}

// captureTools are the Chrome tools a capture turn may call, each by its
// name after the server's prefix; every other call is refused. computer
// passes without its picture actions and its copy and paste keys, navigate
// with an http or https URL alone, javascript_tool with the reader alone.
var captureTools = []string{
	"find", chromeComputer, chromeNavigation, chromeForm, chromeScript, "resize_window",
	"tabs_context_mcp", "tabs_create_mcp", "tabs_close_mcp", "list_connected_browsers", "select_browser", "switch_browser",
}

// clipboardKey is a key press that copies, cuts, pastes or selects all.
var clipboardKey = regexp.MustCompile(`(?i)\b(?:ctrl|cmd|meta|super)\+(?:shift\+)?[acvx]\b`)

// tokenRun is a run of token characters long enough to be a piece of a
// credential (an API key, a client secret, a private key's base64 line): a
// capture turn's results carry each as CaptureMask.
var tokenRun = regexp.MustCompile(`[A-Za-z0-9_+=-]{16,}`)

// CaptureMask stands for a token run, or the captured value, in a capture
// turn's result and transcript.
const CaptureMask = "[masked by beekeeper secret page]"

// ReaderScript is the one script a capture turn runs (javascript_tool): it
// answers the distinct matches of pattern in the page's text and in its
// fields' values, as JSON. Its result reaches beekeeper's hook, which takes
// the value and hands the model the result masked.
func ReaderScript(pattern string) string {
	p, _ := json.Marshal(pattern)
	return "(() => { const re = new RegExp(" + string(p) + ", 'g'); const seen = new Set(); " +
		"const texts = [document.body ? document.body.innerText : '']; " +
		"document.querySelectorAll('input, textarea').forEach(e => texts.push(e.value || '')); " +
		"texts.forEach(t => (t.match(re) || []).forEach(m => seen.add(m))); " +
		"return JSON.stringify({beekeeperCapture: [...seen].length}) + ' ' + JSON.stringify([...seen]); })()"
}

// Capture is what a capture turn's hook knows: the pattern of the value,
// the reader script built from it, and where the one value goes.
type Capture struct {
	Pattern *regexp.Regexp
	// Deliver hands the value to the waiting beekeeper secret page.
	Deliver func(value string) error
}

// CapturePage is a capture turn's hook (beekeeper hook capturepage, in the
// turn's --settings alone). On PreToolUse it refuses every Chrome call but
// those captureTools allows: no screenshot, zoom, page read or batch, no
// copy or paste key, no navigate but to an http or https page, no script
// but the reader. On PostToolUse it delivers the one distinct match of the
// pattern a result carries, and answers every result with images dropped,
// every match of the pattern and every token run masked, so the value
// reaches the model neither as pixels nor as text. A result with several
// distinct matches delivers none: its answer says the page is ambiguous.
// Every other event, and a result with nothing to drop, gets no answer.
func CapturePage(input []byte, c Capture) []byte {
	var ev struct {
		Event    string          `json:"hook_event_name"`
		Tool     string          `json:"tool_name"`
		Input    json.RawMessage `json:"tool_input"`
		Response json.RawMessage `json:"tool_response"`
	}
	if json.Unmarshal(input, &ev) != nil || !isBrowserTool(ev.Tool) {
		return nil
	}
	switch ev.Event {
	case PreToolUseEvent:
		if why := c.refusal(ev.Tool, ev.Input); why != "" {
			return hookJSON(struct {
				Out hookOutput `json:"hookSpecificOutput"`
			}{hookOutput{HookEventName: PreToolUseEvent, PermissionDecision: decisionDeny, Reason: why}})
		}
	case PostToolUseEvent:
		resp, err := decodeJSON(ev.Response)
		if err != nil {
			return nil
		}
		found := c.matches(resp)
		blinded, changed := c.blind(resp)
		note := ""
		switch {
		case len(found) == 1 && c.Deliver != nil:
			if err := c.Deliver(found[0]); err != nil {
				note = "beekeeper did not take the value: " + err.Error()
			} else {
				note = "beekeeper took the value (" + fmt.Sprint(len(found[0])) + " characters) for the vault; you never see it."
			}
		case len(found) > 1:
			note = fmt.Sprintf("the page shows %d different values of the pattern: beekeeper took none.", len(found))
		}
		if !changed && note == "" {
			return nil
		}
		type output struct {
			Event   string `json:"hookEventName"`
			Output  any    `json:"updatedToolOutput"`
			Context string `json:"additionalContext,omitempty"`
		}
		return hookJSON(struct {
			Out output `json:"hookSpecificOutput"`
		}{output{Event: PostToolUseEvent, Output: blinded, Context: note}})
	}
	return nil
}

// refusal returns why a capture turn's Chrome call is refused, "" when it
// passes: an allow list, every call it does not name refused.
func (c Capture) refusal(tool string, input json.RawMessage) string {
	name := strings.ToLower(tool[strings.LastIndex(tool, "__")+2:])
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { v, _ := in[k].(string); return v }
	const blind = "Refused: this turn captures a value the page shows once, which no session sees: "
	switch {
	case !slices.Contains(captureTools, name), takesPicture(tool, in):
		return blind + "no screenshot, zoom, page read or batch. Find each control with find (by its words, never the value) and click it by its ref."
	case name == chromeComputer && clipboardKey.MatchString(str(textKey)):
		return blind + "no copy, cut, paste or select-all key."
	case name == chromeNavigation && !strings.HasPrefix(str(urlKey), "https://") && !strings.HasPrefix(str(urlKey), "http://") &&
		str(urlKey) != "back" && str(urlKey) != "forward":
		return blind + "navigate opens an http or https page only."
	case name == chromeScript && strings.TrimSpace(str(textKey)) != ReaderScript(c.Pattern.String()):
		return blind + "the one script it runs is beekeeper's reader, verbatim: " + ReaderScript(c.Pattern.String())
	}
	return ""
}

// matches are the distinct matches of the pattern in v's strings.
func (c Capture) matches(v any) []string {
	var found []string
	walkStrings(v, func(s string) string {
		for _, m := range c.Pattern.FindAllString(s, -1) {
			if !slices.Contains(found, m) {
				found = append(found, m)
			}
		}
		return s
	})
	return found
}

// blind is v without its image blocks, every match of the pattern and every
// token run masked, and whether anything changed.
func (c Capture) blind(v any) (any, bool) {
	changed := false
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			out := tokenRun.ReplaceAllString(c.Pattern.ReplaceAllString(t, CaptureMask), CaptureMask)
			changed = changed || out != t
			return out
		case []any:
			kept := t[:0]
			for _, x := range t {
				if m, ok := x.(map[string]any); ok && m["type"] == "image" {
					changed = true
					continue
				}
				kept = append(kept, walk(x))
			}
			return kept
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return v
	}
	return walk(v), changed
}

// hookJSON is a hook's answer, nil when it does not encode.
func hookJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil
	}
	return b.Bytes()
}
