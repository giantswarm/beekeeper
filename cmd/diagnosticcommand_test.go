package cmd

import (
	"strings"
	"testing"
)

// The state lock's holder record named the supervisor loop and every start it
// runs "beekeeper": the root's --config ended the subcommand words. It names
// the resolved command and the flags given, never an argument or a value.
func TestDiagnosticCommandKeepsNoArgumentOrValue(t *testing.T) {
	const (
		private    = "private"
		loop       = "loop"
		configPath = "/home/" + private + "/config.yaml"
	)
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, pluginName},
		{[]string{workerGitHubConfigOption, configPath, supervisorCommand, loop}, "beekeeper supervisor loop --config"},
		{[]string{supervisorCommand, loop, "--once", testDryRunFlag}, "beekeeper supervisor loop --once --dry-run"},
		{[]string{workerGitHubConfigOption + "=" + configPath, fatalAsFlag, supervisorCommand + " " + loop, agentsName, agentStartName, "Review " + private + " 1 r1", "/home/" + private + "/brief.md"}, "beekeeper agents start --config --as"},
		{[]string{loopNote, loopAdd, "a " + private + " question", "--" + private + "-flag", "--", deliveryJSONFlag}, "beekeeper note add"},
		{[]string{deliveryJSONFlag, testStatusWord}, "beekeeper status --json"},
	} {
		got := diagnosticCommand(New(), c.args)
		if got != c.want || strings.Contains(got, private) {
			t.Errorf("diagnosticCommand(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
