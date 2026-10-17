package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	uploadTool = "mcp__claude-in-chrome__file_upload"
	scriptTool = "mcp__claude-in-chrome__javascript_tool"
)

// A turn is the calls and results of a transcript: a call with its tool,
// input and result text.
type turnCall struct {
	tool   string
	input  map[string]any
	result string
}

// turnTranscript writes a transcript of the calls, each a tool_use line and
// its tool_result line.
func turnTranscript(t *testing.T, calls ...turnCall) string {
	t.Helper()
	var lines []string
	for i, c := range calls {
		id := "toolu_" + string(rune('a'+i))
		use, err := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": c.tool, "input": c.input}}}})
		if err != nil {
			t.Fatal(err)
		}
		res, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": []any{map[string]any{"type": "text", "text": c.result}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(use), string(res))
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The calls of an upload: the check, its result, the find and its refs.
var (
	checkCall = func(result string) turnCall {
		return turnCall{scriptTool, map[string]any{textKey: UploadTargetCheck("#new_comment_form"), tabKey: 7}, result}
	}
	marked   = checkCall(UploadTargetLabel + ": marked input#fc-new_comment_field in form#new_comment_form (2 other file inputs hidden or disabled); find")
	unmarked = checkCall(UploadTargetLabel + ": none: 2 file inputs, every one hidden or disabled (input in form#review-reply)")
	findCall = turnCall{"mcp__claude-in-chrome__find", map[string]any{"query": UploadTargetLabel},
		"Found 1 matching element\n- ref_42: input \"" + UploadTargetLabel + "\" (file)\n"}
	readPage = turnCall{"mcp__claude-in-chrome__read_page", map[string]any{},
		"- ref_9: input (file) in a hidden review reply\n- ref_42: input \"" + UploadTargetLabel + "\"\n"}
	// fake is a script of the turn's own that prints the check's words.
	fake = turnCall{scriptTool, map[string]any{textKey: "'" + UploadTargetLabel + ": marked x'"}, UploadTargetLabel + ": marked x"}
)

// A file_upload passes only to the ref the last upload-target check marked
// and its find gave; one before any check, after a check that marked none,
// without a find, to another ref, or after the turn's own imitation of the
// check is refused; a batch's file_upload is decided the same way.
func TestUploadRefusal(t *testing.T) {
	upload := func(ref string) map[string]any {
		return map[string]any{refKey: ref, tabKey: 7, "paths": []any{"/x/shot.png"}}
	}
	cases := map[string]struct {
		calls  []turnCall
		tool   string
		input  map[string]any
		refuse string
	}{
		"the marked ref":                {[]turnCall{marked, findCall}, uploadTool, upload("ref_42"), ""},
		"the marked ref of a page read": {[]turnCall{marked, readPage}, uploadTool, upload("ref_42"), ""},
		"a hidden input's ref":          {[]turnCall{marked, readPage}, uploadTool, upload("ref_9"), "ref_9 is not the input the last upload-target check marked"},
		"no check":                      {[]turnCall{findCall}, uploadTool, upload("ref_42"), "comes before any upload-target check"},
		"a check that marked none":      {[]turnCall{marked, findCall, unmarked}, uploadTool, upload("ref_42"), "marked no input"},
		"no find after the check":       {[]turnCall{findCall, marked}, uploadTool, upload("ref_42"), "no find for"},
		"the turn's own imitation":      {[]turnCall{fake, findCall}, uploadTool, upload("ref_42"), "before any upload-target check"},
		"no ref":                        {[]turnCall{marked, findCall}, uploadTool, map[string]any{tabKey: 7}, "no ref is not the input"},
		"a batch's upload": {[]turnCall{marked, findCall}, "mcp__claude-in-chrome__browser_batch",
			map[string]any{actionsKey: []any{map[string]any{nameKey: "file_upload", inputKey: upload("ref_9")}}}, "ref_9 is not"},
		"no upload": {nil, scriptTool, map[string]any{textKey: "1"}, ""},
	}
	for name, c := range cases {
		got := uploadRefusal(event{ToolName: c.tool, ToolInput: c.input, TranscriptPath: turnTranscript(t, c.calls...)})
		if c.refuse == "" && got != "" || c.refuse != "" && !strings.Contains(got, c.refuse) {
			t.Errorf("%s: refusal = %q, want %q", name, got, c.refuse)
		}
		if c.refuse != "" && !strings.Contains(got, UploadTargetScript) {
			t.Errorf("%s: refusal does not carry the check: %q", name, got)
		}
	}
}

// The check's scope line quotes the selector the session gives.
func TestUploadTargetCheck(t *testing.T) {
	if got := UploadTargetCheck(`form[action="/x"]`); !strings.HasPrefix(got, `const scope = "form[action=\"/x\"]";`+"\n(() => {") {
		t.Errorf("check = %q", got)
	}
}

// After one screenshot of the session timed out on a frozen renderer, every
// further screenshot or zoom is refused, alone or in a batch; reads pass,
// and so does a screenshot before any timeout.
func TestFrozenRefusal(t *testing.T) {
	computer := "mcp__claude-in-chrome__computer"
	shot := map[string]any{actionKey: "screenshot", tabKey: 7}
	timedOut := turnCall{computer, shot, "Error capturing screenshot: Timed out, the " + FrozenRenderer}
	fine := turnCall{computer, shot, "Screenshot taken"}
	batch := map[string]any{actionsKey: []any{map[string]any{nameKey: "navigate", inputKey: map[string]any{urlKey: "https://x"}},
		map[string]any{nameKey: "computer", inputKey: map[string]any{actionKey: "zoom"}}}}
	cases := map[string]struct {
		calls  []turnCall
		tool   string
		input  map[string]any
		refuse bool
	}{
		"a screenshot after a timeout":   {[]turnCall{fine, timedOut}, computer, shot, true},
		"a batch's zoom after a timeout": {[]turnCall{timedOut}, "mcp__claude-in-chrome__browser_batch", batch, true},
		"a page read after a timeout":    {[]turnCall{timedOut}, "mcp__claude-in-chrome__get_page_text", map[string]any{tabKey: 7}, false},
		"a click after a timeout":        {[]turnCall{timedOut}, computer, map[string]any{actionKey: "left_click"}, false},
		"a screenshot before any":        {[]turnCall{fine}, computer, shot, false},
	}
	for name, c := range cases {
		got := frozenRefusal(event{ToolName: c.tool, ToolInput: c.input, TranscriptPath: turnTranscript(t, c.calls...)})
		if (got != "") != c.refuse {
			t.Errorf("%s: refusal = %q, want refused %v", name, got, c.refuse)
		}
		// The refusal is not counted as another timeout.
		if strings.Contains(got, FrozenRenderer) {
			t.Errorf("%s: refusal carries %q", name, FrozenRenderer)
		}
	}
}
