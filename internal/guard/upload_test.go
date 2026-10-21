package guard

import (
	"encoding/json"
	"fmt"
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
		id := fmt.Sprintf("toolu_%d", i)
		in, err := json.Marshal(c.input)
		if err != nil {
			t.Fatal(err)
		}
		res, err := json.Marshal(c.result)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf(useLine, id, c.tool, in), fmt.Sprintf(turnResult, id, res))
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// useLine and turnResult are a transcript's tool_use and tool_result lines.
const (
	useLine    = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`
	turnResult = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":[{"type":"text","text":%s}]}]}}`
)

// The Chrome tools of the tests, and the words of two refusals.
const (
	findTool    = "mcp__claude-in-chrome__find"
	batchTool   = "mcp__claude-in-chrome__browser_batch"
	noFind      = "no find for"
	beforeCheck = "before any upload-target check"
)

// The calls of an upload: the check, its result, the find and its refs.
var (
	checkCall = func(result string) turnCall {
		return turnCall{scriptTool, map[string]any{textKey: UploadTargetCheck("#new_comment_form"), tabKey: json.Number("7")}, result}
	}
	marked   = checkCall(UploadTargetLabel + ": marked input#fc-new_comment_field in form#new_comment_form (2 other file inputs hidden or disabled); find")
	unmarked = checkCall(UploadTargetLabel + ": none: 2 file inputs, every one hidden or disabled (input in form#review-reply)")
	findCall = turnCall{findTool, map[string]any{tabKey: json.Number("7")},
		"Found 1 matching element\n\n- ref_42: button \"" + UploadTargetLabel + "\" (file) - Exact match of the accessible name\n"}
	readPage = turnCall{"mcp__claude-in-chrome__read_page", map[string]any{tabKey: json.Number("7")},
		"form [ref_2]\n label \"Attach to reply\" [ref_4]\n  button [ref_9] type=\"file\"\n  button \"" + UploadTargetLabel + "\" [ref_42] type=\"file\"\n"}
	// fake is a script of the turn's own that prints the check's words;
	// appended runs the check and more code after it.
	fake     = turnCall{scriptTool, map[string]any{textKey: "'" + UploadTargetLabel + ": marked x'", tabKey: json.Number("7")}, UploadTargetLabel + ": marked x"}
	appended = turnCall{scriptTool, map[string]any{textKey: UploadTargetCheck("") + ";document.querySelector('#reply-file').setAttribute('aria-label','" +
		UploadTargetLabel + "')", tabKey: json.Number("7")}, UploadTargetLabel + ": marked x"}
	// otherTab is the find in another tab; reload navigates the check's tab;
	// twice is a find that gives two refs the label.
	otherTab = turnCall{findTool, map[string]any{tabKey: json.Number("8")},
		"- ref_42: button \"" + UploadTargetLabel + "\" (file)\n"}
	reload = turnCall{"mcp__claude-in-chrome__navigate", map[string]any{urlKey: "https://x", tabKey: json.Number("7")}, "Navigated"}
	twice  = turnCall{findTool, map[string]any{tabKey: json.Number("7")},
		"- ref_42: button \"" + UploadTargetLabel + "\" (file)\n- ref_9: button \"" + UploadTargetLabel + "\" (file)\n"}
	// requoted is find's answer with the query's quotes kept around the
	// name, bare and curly its answers with none and with typographic ones.
	requoted = turnCall{findTool, map[string]any{tabKey: json.Number("7")},
		"Found 1 matching element\n\n- ref_42: button \"\"" + UploadTargetLabel + "\"\" (file) - Exact match for \"" + UploadTargetLabel + "\"\n"}
	bare  = turnCall{findTool, map[string]any{tabKey: json.Number("7")}, "- ref_42: button " + UploadTargetLabel + " (file)\n"}
	curly = turnCall{findTool, map[string]any{tabKey: json.Number("7")}, "- ref_42: button “" + UploadTargetLabel + "” (file)\n"}
	// namedIn is a page read whose text names the label in no element's name.
	namedIn = turnCall{"mcp__claude-in-chrome__get_page_text", map[string]any{tabKey: json.Number("7")}, "ref_9 says " + UploadTargetLabel}
)

