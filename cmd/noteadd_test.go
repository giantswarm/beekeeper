package cmd

import (
	"strings"
	"testing"
)

func TestNoteAddMissingQuestion(t *testing.T) {
	for name, flags := range map[string][]string{
		"no context":   nil,
		"reason":       {"--for", "reviewer", "--why", "approval is needed"},
		"full context": {"--for", "reviewer", "--due", "3h", "--status-quo", "review is pending", "--why", "approval is needed", "--default", "keep waiting"},
	} {
		t.Run(name, func(t *testing.T) {
			c := New()
			c.SetArgs(append([]string{"note", "add"}, flags...))
			err := c.Execute()
			if err == nil || Code(err) != ExitUsage {
				t.Fatalf("missing question must fail with usage exit code: %v", err)
			}
			for _, want := range []string{"question as its first argument", "flags provide its context", "Usage: beekeeper note add <question> [flags]"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}
