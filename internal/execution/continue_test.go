package execution

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// exitedExecution starts f.r and leaves it as a review park leaves it: first
// task accepted, its input closed and its unit gone.
func exitedExecution(t *testing.T, f fixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Phase, j.Ready, j.Completed, j.Thread = Exited, true, true, fixtureThread
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[UnitName(f.r.Ref)] = unitInactive
}

func TestContinueRunsTheSameUnitAgain(t *testing.T) {
	f := testFixture(t)
	f.r.Messaging = true
	exitedExecution(t, f)
	ctx := context.Background()
	before, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.s.Continue(ctx, f.r.Ref, ContinueRequest{Receipt: Sign(f.r, f.key, f.s.now())})
	if err != nil {
		t.Fatal(err)
	}
	if h != before.Handle || len(f.launcher.starts) != 2 || f.launcher.starts[1].Name != UnitName(f.r.Ref) {
		t.Fatalf("handle %+v, launches %+v", h, f.launcher.starts)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != Activating || j.Completed || !j.continuing() {
		t.Fatalf("journal after continue: phase %s, completed %t, ready %t", j.Phase, j.Completed, j.Ready)
	}
	// A wake stored now waits for the runner instead of failing on the exit.
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "go on"}
	if ack, err := f.s.Message(ctx, f.r.Ref, m); err != nil || ack.Stage != Stored {
		t.Fatalf("wake after continue: %+v %v", ack, err)
	}
}

func TestContinueRefusesWhatMustStayEnded(t *testing.T) {
	stored := Delivery{Stage: Uncertain, Envelope: Envelope{Sequence: 1}}
	for name, test := range map[string]struct {
		change  func(*Journal)
		active  bool
		receipt string
		want    string
	}{
		"stopped":         {change: func(j *Journal) { j.StopRequest = &StopRequest{ID: "stop", Reason: "operator"} }, want: "stopped"},
		"acknowledged":    {change: func(j *Journal) { j.StopAcknowledged = true }, want: "stopped"},
		"retired":         {change: func(j *Journal) { j.Retirement = &Retirement{} }, want: "retired"},
		"never accepted":  {change: func(j *Journal) { j.Ready = false }, want: "first task"},
		"no conversation": {change: func(j *Journal) { j.Request.Harness, j.Thread = HarnessCodex, ""; j.Digest = j.Request.Digest() }, want: "no conversation"},
		"omp":             {change: func(j *Journal) { j.Request.Harness = HarnessOmp; j.Digest = j.Request.Digest() }, want: ErrUnsupported.Error()},
		"blocked delivery": {change: func(j *Journal) {
			j.Messages.Entries = map[string]Delivery{"id": stored}
		}, want: "resolve"},
		"unit active": {active: true, want: "active"},
		"bad receipt": {receipt: "forged", want: "receipt"},
	} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			f.r.Messaging = true
			exitedExecution(t, f)
			ctx := context.Background()
			if test.change != nil {
				if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { test.change(j); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if test.active {
				f.launcher.states[UnitName(f.r.Ref)] = unitActive
			}
			receipt := test.receipt
			if receipt == "" {
				j, err := f.s.Read(f.r.Ref)
				if err != nil {
					t.Fatal(err)
				}
				receipt = Sign(j.Request, f.key, f.s.now())
			}
			_, err := f.s.Continue(ctx, f.r.Ref, ContinueRequest{Receipt: receipt})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("continue = %v, want %q", err, test.want)
			}
			if len(f.launcher.starts) != 1 {
				t.Fatal("refused continuation launched a process")
			}
		})
	}
}

func TestContinueRefusesRulesThatChanged(t *testing.T) {
	f := testFixture(t)
	f.r.Messaging = true
	exitedExecution(t, f)
	ctx := context.Background()
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Policy = Digest([]byte("older rules")); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Continue(ctx, f.r.Ref, ContinueRequest{Receipt: Sign(f.r, f.key, f.s.now())}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("continue = %v", err)
	}
}

