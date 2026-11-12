package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/loop"
)

// The state lock's holder record named the supervisor loop and every start it
// runs "beekeeper": the root's --config ended the subcommand words. It names
// the resolved command and the flags given, never an argument or a value.
func TestDiagnosticCommandKeepsNoArgumentOrValue(t *testing.T) {
	const (
		private    = "private"
		loopName   = "loop"
		configPath = "/home/" + private + "/config.yaml"
	)
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, pluginName},
		{[]string{workerGitHubConfigOption, configPath, supervisorCommand, loopName}, "beekeeper supervisor loop --config"},
		{[]string{supervisorCommand, loopName, "--once", testDryRunFlag}, "beekeeper supervisor loop --once --dry-run"},
		{[]string{workerGitHubConfigOption + "=" + configPath, fatalAsFlag, supervisorCommand + " " + loopName, agentsName, agentStartName, "Review " + private + " 1 r1", "/home/" + private + "/brief.md"}, "beekeeper agents start --config --as"},
		{[]string{loop.LoopNote, loop.LoopAdd, "a " + private + " question", "--" + private + "-flag", "--", jsonFlag}, "beekeeper note add"},
		{[]string{jsonFlag, testStatusWord}, "beekeeper status --json"},
	} {
		got := diagnosticCommand(New(), c.args)
		if got != c.want || strings.Contains(got, private) {
			t.Errorf("diagnosticCommand(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}
