//go:build unix

package agy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSendRefusesStoppedInboxAndDrainsInOrder(t *testing.T) {
	stateDir := t.TempDir()
	inbox := InboxPath(stateDir, "1234abcd-0000-4000-8000-000000000001")

	if err := Send(inbox, "before start"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Send before MakeInbox = %v, want ErrNotRunning", err)
	}
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "no reader"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Send without reader = %v, want ErrNotRunning", err)
	}
	if msgs, err := Drain(inbox); err != nil || len(msgs) != 0 {
		t.Fatalf("Drain after refused Send = %v, %v, want empty", msgs, err)
	}

	reader, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	for _, m := range []string{" first ", "second", "third"} {
		if err := Send(inbox, m); err != nil {
			t.Fatalf("Send(%q) = %v", m, err)
		}
	}
	got, err := Drain(inbox)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third"}
	if !slices.Equal(got, want) {
		t.Fatalf("Drain = %q, want %q", got, want)
	}
	if again, err := Drain(inbox); err != nil || len(again) != 0 {
		t.Fatalf("second Drain = %q, %v, want empty", again, err)
	}

	removed, err := RemoveInbox(inbox)
	if err != nil || !removed {
		t.Fatalf("RemoveInbox = %v, %v", removed, err)
	}
}

func TestPreInvocationInjectsOnceInOrder(t *testing.T) {
	stateDir := t.TempDir()
	const (
		startID = "1234abcd-0000-4000-8000-000000000002"
		convID  = "conv-uuid-0001"
	)
	inbox := InboxPath(stateDir, startID)
	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	reader, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	linkConversation(inbox, convID)
	if err := Send(inbox, "msg one"); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "msg two"); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"conversationId":"` + convID + `","invocationNum":2}`)
	out := PreInvocation(stateDir, payload)
	if out == nil {
		t.Fatal("PreInvocation returned nil, want injectSteps")
	}
	var res PreInvocationResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal PreInvocation output %q: %v", out, err)
	}
	want := []InjectedStep{{UserMessage: "msg one"}, {UserMessage: "msg two"}}
	if !slices.Equal(res.InjectSteps, want) {
		t.Fatalf("InjectSteps = %+v, want %+v", res.InjectSteps, want)
	}

	if second := PreInvocation(stateDir, payload); second != nil {
		t.Fatalf("second PreInvocation = %q, want nil (already drained)", second)
	}
}

// transcriptEntry is a line in the mock agy transcript.
type transcriptEntry struct {
	StepIndex int    `json:"step_index"`
	Type      string `json:"type"`
	Tool      string `json:"tool,omitempty"`
	Content   string `json:"content,omitempty"`
}

// TestHelperMockAgy acts as a mock agy process speaking stream-json on stdin/stdout,
// invoking PreInvocation before each model call and writing a transcript file.
func TestHelperMockAgy(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_MOCK_AGY") != "1" {
		return
	}
	stateDir := os.Getenv("MOCK_AGY_STATE_DIR")
	transcriptPath := os.Getenv("MOCK_AGY_TRANSCRIPT")
	convID := os.Getenv("MOCK_AGY_CONV_ID")
	mode := os.Getenv("MOCK_AGY_MODE")
	signalFile := os.Getenv("MOCK_AGY_SIGNAL_FILE")
	resumeSignal := os.Getenv("MOCK_AGY_RESUME_SIGNAL")

	stepIdx := 0
	if raw, err := os.ReadFile(transcriptPath); err == nil && len(raw) > 0 { //nolint:gosec // test helper path
		stepIdx = len(bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}))
	}
	appendTranscript := func(typ, tool, content string) {
		stepIdx++
		b, _ := json.Marshal(transcriptEntry{
			StepIndex: stepIdx,
			Type:      typ,
			Tool:      tool,
			Content:   content,
		})
		f, err := os.OpenFile(transcriptPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test helper path
		if err != nil {
			os.Exit(2)
		}
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(os.Stdout, "%s\n", b)
	}
	const (
		keyEvent      = "event"
		keyConvID     = "conversation_id"
		keyStepUpdate = "step_update"
	)
	runHook := func(inv int) {
		raw, _ := json.Marshal(PreInvocationArgs{ConversationID: convID, InvocationNum: inv})
		out := PreInvocation(stateDir, raw)
		if len(out) == 0 {
			return
		}
		var res PreInvocationResult
		if json.Unmarshal(out, &res) != nil {
			return
		}
		for _, s := range res.InjectSteps {
			appendTranscript("USER_INPUT", "", s.UserMessage)
			emit(map[string]any{
				keyEvent: keyStepUpdate,
				keyStepUpdate: map[string]any{
					keyConvID:   convID,
					"step_type": "user_input",
				},
			})
		}
	}

	emit(map[string]any{keyEvent: "init", keyConvID: convID})

	sc := bufio.NewScanner(os.Stdin)
	turnNum := 0
	for sc.Scan() {
		var in streamMessage
		if json.Unmarshal(sc.Bytes(), &in) != nil || in.Event != "user" {
			continue
		}
		turnNum++
		if mode == "crash-before-accept" && turnNum == 1 {
			// Simulate crashing after stdin write before recording user_input.
			os.Exit(1)
		}
		appendTranscript("USER_INPUT", "", in.Message.Content)
		emit(map[string]any{
			keyEvent: keyStepUpdate,
			keyStepUpdate: map[string]any{
				keyConvID:   convID,
				"step_type": "user_input",
			},
		})

		// Model call 1: makes first tool call.
		runHook(1)
		appendTranscript("PLANNER_RESPONSE", "tool_call_1", "")

		if turnNum == 1 && signalFile != "" {
			// Signal the test that tool_call_1 has completed inside turn 1.
			_ = os.WriteFile(signalFile, []byte("tool_call_1\n"), 0o600) //nolint:gosec // test helper path
			for range 200 {
				if _, err := os.Stat(resumeSignal); err == nil { //nolint:gosec // test helper path
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}

		// Model call 2: runs after tool_call_1 (drains mid-turn wake messages).
		runHook(2)
		appendTranscript("PLANNER_RESPONSE", "tool_call_2", "")

		// Model call 3: final response of the turn.
		runHook(3)
		appendTranscript("PLANNER_RESPONSE", "", "turn complete: "+in.Message.Content)

		emit(map[string]any{
			keyEvent: "result",
			"result": map[string]any{
				keyConvID: convID,
				"status":  "DONE",
			},
		})

		if mode == "one-turn" || (mode == "two-turns" && turnNum >= 2) {
			return
		}
	}
}

// TestDeliveryMidTurnIdleAndAcrossRestart verifies the requirements in
// https://github.com/giantswarm/beekeeper/issues/1682#issuecomment-6453042504:
//  1. A turn that makes several tool calls receives messages sent after its
//     first tool call via PreInvocation, and they appear in the transcript
//     before the turn ends.
//  2. A message sent while the session is idle is delivered as the next turn.
//  3. Every message is delivered exactly once and in FIFO order.
//  4. Unread messages (both mid-turn queued and unaccepted inflight) survive a
//     session restart unread and are delivered in order on restart.
func TestDeliveryMidTurnIdleAndAcrossRestart(t *testing.T) {
	stateDir := t.TempDir()
	const (
		startID = "1234abcd-0000-4000-8000-000000000003"
		convID  = "conv-uuid-0003"
	)
	inbox := InboxPath(stateDir, startID)
	logPath := StreamPath(stateDir, startID)
	transcriptPath := filepath.Join(stateDir, "transcript.jsonl")
	signalFile := filepath.Join(stateDir, "after-tool-1")
	resumeSignal := filepath.Join(stateDir, "resume-turn-1")

	if err := MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	// Hold a reader open long enough to seed the initial brief into the queue,
	// then close it so Run opens its own descriptor.
	seedReader, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox
	if err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "initial brief"); err != nil {
		t.Fatal(err)
	}
	_ = seedReader.Close()

	runSession := func(ctx context.Context, mode string) error {
		t.Setenv("GO_WANT_HELPER_MOCK_AGY", "1")
		t.Setenv("MOCK_AGY_STATE_DIR", stateDir)
		t.Setenv("MOCK_AGY_TRANSCRIPT", transcriptPath)
		t.Setenv("MOCK_AGY_CONV_ID", convID)
		t.Setenv("MOCK_AGY_MODE", mode)
		t.Setenv("MOCK_AGY_SIGNAL_FILE", signalFile)
		t.Setenv("MOCK_AGY_RESUME_SIGNAL", resumeSignal)
		argv := []string{os.Args[0], "-test.run=^TestHelperMockAgy$"}
		return Run(ctx, inbox, logPath, argv, io.Discard)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runSession(ctx, "two-turns")
	}()

	// Wait for Turn 1 to finish its first tool call.
	for range 200 {
		if _, err := os.Stat(signalFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(signalFile); err != nil {
		t.Fatalf("turn 1 did not reach tool_call_1: %v", err)
	}

	// Send two wake messages mid-turn after the first tool call.
	if err := Send(inbox, "mid-turn first"); err != nil {
		t.Fatalf("Send mid-turn first: %v", err)
	}
	if err := Send(inbox, "mid-turn second"); err != nil {
		t.Fatalf("Send mid-turn second: %v", err)
	}
	// Let Turn 1 proceed to its second model call (which runs PreInvocation).
	if err := os.WriteFile(resumeSignal, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Wait until Turn 1 completes in the transcript, then send an idle wake
	// message for Turn 2, plus two extra messages after Turn 2 starts that
	// will remain unread when the session exits after Turn 2.
	for range 200 {
		raw, _ := os.ReadFile(transcriptPath) //nolint:gosec // test file
		if strings.Contains(string(raw), "turn complete: initial brief") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := Send(inbox, "idle second turn"); err != nil {
		t.Fatalf("Send idle second turn: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("first Run failed: %v", err)
	}

	readTranscript := func() []transcriptEntry {
		t.Helper()
		raw, err := os.ReadFile(transcriptPath) //nolint:gosec // test file
		if err != nil {
			t.Fatal(err)
		}
		var out []transcriptEntry
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var e transcriptEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("bad transcript line %q: %v", line, err)
			}
			out = append(out, e)
		}
		return out
	}

	entries := readTranscript()
	var summary []string
	for _, e := range entries {
		summary = append(summary, fmt.Sprintf("%s:%s:%s", e.Type, e.Tool, e.Content))
	}
	wantSummary := []string{
		// Turn 1: starts with "initial brief", makes tool_call_1, receives
		// "mid-turn first" and "mid-turn second" before tool_call_2 and
		// before Turn 1 ends, and never receives them a second time.
		"USER_INPUT::initial brief",
		"PLANNER_RESPONSE:tool_call_1:",
		"USER_INPUT::mid-turn first",
		"USER_INPUT::mid-turn second",
		"PLANNER_RESPONSE:tool_call_2:",
		"PLANNER_RESPONSE::turn complete: initial brief",
		// Turn 2: delivered via stdin while idle, with no duplicate injection.
		"USER_INPUT::idle second turn",
		"PLANNER_RESPONSE:tool_call_1:",
		"PLANNER_RESPONSE:tool_call_2:",
		"PLANNER_RESPONSE::turn complete: idle second turn",
	}
	if !slices.Equal(summary, wantSummary) {
		t.Fatalf("transcript after two turns =\n  %v\nwant\n  %v", summary, wantSummary)
	}

	// Now test survival across a crash/restart: queue two messages, start a
	// runner whose agy crashes before accepting the first message (so message 1
	// is in Inflight and message 2 is in Messages), then restart the runner and
	// verify both messages are delivered in order, once.
	hold, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox
	if err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "survives restart 1"); err != nil {
		t.Fatal(err)
	}
	if err := Send(inbox, "survives restart 2"); err != nil {
		t.Fatal(err)
	}
	_ = hold.Close()

	_ = os.Remove(signalFile)
	if err := os.WriteFile(resumeSignal, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSession(ctx, "crash-before-accept"); err == nil {
		t.Fatal("crash-before-accept succeeded, want non-zero exit")
	}
	if err := runSession(ctx, "one-turn"); err != nil {
		t.Fatalf("restarted Run failed: %v", err)
	}

	var userInputs []string
	for _, e := range readTranscript() {
		if e.Type == "USER_INPUT" {
			userInputs = append(userInputs, e.Content)
		}
	}
	wantInputs := []string{
		"initial brief",
		"mid-turn first",
		"mid-turn second",
		"idle second turn",
		"survives restart 1",
		"survives restart 2",
	}
	if !slices.Equal(userInputs, wantInputs) {
		t.Fatalf("user inputs across restart = %q, want %q", userInputs, wantInputs)
	}
}
