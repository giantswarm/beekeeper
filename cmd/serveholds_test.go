package cmd

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestCentralHoldSetRefusesMachineLocalTargets(t *testing.T) {
	for _, target := range []string{holdMerges, holdGitHub} {
		t.Run(target, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Arguments = map[string]any{paramTarget: target, paramReason: "pause work"}
			// No store is needed: refuse before reading or changing central state.
			got, err := toolHoldSet(nil, req)
			if err == nil || Code(err) != ExitUsage || !strings.Contains(err.Error(), "machine-local hold target") || got != nil {
				t.Fatalf("hold_set %s: got %v, error %v", target, got, err)
			}
		})
	}
}

func TestLocalHoldSetStillAcceptsMachineWideTargets(t *testing.T) {
	run := holdRunner(t, "")
	for _, target := range []string{holdMerges, holdGitHub} {
		if out, code := run("set", target, "--reason", "pause work"); code != 0 || !strings.Contains(out, "held "+target) {
			t.Errorf("hold set %s: exit %d: %s", target, code, out)
		}
	}
}
