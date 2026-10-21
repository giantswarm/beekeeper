package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// DrawCheck asks a tab whether Chrome draws it: two animation frames within
// a second and a half. A window the compositor does not draw (on a
// workspace not shown, minimised, under another) produces no frame while its
// document.visibilityState still says visible; a consent page there keeps
// its Authorize button disabled (its clickjacking guard waits on a drawn
// frame) and a click on it does nothing. It is one expression, which a
// second run on the same page evaluates again without a declaration clash.
const DrawCheck = "'" + drawOpen + "' + ((await new Promise(done => { const late = setTimeout(() => done(false), 1500); " +
	"requestAnimationFrame(() => requestAnimationFrame(() => { clearTimeout(late); done(true); })); })) ? '" + Drawn + "' : '" +
	NotDrawn + "') + ', visibility ' + document.visibilityState"

// drawOpen opens DrawCheck's answer; Drawn and NotDrawn are its verdicts.
const (
	drawOpen = "beekeeper draw check: "
	Drawn    = "drawn"
	NotDrawn = "not drawn"
)

// drawAnswer is DrawCheck's answer in a result's text.
var drawAnswer = regexp.MustCompile(regexp.QuoteMeta(drawOpen) + `(` + NotDrawn + `|` + Drawn + `)\b`)

