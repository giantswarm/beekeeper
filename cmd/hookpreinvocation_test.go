package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestWakeAgyDeliversMidTurnViaPreInvocationHook(t *testing.T) {
	a, out := stubApp(t)
	const (
		startID = "1234abcd-0000-4000-8000-000000000010"
		convID  = "conv-1234abcd"
		name    = "test: agy worker"
	)
	rosterAgent(t, a, state.Agent{
		Party: state.Party{
			HostSession: agy.HostPrefix + startID,
			Name:        name,
		},
		Task: "ship preinvocation",
	})

	inbox := agy.InboxPath(a.cfg.StateDir, startID)
	if err := agy.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	// With no reader holding the inbox FIFO open, wake refuses.
	err := a.wakeAgent(t.Context(), state.Party{Name: supervisorName}, name, "too early", "")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "its agy agent no longer runs (beekeeper-agy-1234abcd ended)") {
		t.Fatalf("wake of stopped agy agent = %v, want refusal naming unit", err)
	}

	// Open the FIFO as the running agy-run process does, and link convID.
	reader, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := os.WriteFile(agy.ConvLinkPath(a.cfg.StateDir, convID), []byte(inbox+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Send two wake messages while a turn is in progress.
	out.Reset()
	if err := a.wakeAgent(t.Context(), state.Party{Name: supervisorName}, name, "first mid-turn wake", ""); err != nil {
		t.Fatalf("first wakeAgent: %v", err)
	}
	if !strings.Contains(out.String(), "runs (agy): written to its inbox, it runs the message before its next model call or as its next turn") {
		t.Fatalf("wake output = %q", out.String())
	}
	if err := a.wakeAgent(t.Context(), state.Party{Name: supervisorName}, name, "second mid-turn wake", ""); err != nil {
		t.Fatalf("second wakeAgent: %v", err)
	}

	// Run `beekeeper hook preinvocation` with the conversationId on stdin.
	out.Reset()
	cmd := a.preInvocationCmd()
	cmd.SetIn(strings.NewReader(`{"conversationId":"` + convID + `","invocationNum":2}`))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("preInvocationCmd: %v", err)
	}
	hookOut := out.Bytes()
	if len(hookOut) == 0 {
		t.Fatal("preInvocationCmd wrote nothing, want injected steps")
	}
	var res agy.PreInvocationResult
	if err := json.Unmarshal(bytes.TrimSpace(hookOut), &res); err != nil {
		t.Fatalf("unmarshal hook output %q: %v", hookOut, err)
	}
	want := []agy.InjectedStep{
		{UserMessage: "first mid-turn wake"},
		{UserMessage: "second mid-turn wake"},
	}
	if !slices.Equal(res.InjectSteps, want) {
		t.Fatalf("InjectSteps = %+v, want %+v", res.InjectSteps, want)
	}

	// If agy ignored the hook's output (e.g. a hook timeout), the next
	// PreInvocation call re-injects the unconfirmed messages rather than
	// losing them.
	out.Reset()
	cmdRetry := a.preInvocationCmd()
	cmdRetry.SetIn(strings.NewReader(`{"conversationId":"` + convID + `","invocationNum":3}`))
	if err := cmdRetry.Execute(); err != nil {
		t.Fatalf("retry preInvocationCmd: %v", err)
	}
	var retryRes agy.PreInvocationResult
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &retryRes); err != nil || !slices.Equal(retryRes.InjectSteps, want) {
		t.Fatalf("unconfirmed retry = %+v (%v), want %+v", retryRes.InjectSteps, err, want)
	}

	// Once the stream confirms the two injected user_input steps, a
	// subsequent PreInvocation call injects nothing.
	for range want {
		if err := agy.Confirm(inbox); err != nil {
			t.Fatalf("Confirm: %v", err)
		}
	}
	out.Reset()
	cmd2 := a.preInvocationCmd()
	cmd2.SetIn(strings.NewReader(`{"conversationId":"` + convID + `","invocationNum":4}`))
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("second preInvocationCmd: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("second preInvocationCmd = %q, want empty", out.String())
	}
}
