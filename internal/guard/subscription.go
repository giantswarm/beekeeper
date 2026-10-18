package guard

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A subscription control is a page's Subscribe, Unsubscribe, Watch, Star or
// Notifications button: a click there changes the person's own notifications
// or lists, which an agent's token cannot restore. The hook refuses every
// Chrome click that targets one, in any session: a click whose summary or
// ref names one, a scripted click whose code names one, and a scripted click
// that picks its element from a sweep over a page's buttons or links, which
// can land on one.

// SubscriptionControls names the controls a browser call never clicks; a
// refusal's reason carries it, which a browse report reads.
const SubscriptionControls = "a subscription, watch, star or notification control"

// controlWords name a subscription control in a call's words or an
// element's line of a page read.
var controlWords = regexp.MustCompile(`(?i)\b(?:un)?(?:subscribe[ds]?|subscriptions?|watch(?:ed|ing)?|star(?:s|red)?|notifications?)\b`)

// clicking are the computer actions that click an element.
var clicking = []string{actionLeftClick, "right_click", "double_click", "triple_click"}

// The javascript tool of the Chrome tools, the key of its code, and a click
// in its code.
const (
	chromeScript = "javascript_tool"
	refKey       = "ref"
)

var scriptedClick = regexp.MustCompile(`\.click\s*\(|dispatchEvent\s*\(|\.submit\s*\(`)

// sweep is a script's selection of every button or link of a page: the
// element a scripted click then picks is none the step named.
var sweep = regexp.MustCompile(`(?i)(?:querySelectorAll\s*\(\s*["'` + "`" + `]\s*(?:\*|` + sweptTags + `)(?:\s*,\s*(?:` + sweptTags + `))*\s*["'` + "`" + `]\s*\)` +
	`|getElementsByTagName\s*\(\s*["'` + "`" + `]\s*(?:\*|button|a|summary)\s*["'` + "`" + `]\s*\))`)

const sweptTags = `button|a|summary|input|\[role=["']?(?:button|menuitem|link)["']?\]`

// target is one click of a Chrome call: the ref it clicks (a computer click
// or a form value by ref) and its own words (the summary the model wrote, a
// script's code).
type target struct {
	ref, words string
	script     bool
}

// subscriptionRefusal returns why the hook refuses a Chrome call that clicks
// a subscription control, "" when it passes.
func subscriptionRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) {
		return ""
	}
	var labels map[string]string
	for _, t := range clickTargets(ev.ToolName, ev.ToolInput) {
		if m := controlWords.FindString(t.words); m != "" {
			what := "the click's summary"
			if t.script {
				what = "the scripted click's code"
			}
			return subscriptionReason(fmt.Sprintf("%s names %s (%q)", what, SubscriptionControls, m))
		}
		if t.script {
			if m := sweep.FindString(t.words); m != "" {
				return subscriptionReason(fmt.Sprintf("the scripted click picks its element from a sweep over the page's buttons or links (%s), "+
					"where it can land on %s", m, SubscriptionControls))
			}
			continue
		}
		if t.ref == "" {
			continue
		}
		if labels == nil {
			labels = refLines(ev.TranscriptPath)
		}
		if m := controlWords.FindString(labels[t.ref]); m != "" {
			return subscriptionReason(fmt.Sprintf("the click's %s is %s (%q) in the page as the session last read it: %s",
				t.ref, SubscriptionControls, m, strings.TrimSpace(labels[t.ref])))
		}
	}
	return ""
}

// subscriptionReason is the hook's refusal of a click that why describes.
func subscriptionReason(why string) string {
	return "Refused: " + why + ". A browser call never clicks " + SubscriptionControls + " (GitHub's Subscribe, Unsubscribe, Watch, " +
		"Star and Notifications buttons): such a click changes the person's own notifications, which an agent's token cannot restore. " +
		"Click only the element the step names: a computer click by that element's ref, its action_summary naming that element alone, " +
		"or a script that selects that one element by its own selector (an id, a data-testid, an aria-label), never a sweep over the page's buttons."
}

// clickTargets are the clicks of a Chrome call: a computer click, a form
// value, a scripted click, and each of a batch's; none for a call that does
// not click.
func clickTargets(tool string, input map[string]any) []target {
	t := strings.ToLower(tool)
	if i := strings.LastIndex(t, "__"); i >= 0 {
		t = t[i+len("__"):]
	}
	summary, _ := input[summaryKey].(string)
	switch t {
	case chromeComputer:
		if action, _ := input[actionKey].(string); slices.Contains(clicking, action) {
			ref, _ := input[refKey].(string)
			return []target{{ref: ref, words: summary}}
		}
	case chromeForm:
		ref, _ := input[refKey].(string)
		return []target{{ref: ref, words: summary}}
	case chromeScript:
		if code, _ := input[textKey].(string); scriptedClick.MatchString(code) {
			return []target{{words: code, script: true}}
		}
	case chromeBatch:
		actions, _ := input["actions"].([]any)
		var out []target
		for _, a := range actions {
			m, _ := a.(map[string]any)
			name, _ := m[nameKey].(string)
			in, _ := m[inputKey].(map[string]any)
			out = append(out, clickTargets(name, in)...)
		}
		return out
	}
	return nil
}

// refPattern is an element's ref in a page read (read_page, find).
var refPattern = regexp.MustCompile(`\bref_\d+\b`)

// refLines are the lines of the transcript's page reads that name each ref,
// the last read's line for a ref read again.
func refLines(transcript string) map[string]string {
	lines := map[string]string{}
	scanResults(transcript, func(text string) {
		for l := range strings.SplitSeq(text, "\n") {
			for _, ref := range refPattern.FindAllString(l, -1) {
				lines[ref] = l
			}
		}
	})
	return lines
}
