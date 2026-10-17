package guard

import (
	"encoding/json"
	"fmt"
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

// chromeUpload is the Chrome tool that sets the files of a file input.
const chromeUpload = "file_upload"

// uploadRefs are the refs the Chrome call's file_uploads target, each of a
// batch's; "" for one that names none.
func uploadRefs(tool string, input map[string]any) []string {
	switch chromeTool(tool) {
	case chromeUpload:
		ref, _ := input[refKey].(string)
		return []string{ref}
	case chromeBatch:
		actions, _ := input["actions"].([]any)
		var refs []string
		for _, a := range actions {
			m, _ := a.(map[string]any)
			name, _ := m[nameKey].(string)
			in, _ := m[inputKey].(map[string]any)
			refs = append(refs, uploadRefs(name, in)...)
		}
		return refs
	}
	return nil
}

// uploadTarget is what the transcript says of the session's upload target:
// the last check's result and the refs its label's page reads gave after it.
type uploadTarget struct {
	check string
	refs  []string
}

// readUploadTarget reads the upload target of the transcript: a result of a
// javascript_tool call that ran UploadTargetScript is a check, which drops
// the refs read before it; after a check that marked an input, every ref a
// result's line names beside UploadTargetLabel is the target's.
func readUploadTarget(transcript string) uploadTarget {
	var u uploadTarget
	checks := map[string]bool{}
	scanLines(transcript, func(line []byte) {
		for _, id := range checkCalls(line) {
			checks[id] = true
		}
		for _, r := range resultGroups(line) {
			text := strings.Join(r.texts, "\n")
			if checks[r.id] {
				u.check, u.refs = "", nil
				if i := strings.Index(text, UploadTargetLabel+": "); i >= 0 {
					u.check, _, _ = strings.Cut(text[i:], "\n")
				}
				continue
			}
			if !strings.HasPrefix(u.check, uploadMarked) {
				continue
			}
			for l := range strings.SplitSeq(text, "\n") {
				if strings.Contains(l, UploadTargetLabel) {
					u.refs = append(u.refs, refPattern.FindAllString(l, -1)...)
				}
			}
		}
	})
	return u
}

// checkCalls are the ids of the javascript_tool calls on a transcript line
// that run the upload-target check.
func checkCalls(line []byte) []string {
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
	var ids []string
	for _, b := range entry.Message.Content {
		if b.Type != "tool_use" || chromeTool(b.Name) != chromeScript {
			continue
		}
		if code, _ := b.Input[textKey].(string); strings.Contains(code, UploadTargetScript) {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// uploadRefusal returns why the hook refuses a Chrome call's file_upload, ""
// when it passes: one whose ref is not the input the session's last
// upload-target check marked.
func uploadRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) {
		return ""
	}
	refs := uploadRefs(ev.ToolName, ev.ToolInput)
	if len(refs) == 0 {
		return ""
	}
	u := readUploadTarget(ev.TranscriptPath)
	for _, ref := range refs {
		switch {
		case u.check == "":
			return uploadReason(fmt.Sprintf("the file_upload to %s comes before any upload-target check", refName(ref)))
		case strings.HasPrefix(u.check, uploadUnmarked):
			return uploadReason(fmt.Sprintf("the last upload-target check marked no input (%s)", u.check))
		case len(u.refs) == 0:
			return uploadReason("no find for " + quoteLabel + " followed the last upload-target check, so no ref is the marked input's")
		case ref == "" || !slices.Contains(u.refs, ref):
			return uploadReason(fmt.Sprintf("%s is not the input the last upload-target check marked (%s; find gave %s)",
				refName(ref), strings.TrimPrefix(u.check, uploadMarked), strings.Join(u.refs, ", ")))
		}
	}
	return ""
}

// quoteLabel is UploadTargetLabel as find is asked for it.
const quoteLabel = `"` + UploadTargetLabel + `"`

// refName names a file_upload's ref, "no ref" for none.
func refName(ref string) string {
	if ref == "" {
		return "no ref"
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
		actions, _ := input["actions"].([]any)
		for _, a := range actions {
			m, _ := a.(map[string]any)
			name, _ := m[nameKey].(string)
			in, _ := m[inputKey].(map[string]any)
			if takesPicture(name, in) {
				return true
			}
		}
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
