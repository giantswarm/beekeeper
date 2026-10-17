package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tabsCloseTool is the Chrome tool that closes a tab.
const tabsCloseTool = "mcp__claude-in-chrome__tabs_close_mcp"

// tabsTranscript writes a transcript of tool results with the texts.
func tabsTranscript(t *testing.T, texts ...string) string {
	t.Helper()
	var lines []string
	for _, text := range texts {
		quoted, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":`+string(quoted)+`}]}]}}`)
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The results of a browse turn that replaced its group's first tab with a
// fresh one: the close dissolved the group and stranded the fresh tab.
var (
	groupMade  = `{"availableTabs":[{"tabId":806748780,"title":"New Tab","url":"chrome://newtab/"}],"tabGroupId":1741570757}` + "\n\nTab Context:\n- Available tabs:\n  • tabId 806748780: \"New Tab\" (\"chrome://newtab/\")"
	freshTab   = "Created new tab. Tab ID: 806748785 \n\nTab Context:\n- Executed on tabId: 806748785\n- Available tabs:\n  • tabId 806748780: \"Workflow\" (\"https://app.circleci.com/x\")\n  • tabId 806748785: \"New Tab\" (\"chrome://newtab/\")"
	firstClose = "[tabs_close_mcp] Closed tab 806748780. 1 tab(s) remain.\n\nactions[1] (navigate) failed: Failed to navigate: Tab 806748785 is not in the same group as the current tab."
	groupAgain = `{"availableTabs":[{"tabId":806748787,"title":"New Tab","url":"chrome://newtab/"}],"tabGroupId":1897549377}` + "\n\nTab Context:\n- Available tabs:\n  • tabId 806748787: \"New Tab\" (\"chrome://newtab/\")"
	lastClose  = "Closed tab 806748787. Group is now empty (auto-removed). \n\nTab Context:\n- Available tabs:\n"
)

// The tabs a session opened are its groups' tabs and the tabs it created;
// the anchor is the first tab of its current group; a tab closed is no
// longer open; a tab outside the group the last result lists is stray.
func TestReadTabs(t *testing.T) {
	tabs := ReadTabs(tabsTranscript(t, groupMade, freshTab, freshTab))
	if !slices.Equal(tabs.Opened, []string{"806748780", "806748785"}) || tabs.Anchor != "806748780" ||
		!slices.Equal(tabs.Reachable(), []string{"806748780", "806748785"}) || len(tabs.Stray()) != 0 {
		t.Errorf("tabs of a group with a fresh tab = %+v", tabs)
	}
	tabs = ReadTabs(tabsTranscript(t, groupMade, freshTab, firstClose, groupAgain, lastClose))
	if !slices.Equal(tabs.Open(), []string{"806748785"}) || tabs.Anchor != "806748787" ||
		len(tabs.Reachable()) != 0 || !slices.Equal(tabs.Stray(), []string{"806748785"}) {
		t.Errorf("tabs after the first tab closed first = %+v", tabs)
	}
	tabs = ReadTabs(tabsTranscript(t, groupMade, "Closed tab 806748780. Group is now empty (auto-removed). \n\nTab Context:\n- Available tabs:\n"))
	if len(tabs.Open()) != 0 || len(tabs.Opened) != 1 {
		t.Errorf("tabs of a turn that closed its one tab = %+v", tabs)
	}
	if tabs := ReadTabs(""); len(tabs.Opened) != 0 {
		t.Errorf("tabs of no transcript = %+v", tabs)
	}
}

// A close of the group's first tab while another tab of the session is in
// the group is refused, alone or in a batch; the other tab first, then the
// first tab, passes, in two calls or in one batch.
func TestTabCloseRefusal(t *testing.T) {
	transcript := tabsTranscript(t, groupMade, freshTab)
	closeTab := func(id float64) event {
		return event{ToolName: tabsCloseTool, ToolInput: map[string]any{tabKey: id}, TranscriptPath: transcript}
	}
	batch := func(ids ...float64) event {
		var actions []any
		for _, id := range ids {
			actions = append(actions, map[string]any{nameKey: "tabs_close_mcp", inputKey: map[string]any{tabKey: id}})
		}
		return event{ToolName: "mcp__claude-in-chrome__browser_batch", ToolInput: map[string]any{actionsKey: actions}, TranscriptPath: transcript}
	}
	if r := tabCloseRefusal(closeTab(806748780)); !strings.Contains(r, "tab 806748780 is the tab your tab group was made with") ||
		!strings.Contains(r, "Close tab 806748785 first and tab 806748780 last") {
		t.Errorf("close of the first tab = %q", r)
	}
	if r := tabCloseRefusal(batch(806748780, 806748785)); r == "" {
		t.Error("a batch closing the first tab first passed")
	}
	for name, ev := range map[string]event{"the other tab": closeTab(806748785), "a batch in order": batch(806748785, 806748780)} {
		if r := tabCloseRefusal(ev); r != "" {
			t.Errorf("close of %s = %q", name, r)
		}
	}
	// The first tab alone in the group closes; another tool passes.
	alone := tabsTranscript(t, groupMade)
	if r := tabCloseRefusal(event{ToolName: tabsCloseTool, ToolInput: map[string]any{tabKey: 806748780.0}, TranscriptPath: alone}); r != "" {
		t.Errorf("close of the one tab = %q", r)
	}
	if r := tabCloseRefusal(event{ToolName: "mcp__claude-in-chrome__navigate", ToolInput: map[string]any{tabKey: 806748780.0}, TranscriptPath: transcript}); r != "" {
		t.Errorf("a navigate = %q", r)
	}
	// The hook decides it.
	path, _ := json.Marshal(transcript)
	in := []byte(`{"tool_name":"` + tabsCloseTool + `","tool_input":{"tabId":806748780},"transcript_path":` + string(path) + `}`)
	if out := (Hook{}).Decide(in); !strings.Contains(string(out), `"permissionDecision":"deny"`) {
		t.Errorf("the hook answered %s", out)
	}
}

// The Stop hook has a turn with tabs of its own in its group go on once to
// close them, the first tab last; a turn with none open, a stray tab only,
// or one that went on already ends.
func TestBrowseStop(t *testing.T) {
	stop := func(transcript string, active bool) map[string]string {
		in, _ := json.Marshal(map[string]any{"transcript_path": transcript, "stop_hook_active": active})
		out := BrowseStop(in)
		if out == nil {
			return nil
		}
		var m map[string]string
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	open := tabsTranscript(t, groupMade, freshTab)
	if m := stop(open, false); m["decision"] != "block" || !strings.Contains(m["reason"], "in this order: 806748785, 806748780") {
		t.Errorf("Stop with two tabs open = %v", m)
	}
	if m := stop(open, true); m != nil {
		t.Errorf("Stop after going on once = %v", m)
	}
	if m := stop(tabsTranscript(t, groupMade, freshTab, firstClose, groupAgain, lastClose), false); m != nil {
		t.Errorf("Stop with a stray tab only = %v", m)
	}
	if out := BrowseStop([]byte("not json")); out != nil {
		t.Errorf("Stop of malformed input = %s", out)
	}
}
