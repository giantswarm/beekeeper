package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A file input a page hides (a review-reply box GitHub keeps in the DOM
// beside the comment box, display:none) takes a file_upload as readily as
// the one the step means: the file is uploaded and attached nowhere the
// person sees. The hook therefore passes a file_upload only to the input the
// session's last upload-target check marked: UploadTargetScript, run with
// javascript_tool, marks the one visible, enabled file input of the scope
// the session names with UploadTargetLabel and refuses to mark a hidden one;
// the session's find for that label gives the ref file_upload takes.

// UploadTargetLabel is the accessible name the upload-target check gives the
// input it marks, which find reads.
const UploadTargetLabel = "beekeeper upload target"

// The words an upload-target check's result opens with: one input marked,
// or none.
const (
	uploadMarked   = UploadTargetLabel + ": marked "
	uploadUnmarked = UploadTargetLabel + ": none: "
)

// UploadTargetScript is the upload-target check after its scope line: in the
// scope (a selector of the intended form or comment box, "" for the page) it
// clears the label of every file input, marks the one input that is enabled
// and whose box is rendered (no ancestor display:none, visibility:hidden,
// hidden or inert) and scrolls it into view; several such inputs or none
// mark nothing and say which there are.
const UploadTargetScript = `(() => {
  const label = '` + UploadTargetLabel + `';
  document.querySelectorAll('input[type=file]').forEach(e => { if (e.getAttribute('aria-label') === label) e.removeAttribute('aria-label'); });
  const root = scope ? document.querySelector(scope) : document;
  if (!root) return label + ': none: no element matches the scope ' + scope;
  const name = e => { const f = e.form || e.closest('form'); return 'input' + (e.id ? '#' + e.id : '') + (e.name ? '[name=' + e.name + ']' : '') +
    (f ? ' in form' + (f.id ? '#' + f.id : '') + (f.getAttribute('action') ? ' (' + f.getAttribute('action') + ')' : '') : ''); };
  const shown = e => !e.disabled && !!e.parentElement && e.parentElement.checkVisibility({ visibilityProperty: true }) && !e.parentElement.closest('[hidden],[inert]');
  const all = [...root.querySelectorAll('input[type=file]')];
  const ok = all.filter(shown);
  if (ok.length === 1) { ok[0].setAttribute('aria-label', label); ok[0].parentElement.scrollIntoView({ block: 'center' });
    return label + ': marked ' + name(ok[0]) + ' (' + (all.length - 1) + ' other file inputs hidden or disabled); find "' + label + '" for its ref'; }
  if (ok.length > 1) return label + ': none: ' + ok.length + ' visible file inputs (' + ok.map(name).join('; ') + '): run again with scope set to the intended form';
  return label + ': none: ' + all.length + ' file inputs, every one hidden or disabled' + (all.length ? ' (' + all.map(name).join('; ') + ')' : '');
})()`

// UploadTargetCheck is the upload-target check as a session runs it, its
// scope line first; the hook knows the check by UploadTargetScript.
func UploadTargetCheck(scope string) string {
	q, _ := json.Marshal(scope)
	return "const scope = " + string(q) + ";\n" + UploadTargetScript
}

// chromeUpload is the Chrome tool that sets the files of a file input;
// chromeFind and chromeReadPage are the page reads that give an element's
// ref.
const (
	chromeUpload   = "file_upload"
	chromeFind     = "find"
	chromeReadPage = "read_page"
)

// An upload is one file_upload of a Chrome call: the ref and the tab it
// targets, and whether an action of its batch before it changes that tab's
// page.
type upload struct {
	ref, tab string
	changed  bool
}

// uploads are the file_uploads of a Chrome call, each of a batch's.
func uploads(tool string, input map[string]any) []upload {
	switch chromeTool(tool) {
	case chromeUpload:
		ref, _ := input[refKey].(string)
		return []upload{{ref: ref, tab: tabOf(input[tabKey])}}
	case chromeBatch:
		var out []upload
		actions := batchActions(input)
		for i, a := range actions {
			for _, up := range uploads(a.name, a.input) {
				up.changed = slices.ContainsFunc(actions[:i], func(b chromeAction) bool { return changesPage(b.name, b.input, up.tab) })
				out = append(out, up)
			}
		}
		return out
	}
	return nil
}

// A chromeAction is one Chrome call or batch action: its tool and input.
type chromeAction struct {
	name  string
	input map[string]any
}

// batchActions are the actions of a browser_batch's input.
func batchActions(input map[string]any) []chromeAction {
	actions, _ := input["actions"].([]any)
	out := make([]chromeAction, 0, len(actions))
	for _, a := range actions {
		m, _ := a.(map[string]any)
		name, _ := m[nameKey].(string)
		in, _ := m[inputKey].(map[string]any)
		out = append(out, chromeAction{name, in})
	}
	return out
}

// changesPage reports whether a Chrome call can change the page of tab
// (a call naming no tab can change any): a navigate, a click, key or typed
// text, a form value, a script other than the upload-target check, or a
// batch's.
func changesPage(tool string, input map[string]any, tab string) bool {
	if t := tabOf(input[tabKey]); t != "" && t != tab && chromeTool(tool) != chromeBatch {
		return false
	}
	switch chromeTool(tool) {
	case chromeNavigation, chromeForm:
		return true
	case chromeComputer:
		action, _ := input[actionKey].(string)
		return slices.Contains(acting, action)
	case chromeScript:
		code, _ := input[textKey].(string)
		return !isCheck(code)
	case chromeBatch:
		return slices.ContainsFunc(batchActions(input), func(a chromeAction) bool { return changesPage(a.name, a.input, tab) })
	}
	return false
}

// checkScope opens the upload-target check: its scope line.
var checkScope = regexp.MustCompile(`^const scope = ("(?:[^"\\\n]|\\.)*");\n`)

// isCheck reports whether code is the upload-target check and nothing else:
// a scope line, then UploadTargetScript.
func isCheck(code string) bool {
	code = strings.TrimSpace(code)
	m := checkScope.FindStringSubmatch(code)
	var scope string
	return m != nil && json.Unmarshal([]byte(m[1]), &scope) == nil && code[len(m[0]):] == UploadTargetScript
}

// labelled is the element line of a page read whose accessible name is
// UploadTargetLabel: find's `- ref_9: button "<label>" (file)` and
// read_page's `button "<label>" [ref_9] type="file"`.
var labelled = regexp.MustCompile(`^\s*(?:-\s*(ref_\d+):\s*\S+\s+` + regexp.QuoteMeta(quoteLabel) + `(?:\s|$)|\S+\s+` +
	regexp.QuoteMeta(quoteLabel) + `\s+\[(ref_\d+)\])`)

// uploadTarget is what the transcript says of the session's upload target:
// the last check's result and its tab, and the refs the page reads of that
// tab gave the label after it.
type uploadTarget struct {
	check, tab string
	refs       []string
}

// readUploadTarget reads the upload target of the transcript. A result of a
// javascript_tool call whose code is the upload-target check is a check,
// which drops the refs read before it; a call that can change its tab's page
// after it (changesPage) drops the check.
// After a check that marked an input, a ref a find or read_page of its tab
// gives an element named UploadTargetLabel is the target's.
func readUploadTarget(transcript string) uploadTarget {
	var u uploadTarget
	calls := map[string]chromeAction{}
	scanLines(transcript, func(line []byte) {
		for _, c := range toolCalls(line) {
			calls[c.id] = c.chromeAction
			if u.check != "" && changesPage(c.name, c.input, u.tab) {
				u = uploadTarget{}
			}
		}
		for _, r := range resultGroups(line) {
			c, ok := calls[r.id]
			if !ok {
				continue
			}
			text := strings.Join(r.texts, "\n")
			tab := tabOf(c.input[tabKey])
			switch tool := chromeTool(c.name); {
			case tool == chromeScript:
				code, _ := c.input[textKey].(string)
				if !isCheck(code) {
					continue
				}
				u = uploadTarget{tab: tab}
				if strings.HasPrefix(text, UploadTargetLabel+": ") {
					u.check, _, _ = strings.Cut(text, "\n")
				}
			case (tool == chromeFind || tool == chromeReadPage) && strings.HasPrefix(u.check, uploadMarked) && tab == u.tab:
				for l := range strings.SplitSeq(text, "\n") {
					if m := labelled.FindStringSubmatch(l); m != nil {
						u.refs = addTab(u.refs, m[1]+m[2])
					}
				}
			}
		}
	})
	return u
}

// A toolUse is one tool call of a transcript line: its id, tool and input.
type toolUse struct {
	id string
	chromeAction
}

// toolCalls are the tool calls on a transcript line, with their inputs.
func toolCalls(line []byte) []toolUse {
	var entry struct {
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return nil
	}
	var out []toolUse
	for _, b := range entry.Message.Content {
		if b.Type == "tool_use" {
			out = append(out, toolUse{b.ID, chromeAction{b.Name, b.Input}})
		}
	}
	return out
}

// uploadRefusal returns why the hook refuses a Chrome call's file_upload, ""
// when it passes: one whose tab and ref are not the tab and the one input
// the session's last upload-target check marked.
func uploadRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) {
		return ""
	}
	ups := uploads(ev.ToolName, ev.ToolInput)
	if len(ups) == 0 {
		return ""
	}
	u := readUploadTarget(ev.TranscriptPath)
	for _, up := range ups {
		switch {
		case up.changed:
			return uploadReason(fmt.Sprintf("an action of the batch before the file_upload to %s can change its page after the upload-target check", refName(up.ref)))
		case u.check == "":
			return uploadReason(fmt.Sprintf("the file_upload to %s comes before any upload-target check of its page", refName(up.ref)))
		case strings.HasPrefix(u.check, uploadUnmarked):
			return uploadReason(fmt.Sprintf("the last upload-target check marked no input (%s)", u.check))
		case up.tab != u.tab:
			return uploadReason(fmt.Sprintf("the file_upload targets tab %s, the last upload-target check ran in tab %s", refName(up.tab), refName(u.tab)))
		case len(u.refs) == 0:
			return uploadReason("no find for " + quoteLabel + " in its tab followed the last upload-target check, so no ref is the marked input's")
		case len(u.refs) > 1:
			return uploadReason(fmt.Sprintf("the page reads after the last upload-target check give several refs the label (%s)", strings.Join(u.refs, ", ")))
		case up.ref != u.refs[0]:
			return uploadReason(fmt.Sprintf("%s is not the input the last upload-target check marked (%s; find gave %s)",
				refName(up.ref), strings.TrimPrefix(u.check, uploadMarked), u.refs[0]))
		}
	}
	return ""
}

