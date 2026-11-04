package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
)

func TestManagedSupervisorStatusRefusesInsteadOfListingAgents(t *testing.T) {
	a, request := executionTestApp(t)
	s := hostUpdateServer(t, a, hostNewRelease)
	request.PolicyDigest = s.Policy().Digest()
	j := execution.Journal{Request: request, Digest: request.Digest(), Handle: execution.Handle{Ref: request.Ref, Unit: execution.UnitName(request.Ref), ConversationID: request.ConversationID}}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(request.Ref.Execution)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.OwnUnit = func() string { return j.Handle.Unit }
	t.Setenv(execution.EnvID, string(request.Ref.Execution))
	for _, args := range [][]string{{supervisorName, testStatusWord}, {supervisorName}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			a.out = &out
			root := &cobra.Command{Use: pluginName, SilenceUsage: true, SilenceErrors: true, PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return a.checkExecutionCommand(cmd) }}
			root.AddCommand(a.supervisorCmd())
			root.SetArgs(args)
			root.SetOut(&out)
			root.SetErr(&out)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "supervisor status is coordinator-only") {
				t.Fatalf("wanted coordinator-only refusal, got %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected status output: %q", out.String())
			}
		})
	}
}
