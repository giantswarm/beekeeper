package hypr

import (
	"context"
	"encoding/json"
	"testing"
)

// Focus, rather than workspace visibility or history, determines whether
// the person is using Chrome, including a window moved during the turn.
func TestFocusedChromeAcrossWorkspaces(t *testing.T) {
	for _, ws := range []Workspace{ws2, music, magic, surface} {
		t.Run(ws.Name, func(t *testing.T) {
			for _, focused := range []bool{false, true} {
				w := chrome
				w.Workspace = ws
				w.FocusHistoryID = 1
				ctl := func(_ context.Context, _ ...string) ([]byte, error) {
					if focused {
						return json.Marshal(w)
					}
					return []byte(`{"address":"0xt","class":"terminal"}`), nil
				}
				got, err := FocusedChrome(t.Context(), ctl)
				if err != nil || (got != "") != focused {
					t.Errorf("focused %v: got %q, %v", focused, got, err)
				}
			}
		})
	}
}
