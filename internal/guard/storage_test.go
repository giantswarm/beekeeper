package guard

import (
	"strings"
	"testing"
)

func TestBrowseStorageRefusal(t *testing.T) {
	h := Hook{Shared: func() ([]string, bool) { return nil, true }}
	for _, code := range []string{
		`localStorage.getItem('session')`, `window['localStorage']`,
		`sessionStorage.getItem('key')`, `document.cookie`, `indexedDB.open('db')`,
		`window['sessionStorage']`, `cookieStore.getAll()`,
		`fetch('/api/usergroups.list')`, `new XMLHttpRequest()`,
		`const sessionToken = window.state.token`, `eval('code')`,
	} {
		t.Run(code, func(t *testing.T) {
			input := map[string]any{textKey: code}
			for _, ev := range []map[string]any{
				toolEvent(scriptTool, input),
				toolEvent(batchTool, map[string]any{actionsKey: []any{map[string]any{nameKey: chromeScript, inputKey: input}}}),
			} {
				d := decideEvent(t, h, ev)
				if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, BrowseStorageBarrier) {
					t.Fatalf("decision = %+v, want storage barrier", d)
				}
			}
		})
	}
	for _, code := range []string{`document.querySelector('[aria-label="User groups"]').click()`, `document.body.innerText`, DrawCheck} {
		ev := event{ToolName: scriptTool, ToolInput: map[string]any{textKey: code}}
		if got := storageRefusal(ev, h.Shared); got != "" {
			t.Errorf("UI script refused: %s", got)
		}
	}
	ev := event{ToolName: scriptTool, ToolInput: map[string]any{textKey: `localStorage.getItem('key')`}}
	if got := storageRefusal(ev, nil); got != "" {
		t.Errorf("non-browse call refused: %s", got)
	}
}