// A file_upload passes only to the ref the last upload-target check marked
// and its find gave; one before any check, after a check that marked none,
// without a find, to another ref, or after the turn's own imitation of the
// check is refused; a batch's file_upload is decided the same way.
func TestUploadRefusal(t *testing.T) {
	upload := func(ref string) map[string]any {
		return map[string]any{refKey: ref, tabKey: json.Number("7"), "paths": []any{"/x/shot.png"}}
	}
	cases := map[string]struct {
		calls  []turnCall
		tool   string
		input  map[string]any
		refuse string
	}{
		"the marked ref":                {[]turnCall{marked, findCall}, uploadTool, upload("ref_42"), ""},
		"the marked ref of a page read": {[]turnCall{marked, readPage}, uploadTool, upload("ref_42"), ""},
		"find's name quoted twice":      {[]turnCall{marked, requoted}, uploadTool, upload("ref_42"), ""},
		"find's name unquoted":          {[]turnCall{marked, bare}, uploadTool, upload("ref_42"), ""},
		"find's name in curly quotes":   {[]turnCall{marked, curly}, uploadTool, upload("ref_42"), ""},
		"a check and find in the upload's batch": {[]turnCall{findCall}, batchTool,
			map[string]any{actionsKey: []any{map[string]any{nameKey: "javascript_tool", inputKey: map[string]any{textKey: UploadTargetCheck(""), tabKey: json.Number("7")}},
				map[string]any{nameKey: "find", inputKey: map[string]any{tabKey: json.Number("7")}},
				map[string]any{nameKey: "file_upload", inputKey: upload("ref_42")}}}, "the file_upload in a call of its own"},
		"a hidden input's ref":       {[]turnCall{marked, readPage}, uploadTool, upload("ref_9"), "ref_9 is not the input the last upload-target check marked"},
		"no check":                   {[]turnCall{findCall}, uploadTool, upload("ref_42"), "comes before any upload-target check"},
		"a check that marked none":   {[]turnCall{marked, findCall, unmarked}, uploadTool, upload("ref_42"), "marked no input"},
		"no find after the check":    {[]turnCall{findCall, marked}, uploadTool, upload("ref_42"), noFind},
		"the turn's own imitation":   {[]turnCall{fake, findCall}, uploadTool, upload("ref_42"), beforeCheck},
		"code after the check":       {[]turnCall{appended, findCall}, uploadTool, upload("ref_42"), beforeCheck},
		"a find in another tab":      {[]turnCall{marked, otherTab}, uploadTool, upload("ref_42"), noFind},
		"an upload to another tab":   {[]turnCall{marked, findCall}, uploadTool, map[string]any{refKey: "ref_42", tabKey: json.Number("8")}, "targets tab 8"},
		"a navigate after the check": {[]turnCall{marked, findCall, reload}, uploadTool, upload("ref_42"), beforeCheck},
		"two refs with the label":    {[]turnCall{marked, twice}, uploadTool, upload("ref_42"), "several refs"},
		"a mention in a page's text": {[]turnCall{marked, namedIn}, uploadTool, upload("ref_9"), noFind},
		"a script after the check": {[]turnCall{marked, findCall, {scriptTool, map[string]any{textKey: "document.querySelector('#x').remove()", tabKey: json.Number("7")}, "ok"}},
			uploadTool, upload("ref_42"), beforeCheck},
		"a click after the check": {[]turnCall{marked, findCall, {"mcp__claude-in-chrome__computer", map[string]any{actionKey: leftClick, tabKey: json.Number("7")}, "ok"}},
			uploadTool, upload("ref_42"), beforeCheck},
		"a script in another tab": {[]turnCall{marked, findCall, {scriptTool, map[string]any{textKey: "1", tabKey: json.Number("8")}, "1"}}, uploadTool, upload("ref_42"), ""},
		"a batch's script before its upload": {[]turnCall{marked, findCall}, batchTool,
			map[string]any{actionsKey: []any{map[string]any{nameKey: "javascript_tool", inputKey: map[string]any{textKey: "x()", tabKey: json.Number("7")}},
				map[string]any{nameKey: "file_upload", inputKey: upload("ref_42")}}}, "an action of the batch before"},
		"no ref": {[]turnCall{marked, findCall}, uploadTool, map[string]any{tabKey: json.Number("7")}, "none is not the input"},
		"a batch's upload": {[]turnCall{marked, findCall}, batchTool,
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

// In a browse turn the hook passes a file_upload only of the files the
// caller shared with --file, alone or in a batch, and names the path and the
// flag when it refuses; outside a browse turn it decides no path.
func TestSharedRefusal(t *testing.T) {
	staged := "/state/browse/1/upload/1/shot.png"
	shared := func(files ...string) func() ([]string, bool) { return func() ([]string, bool) { return files, true } }
	notBrowse := func() ([]string, bool) { return nil, false }
	upload := func(paths ...any) map[string]any { return map[string]any{refKey: "ref_42", "paths": paths} }
	batch := func(paths ...any) map[string]any {
		return map[string]any{actionsKey: []any{map[string]any{nameKey: chromeUpload, inputKey: upload(paths...)}}}
	}
	cases := map[string]struct {
		shared func() ([]string, bool)
		tool   string
		input  map[string]any
		refuse string
	}{
		"the shared copy":          {shared(staged), uploadTool, upload(staged), ""},
		"the shared copy, unclean": {shared(staged), uploadTool, upload("/state/browse/1/upload/./1/shot.png"), ""},
		"the caller's own path":    {shared(staged), uploadTool, upload("/work/shot.png"), NotShared + "/work/shot.png"},
		"one of two not shared":    {shared(staged), uploadTool, upload(staged, "/home/x/.ssh/id_ed25519"), NotShared + "/home/x/.ssh/id_ed25519"},
		"nothing shared":           {shared(), uploadTool, upload("/work/shot.png"), "(shared: none)"},
		"a batch's upload":         {shared(staged), batchTool, batch("/work/shot.png"), NotShared + "/work/shot.png"},
		"a batch's shared upload":  {shared(staged), batchTool, batch(staged), ""},
		"not a browse turn":        {notBrowse, uploadTool, upload("/work/shot.png"), ""},
		"no hook configuration":    {nil, uploadTool, upload("/work/shot.png"), ""},
		"not an upload":            {shared(), scriptTool, map[string]any{textKey: "1"}, ""},
	}
	for name, c := range cases {
		got := sharedRefusal(event{ToolName: c.tool, ToolInput: c.input}, c.shared)
		if c.refuse == "" && got != "" || c.refuse != "" && (!strings.Contains(got, c.refuse) || !strings.Contains(got, "beekeeper browse --file <path>")) {
			t.Errorf("%s: refusal = %q, want %q", name, got, c.refuse)
		}
	}
	// The hook answers with the refusal, before the upload-target check's.
	ev := toolEvent(uploadTool, upload("/work/shot.png"))
	ev[sessionKey] = "s"
	raw, _ := json.Marshal(ev)
	if out := string((Hook{Shared: shared(staged)}).Decide(raw)); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "not shared for upload: /work/shot.png") {
		t.Errorf("Decide = %s", out)
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
	shot := map[string]any{actionKey: "screenshot", tabKey: json.Number("7")}
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
		"a batch's zoom after a timeout": {[]turnCall{timedOut}, batchTool, batch, true},
		"a page read after a timeout":    {[]turnCall{timedOut}, "mcp__claude-in-chrome__get_page_text", map[string]any{tabKey: json.Number("7")}, false},
		"a click after a timeout":        {[]turnCall{timedOut}, computer, map[string]any{actionKey: leftClick}, false},
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
