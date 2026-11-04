package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func subscriptionTranscript(t *testing.T, calls ...drawsCall) string {
	t.Helper()
	var lines []string
	for i, call := range calls {
		id := strconv.Itoa(i)
		use, err := json.Marshal(map[string]any{"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": call.tool, "input": call.input},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(use), strings.Replace(resultLine(strconv.Quote(call.text)), `"tool_use_id":"t"`, `"tool_use_id":"`+id+`"`, 1))
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubscriptionLabelsComeFromTheClickedTabsBrowserReads(t *testing.T) {
	const subscribed = `- button "Subscribe" [ref_45]`
	const ordinary = `- button "Continue" [ref_45]`
	read := func(tab, text string) drawsCall {
		return drawsCall{tool: browserTools + chromeReadPage, input: map[string]any{tabKey: tab}, text: text}
	}
	for name, tc := range map[string]struct {
		first, later drawsCall
		deny         bool
	}{
		"shell cannot clear":     {read(tab7, subscribed), drawsCall{tool: bashTool, input: map[string]any{commandKey: "printf fixture"}, text: ordinary}, true},
		"other tab cannot clear": {read(tab7, subscribed), read("8", ordinary), true},
		"other tab cannot block": {read(tab7, ordinary), read("8", subscribed), false},
		"same tab new read":      {read(tab7, subscribed), read(tab7, ordinary), false},
		"script cannot clear":    {read(tab7, subscribed), drawsCall{tool: browserTools + chromeScript, input: map[string]any{tabKey: tab7}, text: ordinary}, true},
		"single tab batch read": {read(tab7, ordinary), drawsCall{tool: browserTools + chromeBatch, input: map[string]any{"actions": []any{
			batchAction(chromeReadPage, map[string]any{tabKey: tab7}),
		}}, text: subscribed}, true},
		"ambiguous batch cannot clear": {read(tab7, subscribed), drawsCall{tool: browserTools + chromeBatch, input: map[string]any{"actions": []any{
			batchAction(chromeReadPage, map[string]any{tabKey: tab7}),
			batchAction(chromeReadPage, map[string]any{tabKey: "8"}),
		}}, text: subscribed + "\n" + ordinary}, true},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeComputer, tabInput(leftClick, map[string]any{refKey: "ref_45", summaryKey: "Clicks the button"}))
			ev["transcript_path"] = subscriptionTranscript(t, tc.first, tc.later)
			d := decideEvent(t, Hook{}, ev)
			if got := d != nil && d.PermissionDecision == decisionDeny; got != tc.deny {
				t.Fatalf("refused = %v, want %v", got, tc.deny)
			}
		})
	}
}
