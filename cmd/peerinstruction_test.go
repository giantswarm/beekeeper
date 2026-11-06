package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/peer"
)

func TestRelayInstructionQueuesPayloadSeparately(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	sent := fakeRelay(t, false)
	status, err := a.relayInstruction(context.Background(), "waiting", "recipient", "Click the OAuth consent")
	if err != nil || status != reviewedMergeQueued {
		t.Fatalf("relay: %s, %v", status, err)
	}
	if got := relayed(t, sent); len(got) != 1 || got[0] != "recipient" {
		t.Fatalf("notification: %v", got)
	}
	out, err := peer.InstructionHook(a.cfg.StateDir, "waiting", "Stop")
	if err != nil || !strings.Contains(string(out), "Click the OAuth consent") {
		t.Fatalf("next turn boundary: %s, %v", out, err)
	}
	out, err = peer.InstructionHook(a.cfg.StateDir, "waiting", "UserPromptSubmit")
	if err != nil || len(out) != 0 {
		t.Fatalf("duplicate: %s, %v", out, err)
	}
}

func TestRelayInstructionWithdrawsFailedNotification(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	fakeRelay(t, true)
	status, err := a.relayInstruction(context.Background(), "waiting", "recipient", "Click consent")
	if err == nil || status != "" {
		t.Fatalf("failed relay: %s, %v", status, err)
	}
	out, err := peer.InstructionHook(a.cfg.StateDir, "waiting", "Stop")
	if err != nil || len(out) != 0 {
		t.Fatalf("failed relay left instruction: %s, %v", out, err)
	}
}

func TestRelayTurnBoundaryHook(t *testing.T) {
	for _, event := range []string{"Stop", "UserPromptSubmit"} {
		t.Run(event, func(t *testing.T) {
			var out bytes.Buffer
			a, dir := scopeApp(t, &out)
			if _, err := peer.QueueInstruction(dir, "waiting", "Click consent now"); err != nil {
				t.Fatal(err)
			}
			got := runHook(t, a, &out, "relay", `{"session_id":"waiting","hook_event_name":"`+event+`"}`)
			if !strings.Contains(got, "Click consent now") {
				t.Fatalf("hook did not deliver: %s", got)
			}
		})
	}
}

func TestRelayReportsDelivered(t *testing.T) {
	if dir := os.Getenv("BEEKEEPER_TEST_RELAY_HOOK"); dir != "" {
		if out, err := peer.InstructionHook(dir, "waiting", "Stop"); err != nil || !strings.Contains(string(out), "Click consent") {
			t.Fatalf("hook: %s, %v", out, err)
		}
		return
	}
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	script := `"$BEEKEEPER_TEST_RELAY_BINARY" -test.run '^TestRelayReportsDelivered$' >/dev/null || exit 1
echo '{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"success\":true}"}]}}'
`
	fakeRelayTurn(t, script)
	t.Setenv("BEEKEEPER_TEST_RELAY_HOOK", a.cfg.StateDir)
	t.Setenv("BEEKEEPER_TEST_RELAY_BINARY", os.Args[0])
	status, err := a.relayInstruction(context.Background(), "waiting", "recipient", "Click consent")
	if err != nil || status != relayInstructionDelivered {
		t.Fatalf("relay: %s, %v", status, err)
	}
}