// DrawVerdict is the verdict a result's text of DrawCheck gives, "" for
// none.
func DrawVerdict(text string) string {
	if m := drawAnswer.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// isDrawCheck reports whether code is DrawCheck and nothing else.
func isDrawCheck(code string) bool {
	return strings.TrimSpace(code) == DrawCheck
}

// LeaveDisarm takes a page's leave-page prompt ("Leave site? Changes you
// made may not be saved") away before its tab closes: a page with an
// unsaved form asks it on close, nobody answers, and the close hangs with
// the tab left open in the shared Chrome. It drops the page's
// onbeforeunload and makes every beforeunload listener's request for the
// prompt a no-op, whatever order the page registered them in.
const LeaveDisarm = "window.onbeforeunload = null; " +
	"BeforeUnloadEvent.prototype.preventDefault = function () {}; " +
	"Object.defineProperty(BeforeUnloadEvent.prototype, 'returnValue', { configurable: true, get() { return ''; }, set() {} }); " +
	"'" + leaveAnswer + "'"

// leaveAnswer is LeaveDisarm's answer.
const leaveAnswer = "beekeeper leave: no leave-page prompt"

// isLeaveDisarm reports whether code is LeaveDisarm and nothing else.
func isLeaveDisarm(code string) bool {
	return strings.TrimSpace(code) == LeaveDisarm
}

// pageCheck is what a tab's checks said while it was at url: DrawCheck's
// verdict, and whether LeaveDisarm ran there.
type pageCheck struct {
	url, drawn string
	left       bool
}

// pageChecks are the checks of each tab at its page as the transcript last
// showed it: a check made at an earlier URL of the tab says nothing of its
// page now. A DrawCheck counts as a call of its own, its verdict from its
// result; a LeaveDisarm counts alone or in a batch, run or refused (a page
// the extension refuses scripts on asks nothing either).
func pageChecks(transcript string) map[string]pageCheck {
	urls, checks := map[string]string{}, map[string]pageCheck{}
	at := func(tab string) pageCheck {
		c := checks[tab]
		if c.url != urls[tab] {
			c = pageCheck{url: urls[tab]}
		}
		return c
	}
	scanJoined(transcript, func(call chromeAction, text string) {
		if trimmed := strings.TrimSpace(text); strings.HasPrefix(trimmed, tabsJSONOpen) {
			for _, m := range tabJSON.FindAllStringSubmatch(trimmed, -1) {
				urls[m[1]] = unquote(m[3])
			}
		} else if i := contextLine.FindAllStringIndex(text, -1); len(i) > 0 {
			for _, m := range trailerTab.FindAllStringSubmatch(text[i[len(i)-1][0]:], -1) {
				urls[m[1]] = m[3]
			}
		}
		for _, a := range append([]chromeAction{call}, batchActions(call.input)...) {
			code, _ := a.input[textKey].(string)
			tab := tabOf(a.input[tabKey])
			if chromeTool(a.name) != chromeScript || tab == "" {
				continue
			}
			switch {
			case isDrawCheck(code) && a.name == call.name:
				c := at(tab)
				c.drawn = DrawVerdict(text)
				checks[tab] = c
			case isLeaveDisarm(code):
				c := at(tab)
				c.left = true
				checks[tab] = c
			}
		}
	})
	for tab := range checks {
		checks[tab] = at(tab)
	}
	return checks
}

// drawRefusal returns why the hook refuses a click or key it would allow on
// page, the tab's, "" when it passes: one the tab's DrawCheck at that page
// did not answer drawn, where the click would do nothing.
func drawRefusal(page Page, tab string, checks map[string]pageCheck) string {
	c := checks[tab]
	if c.url != page.URL {
		c = pageCheck{}
	}
	switch c.drawn {
	case Drawn:
		return ""
	case NotDrawn:
		return "Refused: Chrome does not draw this tab (its draw check answered '" + NotDrawn + "'): its window is not on screen " +
			"(on a workspace not shown, minimised or under another window), where a consent page keeps its button disabled and a click " +
			"or key does nothing. Open a fresh tab with tabs_create_mcp, navigate it to this page's URL and run the draw check there, " +
			"a call of its own. When that one answers '" + NotDrawn + "' too, click nothing on the page: end your report with the line '" +
			NotDrawnLine + "', naming the page and the step not done."
	}
	return "Refused: first check that Chrome draws this tab: a click or key on a page whose window is not on screen does nothing, " +
		"and a consent page there keeps its button disabled. Run this draw check with javascript_tool on the tab, a call of its own " +
		"(not in a batch), verbatim:\n" + DrawCheck + "\nWhen it answers '" + Drawn + "', click again; when it answers '" + NotDrawn +
		"', open a fresh tab, navigate it to this page's URL and check there once."
}

// NotDrawnLine is the line a browse turn's report names a page Chrome did
// not draw with.
const NotDrawnLine = "window not drawn"

// leaveRefusal returns why the hook refuses a Chrome call that closes tabs,
// "" when it passes: the close of a tab on a web page LeaveDisarm did not run
// on, which a leave-page prompt can hold open. A batch passes when it runs
// LeaveDisarm on the tab before its close.
func leaveRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) {
		return ""
	}
	closing := tabsClosing(ev.ToolName, ev.ToolInput)
	if len(closing) == 0 {
		return ""
	}
	var checks map[string]pageCheck
	disarmed := map[string]bool{}
	actions := []chromeAction{{ev.ToolName, ev.ToolInput}}
	if chromeTool(ev.ToolName) == chromeBatch {
		actions = batchActions(ev.ToolInput)
	}
	for _, a := range actions {
		tab := tabOf(a.input[tabKey])
		switch chromeTool(a.name) {
		case chromeScript:
			if code, _ := a.input[textKey].(string); isLeaveDisarm(code) {
				disarmed[tab] = true
			}
		case chromeTabsClose:
			if disarmed[tab] {
				continue
			}
			if checks == nil {
				checks = pageChecks(ev.TranscriptPath)
			}
			if c, ok := checks[tab]; ok && c.left {
				continue
			}
			page, ok := lastPage(ev.TranscriptPath, tab)
			if !ok || !webPage(page.URL) {
				continue
			}
			return fmt.Sprintf("Refused: tab %s is on a web page (%s), which can hold its close on a leave-page prompt (\"Leave site?\" over an "+
				"unsaved form) that nobody answers, leaving the tab open in the shared Chrome. First run this with javascript_tool on tab %s, "+
				"verbatim, alone or in the same batch before the close:\n%s\nthen close it. Run it even when the extension may refuse scripts "+
				"there; the close follows either way.", tab, hostOf(page.URL), tab, LeaveDisarm)
		}
	}
	return ""
}

// webPage reports whether a tab's URL is a web page, which can ask before
// it closes; a blank or browser page cannot.
func webPage(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && slices.Contains([]string{"http", "https"}, strings.ToLower(u.Scheme))
}

// hostOf is the host of a URL, the URL itself when it has none.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
