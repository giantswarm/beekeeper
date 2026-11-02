package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ompScript = `#!/bin/sh
set -eu
root=$(dirname "$0")
case "$1" in
 --version) echo 'omp v18.14.2'; exit 0;;
 models) if test -e "$root/logged-out"; then echo '{"models":[]}'; else echo '{"models":[{"selector":"fixture/model"}]}'; fi; exit 0;;
esac
test -n "$PI_CODING_AGENT_DIR"
test -z "${OPENAI_API_KEY+x}"
case " $* " in *' --no-extensions '*) ;; *) exit 2;; esac
case " $* " in *' --extension '*) ;; *) exit 2;; esac
case " $* " in *' --tools bash,read,write '*) ;; *) exit 2;; esac
if test -e "$root/extension-error"; then echo '{"type":"extension_error"}'; exit 1; fi
echo '{"type":"ready"}'
echo '{"type":"beekeeper_guard_ready","session_id":"fixture-omp-thread"}'
while IFS= read -r line; do
 printf '%s\n' "$line" >> "$root/prompts"
 id=$(printf '%s' "$line" | sed 's/.*"id":"\([^"]*\)".*/\1/')
 printf '{"type":"response","command":"prompt","id":"%s","success":true}\n' "$id"
 echo '{"type":"agent_start"}'
 echo '{"type":"agent_end"}'
 if test -e "$root/fail-turn"; then status=error; else status=completed; fi
 printf '{"type":"prompt_result","id":"%s","status":"%s"}\n' "$id" "$status"
done
`

func ompFixture(t *testing.T) (fixture, string) {
	t.Helper()
	f := testFixture(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "omp")
	if err := os.WriteFile(bin, []byte(ompScript), 0o700); err != nil {
		t.Fatal(err)
	}
	f.s.Config.Harnesses[HarnessOmp] = HarnessConfig{Executable: bin, Version: "18.14.2", ConfigDir: filepath.Join(dir, "host-login")}
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.Harness, f.r.Model, f.r.PolicyDigest = HarnessOmp, "fixture/model", digest
	return f, dir
}

func TestExecutionOmpPolicyAndReadiness(t *testing.T) {
	f, dir := ompFixture(t)
	ctx := context.Background()
	if _, err := f.s.InstalledPolicy(); err != nil {
		t.Fatal(err)
	}
	caps, err := f.s.Check(ctx)
	if err != nil || caps.Harnesses[HarnessOmp] != "18.14.2" {
		t.Fatalf("capabilities %+v: %v", caps, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logged-out"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	caps, err = f.s.Check(ctx)
	if err != nil || caps.Harnesses[HarnessOmp] != "" || !strings.Contains(caps.Unavailable[HarnessOmp], "login") {
		t.Fatalf("logged out %+v: %v", caps, err)
	}
	if err := os.WriteFile(f.s.ompGuardFile(), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstalledPolicy(); err == nil {
		t.Fatal("accepted changed guard")
	}
}

func TestExecutionOmpRunnerSingleTurn(t *testing.T) {
	f, _ := ompFixture(t)
	ctx := startCodex(t, f)
	if err := f.s.Run(ctx, f.r.Ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.Phase != Exited || !j.Ready || j.InTurn || j.Thread != "fixture-omp-thread" {
		t.Fatalf("journal %+v: %v", j, err)
	}
}

func TestExecutionOmpRefusesMissingGuardBeforePrompt(t *testing.T) {
	f, dir := ompFixture(t)
	ctx := startCodex(t, f)
	if err := os.WriteFile(filepath.Join(dir, "extension-error"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("accepted a failed guard")
	}
	if _, err := os.Stat(filepath.Join(dir, "prompts")); !os.IsNotExist(err) {
		t.Fatal("sent a prompt without guard readiness")
	}
}

func TestExecutionOmpTurnFailureAfterAdmission(t *testing.T) {
	f, dir := ompFixture(t)
	ctx := startCodex(t, f)
	if err := os.WriteFile(filepath.Join(dir, "fail-turn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Run(ctx, f.r.Ref); err == nil {
		t.Fatal("accepted a failed turn")
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.Phase != Failed || !j.Ready || j.InTurn {
		t.Fatalf("failed journal %+v: %v", j, err)
	}
}

func TestExecutionOmpWakesAcknowledgedOnce(t *testing.T) {
	f, dir := ompFixture(t)
	f.r.Messaging = true
	ctx := startCodex(t, f)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx, f.r.Ref) }()
	wait := func(check func(Journal) bool) {
		t.Helper()
		for {
			j, _ := f.s.Read(f.r.Ref)
			if check(j) {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("runner did not reach expected state")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait(func(j Journal) bool { return j.Ready && !j.InTurn })
	m := Envelope{ID: newID(), Sequence: 1, From: f.r.Ref, To: f.r.Ref, Kind: MessageWake, Text: "wake fixture", Sender: Sender{Name: "fixture-sender"}}
	if _, err := f.s.Message(ctx, f.r.Ref, m); err != nil {
		t.Fatal(err)
	}
	wait(func(j Journal) bool { return j.Messages.Entries[m.ID].Stage == Accepted && !j.InTurn })
	if _, err := f.s.Message(ctx, f.r.Ref, m); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	prompts, err := os.ReadFile(filepath.Join(dir, "prompts"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(prompts), "wake fixture") != 1 || !strings.Contains(string(prompts), "fixture-sender") {
		t.Fatalf("delivery lost identity or repeated: %s", prompts)
	}
}
