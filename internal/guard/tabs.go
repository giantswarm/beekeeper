package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Tabs are the Chrome tabs a session opened, as its transcript's tool
// results show them: a tabs_context result lists the tabs of the session's
// own tab group, which the session made, a tabs_create result names the tab
// it created, a tabs_close result the tab it closed, and every result ends
// with the tabs its group holds now. Anchor is the tab the session's current
// group was made with: the Claude in Chrome extension keys the group by it,
// so closing it while other tabs remain dissolves the group and leaves them
// open outside it, where no call of the session reaches them.
type Tabs struct {
	Opened, Closed, Group []string
	Anchor                string
}

// The words of the Chrome tools' results the session's tabs are read from.
var (
	tabsGroup   = regexp.MustCompile(`"availableTabs":\s*\[([^\]]*)\],\s*"tabGroupId":\s*(\d+)`)
	tabsID      = regexp.MustCompile(`"tabId":\s*(\d+)`)
	tabsCreated = regexp.MustCompile(`Created new tab\. Tab ID: (\d+)`)
	tabsClosed  = regexp.MustCompile(`Closed tab (\d+)\.`)
)

// availableTabs opens the list of the tabs a result's group holds now.
const availableTabs = "Available tabs:"

// ReadTabs reads the tabs of the session's transcript; none for a transcript
// that cannot be read.
func ReadTabs(transcript string) Tabs {
	var t Tabs
	group := ""
	scanResults(transcript, func(text string) { t.read(text, &group) })
	return t
}

// read adds what one result's text says of the tabs; group is the id of the
// group the anchor was read from.
func (t *Tabs) read(text string, group *string) {
	for _, m := range tabsGroup.FindAllStringSubmatch(text, -1) {
		ids := tabsID.FindAllStringSubmatch(m[1], -1)
		for _, id := range ids {
			t.Opened = addTab(t.Opened, id[1])
		}
		if m[2] != *group && len(ids) > 0 {
			*group, t.Anchor = m[2], ids[0][1]
		}
	}
	for _, m := range tabsCreated.FindAllStringSubmatch(text, -1) {
		t.Opened = addTab(t.Opened, m[1])
	}
	for _, m := range tabsClosed.FindAllStringSubmatch(text, -1) {
		t.Closed = addTab(t.Closed, m[1])
	}
	if _, after, ok := strings.Cut(text, availableTabs); ok {
		t.Group = t.Group[:0]
		for _, m := range tabLine.FindAllStringSubmatch(after, -1) {
			t.Group = addTab(t.Group, m[1])
		}
	}
}

// addTab is tabs with id after them, once.
func addTab(tabs []string, id string) []string {
	if slices.Contains(tabs, id) {
		return tabs
	}
	return append(tabs, id)
}

// Open are the tabs the session opened and did not close, in the order it
// opened them.
func (t Tabs) Open() []string {
	return slices.DeleteFunc(slices.Clone(t.Opened), func(id string) bool { return slices.Contains(t.Closed, id) })
}

// Reachable are the open tabs still in the session's group, which its calls
// reach; Stray the open tabs outside it, which they no longer reach.
func (t Tabs) Reachable() []string {
	return slices.DeleteFunc(t.Open(), func(id string) bool { return !slices.Contains(t.Group, id) })
}

// Stray are the open tabs outside the session's group.
func (t Tabs) Stray() []string {
	return slices.DeleteFunc(t.Open(), func(id string) bool { return slices.Contains(t.Group, id) })
}

// chromeTabsClose is the Chrome tool that closes a tab.
const chromeTabsClose = "tabs_close_mcp"

// chromeTool is a Chrome tool's name after the server's prefix.
func chromeTool(tool string) string {
	t := strings.ToLower(tool)
	if i := strings.LastIndex(t, "__"); i >= 0 {
		t = t[i+len("__"):]
	}
	return t
}

// tabsClosing are the tabs a Chrome call closes, in order: a tabs_close's
// and each of a batch's.
func tabsClosing(tool string, input map[string]any) []string {
	switch chromeTool(tool) {
	case chromeTabsClose:
		if id := tabOf(input[tabKey]); id != "" {
			return []string{id}
		}
	case chromeBatch:
		actions, _ := input["actions"].([]any)
		var ids []string
		for _, a := range actions {
			m, _ := a.(map[string]any)
			name, _ := m[nameKey].(string)
			in, _ := m[inputKey].(map[string]any)
			ids = append(ids, tabsClosing(name, in)...)
		}
		return ids
	}
	return nil
}

// tabCloseRefusal returns why the hook refuses a Chrome call, "" when it
// passes: a close of the tab the session's group was made with while other
// tabs the session opened are still in the group, which would leave them
// open outside it, out of the session's reach.
func tabCloseRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) {
		return ""
	}
	closing := tabsClosing(ev.ToolName, ev.ToolInput)
	if len(closing) == 0 {
		return ""
	}
	t := ReadTabs(ev.TranscriptPath)
	if t.Anchor == "" {
		return ""
	}
	for _, id := range closing {
		if id == t.Anchor {
			if rest := slices.DeleteFunc(t.Reachable(), func(o string) bool { return o == id }); len(rest) > 0 {
				return fmt.Sprintf("Refused: tab %s is the tab your tab group was made with; closing it while tab %s of yours is still open "+
					"dissolves the group and leaves %s open outside it, where no call of yours reaches it. Close tab %s first and tab %s last. "+
					"To leave a frozen or hidden page, reload it in the same tab (navigate to its own URL) instead of replacing the tab.",
					id, strings.Join(rest, ", "), strings.Join(rest, ", "), strings.Join(rest, ", "), id)
			}
		}
		t.Closed = addTab(t.Closed, id)
	}
	return ""
}

// stopEvent is the part of a Stop event BrowseStop reads.
type stopEvent struct {
	TranscriptPath string `json:"transcript_path"`
	Active         bool   `json:"stop_hook_active"`
}

// BrowseStop is a browse turn's Stop hook's answer for the event, nil to let
// the turn end: while tabs the turn opened are still open in its group, the
// turn goes on once to close them, the group's first tab last. A turn that
// went on once already ends; browse names the tabs it left.
func BrowseStop(input []byte) []byte {
	var ev stopEvent
	if json.Unmarshal(input, &ev) != nil || ev.Active {
		return nil
	}
	t := ReadTabs(ev.TranscriptPath)
	open := t.Reachable()
	if len(open) == 0 {
		return nil
	}
	if i := slices.Index(open, t.Anchor); i >= 0 {
		open = append(slices.Delete(open, i, i+1), t.Anchor)
	}
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": fmt.Sprintf(
		"Before you end, close the tabs you opened that are still open, blank ones included, with tabs_close_mcp in this order: %s "+
			"(the tab your group was made with last), each on a web page after this, run on it with javascript_tool (alone or in the same batch "+
			"before its close), so no leave-page prompt holds it open:\n%s\nThen end with your report as before, adding which tabs you closed.",
		strings.Join(open, ", "), LeaveDisarm)})
	return out
}