// quoteLabel is UploadTargetLabel as find is asked for it.
const quoteLabel = `"` + UploadTargetLabel + `"`

// refName names a file_upload's ref or tab, "none" for none.
func refName(ref string) string {
	if ref == "" {
		return "none"
	}
	return ref
}

// uploadReason is the hook's refusal of a file_upload that why describes.
func uploadReason(why string) string {
	return "Refused: " + why + ". A page keeps hidden file inputs (a review-reply box beside the comment box) that take an upload " +
		"as readily as the one the step means, and attach the file nowhere the person sees. Before file_upload, run the upload-target " +
		"check with javascript_tool on the tab, the scope set to a selector of the intended form or comment box (\"\" for the whole page):\n" +
		UploadTargetCheck("<selector>") + "\nIt marks the one visible, enabled file input there, or says why it marked none (hidden, " +
		"disabled, several: narrow the scope). Then find " + quoteLabel + " and file_upload to the ref it gives; never upload to a ref the check did not mark."
}

// FrozenRenderer is in the error of a screenshot of a tab whose window the
// compositor does not draw: the capture times out.
const FrozenRenderer = "renderer may be frozen"

// takesPicture reports whether a Chrome call takes a picture of the page: a
// screenshot or a zoom, or a batch's.
func takesPicture(tool string, input map[string]any) bool {
	switch chromeTool(tool) {
	case chromeComputer:
		action, _ := input[actionKey].(string)
		return action == "screenshot" || action == "zoom"
	case chromeBatch:
		return slices.ContainsFunc(batchActions(input), func(a chromeAction) bool { return takesPicture(a.name, a.input) })
	}
	return false
}

// frozenRefusal returns why the hook refuses a Chrome call's screenshot, ""
// when it passes: once one screenshot of the session timed out on a frozen
// renderer, every other waits its own timeout on the same undrawn window, so
// the session reads the page instead.
func frozenRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) || !takesPicture(ev.ToolName, ev.ToolInput) {
		return ""
	}
	frozen := false
	scanResults(ev.TranscriptPath, func(text string) { frozen = frozen || strings.Contains(text, FrozenRenderer) })
	if !frozen {
		return ""
	}
	return "Refused: a screenshot of this session already timed out on a frozen renderer: Chrome's window is not drawn (occluded, " +
		"minimised or behind another tab), and every further screenshot would wait out its own timeout on it. Take no further screenshot: " +
		"read the page with get_page_text or read_page for the evidence, and name the page 'not rendered' in your report."
}
