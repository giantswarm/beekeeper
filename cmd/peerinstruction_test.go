package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The peer relay accepts its input, but no recipient hook runs. The Guide
// must still receive the payload, and a later holder must be able to read it.
func TestRelayGuideWithoutHooks(t *testing.T) {
	a, log := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	const session = "guide-without-hooks"
	const message = "P1 transport proof: four wakes lost\nKeep the alarm open."
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: session, Name: "Guide run fixture"}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	sent := fakeRelay(t, false)
	status, err := a.relayInstruction(context.Background(), session, "recipient", message)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(readFile(t, sent))
	var input struct{ Message string }
	if err := json.Unmarshal(bytes.TrimSpace(raw), &input); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input.Message, message) {
		t.Fatalf("no-hook recipient got only %q; payload is stranded", input.Message)
	}
	if status != relayPeerAccepted {
		t.Fatalf("peer acceptance is not recipient acknowledgement: %q", status)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.Contains(e.Detail, message) })
	if err != nil || len(evs) == 0 {
		t.Fatalf("message not durably readable in log: %v, %v", evs, err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide.Holder = &state.Supervisor{Party: state.Party{Session: "successor"}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	c := a.logCmd()
	if err := c.Flags().Set("verb", relayMessageVerb); err != nil {
		t.Fatal(err)
	}
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(message, "\n") {
		if !strings.Contains(log.String(), line) {
			t.Fatalf("successor cannot recover message from log: %q", log.String())
		}
	}
	// Enabling hooks later must not execute the direct peer payload twice.
	if out, err := peer.InstructionHook(a.cfg.StateDir, session, "Stop"); err != nil || len(out) != 0 {
		t.Fatalf("direct message also left a hook delivery: %s, %v", out, err)
	}
}

func TestRelayRequiresDurableMessageLog(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	a.store = unloggedStore{a.store}
	sent := fakeRelay(t, false)
	status, err := a.relayInstruction(t.Context(), "waiting", "recipient", "Do not lose this message")
	if err == nil || status != "" || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("unlogged relay: %q, %v", status, err)
	}
	if _, err := os.Stat(sent); !os.IsNotExist(err) {
		t.Fatalf("sent before durable logging: %v", err)
	}
}

func TestRelayGuideWithoutCommandStore(t *testing.T) {
	a, _ := stubApp(t)
	store := a.store
	a.store = nil // Hooks load configuration without the command store.
	fakeRelay(t, false)
	if status, err := a.relayInstruction(t.Context(), "s1", "recipient", "hook-originated message"); err != nil || status != relayPeerAccepted {
		t.Fatalf("hook relay: %q, %v", status, err)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == relayMessageVerb })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "hook-originated message") {
		t.Fatalf("hook message not retained: %v, %v", evs, err)
	}
}

func TestRelayGuidePeerFailureRetainsMessage(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	const session = "no-hook-guide"
	const message = "Recover this operational update after a failed send"
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: session}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	fakeRelay(t, true)
	status, err := a.relayInstruction(t.Context(), session, "recipient", message)
	if err == nil || status != "" || !strings.Contains(err.Error(), "acceptance unconfirmed") || !strings.Contains(err.Error(), "message retained") {
		t.Fatalf("failed send overstated delivery or lost recovery: %q, %v", status, err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == relayMessageVerb })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, message) {
		t.Fatalf("lost failed peer payload: %v, %v", evs, err)
	}
	fakeRelay(t, false)
	if status, err := a.relayInstruction(t.Context(), session, "recipient", message); err != nil || status != relayPeerAccepted {
		t.Fatalf("explicit retry: %q, %v", status, err)
	}
}

func TestGuideInstructionSessionUsesDesktopCLIIdentity(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Claude.DesktopDir = t.TempDir()
	holder := state.Party{Session: "old-cli", HostSession: "local_fixture"}
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: holder}}}
	writeFile(t, filepath.Join(a.cfg.Claude.DesktopDir, "fixture", "sessions", "local_fixture.json"),
		`{"sessionId":"local_fixture","cliSessionId":"current-cli"}`)
	if !a.guideInstructionSession(st, "current-cli") || a.guideInstructionSession(st, "unrelated-cli") {
		t.Fatal("desktop CLI replacement not resolved by identity")
	}
}

func TestWakeGuideAfterRelayWithoutHooks(t *testing.T) {
	a, out := stubApp(t)
	from := state.Party{Session: "s3", Name: "Previous guide"}
	to := state.Party{Session: "s9", Name: "Current guide"}
	runLive(t, a, map[int]string{3: from.Name, 9: to.Name})
	writeFile(t, filepath.Join(a.cfg.Claude.ProjectsDir, "fixture", "s9.jsonl"), `{"type":"user","cwd":"/fixture"}`+"\n")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Guide = &state.Role{Holder: &state.Supervisor{Party: from, Since: a.now.Add(-time.Hour)}}
		st.Agents = []state.Agent{{Party: from}, {Party: to}}
		if _, _, err := guideRole.relay(st, from, to, a.now, 15*time.Minute); err != nil {
			return nil, err
		}
		_, evs, err := guideRole.start(st, to, true, false, a.now.Add(time.Minute))
		return evs, err
	}); err != nil {
		t.Fatal(err)
	}
	sent := fakeRelay(t, false)
	const message = "The successor must receive this operational message"
	if err := a.wakeAgent(t.Context(), state.Party{Name: "fixture sender"}, guideName, message, ""); err != nil {
		t.Fatal(err)
	}
	if targets := relayed(t, sent); len(targets) != 1 || targets[0] != to.Name {
		t.Fatalf("wrong holder: %v", targets)
	}
	raw := []byte(readFile(t, sent))
	var input struct{ Message string }
	if err := json.Unmarshal(bytes.TrimSpace(raw), &input); err != nil || input.Message != message {
		t.Fatalf("payload: %q, %v", input.Message, err)
	}
	if !strings.Contains(out.String(), "accepted by peer queue") || strings.Contains(out.String(), "delivered") || strings.Contains(out.String(), "next tool") {
		t.Fatalf("wake overstated delivery: %s", out)
	}
}

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
	raw := []byte(readFile(t, sent))
	var input struct{ Message string }
	if err := json.Unmarshal(bytes.TrimSpace(raw), &input); err != nil || !strings.Contains(input.Message, "beekeeper log --verb agents.message") || strings.Contains(input.Message, "Click the OAuth consent") {
		t.Fatalf("worker notice must offer no-hook recovery without duplicating payload: %q, %v", input.Message, err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == relayMessageVerb })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "Click the OAuth consent") {
		t.Fatalf("no-hook recipient cannot recover payload: %v, %v", evs, err)
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
	if err != nil || status != relayInstructionConsumed {
		t.Fatalf("relay: %s, %v", status, err)
	}
}
