package guard

import "testing"

func TestSubscriptionLabelsComeFromTheClickedTabsBrowserReads(t *testing.T) {
	const subscribed = `- button "Subscribe" [ref_45]`
	const ordinary = `- button "Continue" [ref_45]`
	read := func(tab, text string) turnCall {
		return turnCall{tool: browserTools + chromeReadPage, input: map[string]any{tabKey: tab}, result: text}
	}
	for name, tc := range map[string]struct {
		first, later turnCall
		deny         bool
	}{
		"shell cannot clear":     {read(tab7, subscribed), turnCall{tool: bashTool, input: map[string]any{commandKey: "printf fixture"}, result: ordinary}, true},
		"other tab cannot clear": {read(tab7, subscribed), read("8", ordinary), true},
		"other tab cannot block": {read(tab7, ordinary), read("8", subscribed), false},
		"same tab new read":      {read(tab7, subscribed), read(tab7, ordinary), false},
		"script cannot clear":    {read(tab7, subscribed), turnCall{tool: browserTools + chromeScript, input: map[string]any{tabKey: tab7}, result: ordinary}, true},
		"single tab batch read": {read(tab7, ordinary), turnCall{tool: browserTools + chromeBatch, input: map[string]any{actionsKey: []any{
			batchAction(chromeReadPage, map[string]any{tabKey: tab7}),
		}}, result: subscribed}, true},
		"mixed batch cannot clear": {read(tab7, subscribed), turnCall{tool: batchTool, input: map[string]any{actionsKey: []any{
			batchAction(chromeReadPage, map[string]any{tabKey: tab7}),
			batchAction(chromeScript, map[string]any{tabKey: tab7}),
		}}, result: subscribed + "\n" + ordinary}, true},
		"nested mixed batch cannot clear": {read(tab7, subscribed), turnCall{tool: batchTool, input: map[string]any{actionsKey: []any{
			batchAction(chromeBatch, map[string]any{actionsKey: []any{
				batchAction(chromeFind, map[string]any{tabKey: tab7}),
				batchAction(chromeScript, map[string]any{tabKey: tab7}),
			}}),
		}}, result: ordinary}, true},
		"single tab batch can clear": {read(tab7, subscribed), turnCall{tool: batchTool, input: map[string]any{actionsKey: []any{
			batchAction(chromeReadPage, map[string]any{tabKey: tab7}),
			batchAction(chromeFind, map[string]any{tabKey: tab7}),
		}}, result: ordinary}, false},
		"ambiguous batch cannot clear": {read(tab7, subscribed), turnCall{tool: browserTools + chromeBatch, input: map[string]any{actionsKey: []any{
			batchAction(chromeReadPage, map[string]any{tabKey: tab7}),
			batchAction(chromeReadPage, map[string]any{tabKey: "8"}),
		}}, result: subscribed + "\n" + ordinary}, true},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeComputer, tabInput(leftClick, map[string]any{refKey: subscriptionRef, summaryKey: "Clicks the button"}))
			ev["transcript_path"] = turnTranscript(t, tc.first, tc.later)
			d := decideEvent(t, Hook{}, ev)
			if got := d != nil && d.PermissionDecision == decisionDeny; got != tc.deny {
				t.Fatalf("refused = %v, want %v", got, tc.deny)
			}
		})
	}
}