func TestContinuedClaudeResumesWithoutItsBrief(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f := testFixture(t)
	f.r.Messaging = true
	dir := t.TempDir()
	child := filepath.Join(dir, "fake-harness")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$(dirname "$0")/args"
session=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--resume' ]; then session="$2"; shift; fi
 shift
done
IFS= read -r line
printf '%s\n' "$line" > "$(dirname "$0")/input"
printf '{"type":"system","subtype":"init","session_id":"%s"}\n' "$session"
printf '%s\n' "$line" | sed 's/}$/,"isReplay":true}/'
`
	if err := os.WriteFile(child, []byte(script), 0o700); err != nil { //nolint:gosec // executable fake harness in the test's own temporary directory
		t.Fatal(err)
	}
	h := f.s.Config.Harnesses[HarnessClaude]
	h.Executable = child
	f.s.Config.Harnesses[HarnessClaude] = h
	original := f.s.Command
	f.s.Command = func(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, error) {
		if bin == child {
			bin = fixtureClaudeExecutable
		}
		return original(ctx, dir, env, bin, args...)
	}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest
	exitedExecution(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.s.Continue(ctx, f.r.Ref, ContinueRequest{Receipt: Sign(f.r, f.key, f.s.now())}); err != nil {
		t.Fatal(err)
	}
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "the review found this"}
	if _, err := f.s.Message(ctx, f.r.Ref, m); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--resume "+f.r.ConversationID) || strings.Contains(string(args), "--session-id") {
		t.Fatalf("harness arguments: %s", args)
	}
	input, err := os.ReadFile(filepath.Join(dir, "input")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(input), m.Text) || strings.Contains(string(input), f.r.Prompt) {
		t.Fatalf("first input of the continuation: %s", input)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != Exited || j.Messages.Entries[m.ID].Stage != Accepted {
		t.Fatalf("after the continuation: phase %s, delivery %s", j.Phase, j.Messages.Entries[m.ID].Stage)
	}
}

func TestContinuedCodexResumesItsThread(t *testing.T) {
	if runtime.GOOS != messageTestLinux {
		t.Skip("execution runner uses a Linux user service")
	}
	f, dir := codexFixture(t)
	f.r.Messaging = true
	exitedExecution(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.s.Continue(ctx, f.r.Ref, ContinueRequest{Receipt: Sign(f.r, f.key, f.s.now())}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "the review found this"}
	if _, err := f.s.Message(ctx, f.r.Ref, m); err != nil {
		t.Fatal(err)
	}
	for {
		j, err := f.s.Read(f.r.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if j.Messages.Sequence == 1 && !j.InTurn {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("runner ended before its wake: %v", err)
		case <-ctx.Done():
			t.Fatal("wake was not accepted")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Completed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if conn, err := (&net.Dialer{}).DialContext(ctx, "unix", f.s.messageSocket(f.r.Ref)); err == nil {
		_ = conn.Close()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test-owned fake harness log
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), f.s.codexHomeDir()+"|") != 1 || !strings.Contains(string(calls), "exec resume") || !strings.Contains(string(calls), fixtureThread) || strings.Contains(string(calls), f.r.Prompt) {
		t.Fatalf("codex turns: %s", calls)
	}
}

func TestOldHostAnswersContinueAsUnsupported(t *testing.T) {
	f := testFixture(t)
	transport := SSHTransport{Host: f.host, Run: func(_ context.Context, _ []string, raw []byte) ([]byte, error) {
		var req WireRequest
		if err := Decode(raw, &req); err != nil {
			return nil, err
		}
		return mustJSON(WireResponse{Protocol: Protocol, RequestID: req.RequestID, Ref: req.Ref, Error: "unknown execution operation"}), nil
	}}
	if _, err := transport.Continue(context.Background(), f.r.Ref, ContinueRequest{Receipt: "any"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("continue on an old host = %v", err)
	}
}
