package guard

import "testing"

func TestConsentBatchDenialWinsOverUndecidedActions(t *testing.T) {
	path := transcriptWith(t, Page{URL: otherAppPage, Title: otherTitle}, false)
	denied := batchAction(chromeComputer, tabInput(leftClick, map[string]any{summaryKey: clicksAuthorize}))
	ordinary := batchAction(chromeNavigation, map[string]any{urlKey: "https://ordinary-navigation.example.test/", tabKey: "8"})
	for name, actions := range map[string][]any{
		"undecided first": {ordinary, denied},
		"denied first":    {denied, ordinary},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeBatch, map[string]any{actionsKey: actions})
			ev["transcript_path"] = path
			d := decideEvent(t, Hook{Apps: func() []App { return recorded }}, ev)
			if d == nil || d.PermissionDecision != decisionDeny {
				t.Fatal("a batch containing a denied consent was not denied")
			}
		})
	}
}

func TestConsentBatchUndecidedActionPreventsBlanketAllow(t *testing.T) {
	path := transcriptWith(t, Page{URL: ownPage, Title: ownTitle}, false)
	allowed := batchAction(chromeComputer, tabInput(leftClick, map[string]any{summaryKey: clicksAuthorize}))
	ordinary := batchAction(chromeNavigation, map[string]any{urlKey: "https://ordinary-navigation.example.test/", tabKey: "8"})
	for name, actions := range map[string][]any{
		"undecided first": {ordinary, allowed},
		"allowed first":   {allowed, ordinary},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeBatch, map[string]any{actionsKey: actions})
			ev["transcript_path"] = path
			if d := decideEvent(t, Hook{Apps: func() []App { return recorded }}, ev); d != nil && d.PermissionDecision != "" {
				t.Fatal("a partly undecided batch received a blanket decision")
			}
		})
	}
}
