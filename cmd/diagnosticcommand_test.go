package cmd

import (
	"strings"
	"testing"
)

// The state lock's holder record named the supervisor loop and every start it
// runs "beekeeper": the root's --config ended the subcommand words. It names
// the resolved command and the flags given, never an argument or a value.
func TestDiagnosticCommandKeepsNoArgumentOrValue(t *testing.T) {
	const private = "private"
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "beekeeper"},
		{[]string{"--config", "/home/" + private + "/config.yaml", "supervisor", "loop"}, "beekeeper supervisor loop --config"},
		{[]string{"supervisor", "loop", "--once", "--dry-run"}, "beekeeper supervisor loop --once --dry-run"},
		{[]string{"--config=/home/" + private + "/config.yaml", "--as", "supervisor loop", "agents", "start", "Review " + private + " 1 r1", "/home/" + private + "/brief.md"}, "beekeeper agents start --config --as"},
		{[]string{"note", "add", "a " + private + " question", "--" + private + "-flag", "--", "--json"}, "beekeeper note add"},
		{[]string{"--json", "status"}, "beekeeper status --json"},
	} {
		got := diagnosticCommand(New(), c.args)
		if got != c.want || strings.Contains(got, private) {
			t.Errorf("diagnosticCommand(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
