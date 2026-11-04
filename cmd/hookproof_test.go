package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestHookProofScope(t *testing.T) {
	for _, tc := range []struct {
		name, event, want string
		known             bool
	}{
		{name: "outside", event: `{"session_id":"invented","cwd":"/outside"}`, want: "out of scope"},
		{name: "missing ID", event: `{"cwd":"/outside"}`, want: "real in-scope session ID"},
		{name: "invalid JSON", event: `{`, want: "invalid proof event"},
		{name: "known session", event: `{"session_id":"` + tStarted + `","cwd":"/outside"}`, known: true},
		{name: "agy known session", event: `{"conversationId":"` + tStarted + `","workspacePaths":["/outside"]}`, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			a, dir := scopeApp(t, &out)
			t.Setenv(codex.EnvAgent, "")
			if tc.known {
				store, err := state.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Update(func(s *state.State) ([]state.Event, error) {
					s.Starts = append(s.Starts, state.Start{Party: state.Party{Session: tStarted}})
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			c := a.hookCmd()
			c.SetIn(strings.NewReader(tc.event))
			c.SetArgs([]string{"check-scope"})
			err := c.Execute()
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				if out.Len() != 0 {
					t.Fatalf("failed proof printed success: %q", out.String())
				}
			} else if err != nil || !strings.Contains(out.String(), "proof event is in scope") {
				t.Fatalf("error = %v, output = %q", err, out.String())
			}
		})
	}
}
