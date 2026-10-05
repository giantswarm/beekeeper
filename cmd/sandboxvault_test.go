package cmd

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
)

const (
	testVaultSession = "OP_SESSION_TESTACCOUNT"
	testVaultRef     = "op://Shared/i/f"
)

// vaultApp is an app with secret.session and a runtime directory of its own.
func vaultApp(t *testing.T, wait time.Duration) *app {
	t.Helper()
	dir, err := os.MkdirTemp("", "bkv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	return &app{cfg: &config.Config{Secret: config.Secret{Vault: "Shared", Session: true, UnlockWait: config.Duration{Duration: wait}}}}
}

// A call on the vault waits while the broker holds no session, listed for
// the watch, runs with the session once the person unlocks, and gives up
// with ExitVault after secret.unlockWait.
func TestBrokeredVaultWaitsForThePerson(t *testing.T) {
	a := vaultApp(t, 2*time.Second)
	k := secret.NewKeeper()
	var gotEnv []string
	h := a.brokeredVault(k, func(env []string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			gotEnv = env
			return sandbox.Reply{Out: "ok"}, nil
		}
	})
	req := sandbox.Request{Op: sandbox.OpSecret, Args: []string{"fingerprint", testVaultRef}}
	done := make(chan sandbox.Reply)
	go func() {
		r, _ := h(context.Background(), os.Getpid(), req)
		done <- r
	}()
	path, _ := secret.WaitsPath()
	var ws []secret.VaultWait
	for range 200 {
		if ws, _ = secret.ReadWaits(path); len(ws) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(ws) != 1 || ws[0].Ref != testVaultRef {
		t.Fatalf("waits while locked: %+v", ws)
	}
	if err := k.Unlock(testVaultSession, "tok", time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Out != "ok" || len(gotEnv) != 1 || gotEnv[0] != testVaultSession+"=tok" {
		t.Errorf("after the unlock: %+v, env %q", r, gotEnv)
	}
	if ws, _ = secret.ReadWaits(path); len(ws) != 0 {
		t.Errorf("waits after the unlock: %+v", ws)
	}

	k.Lock()
	a.cfg.Secret.UnlockWait.Duration = 20 * time.Millisecond
	r, err := h(context.Background(), os.Getpid(), req)
	if err != nil || r.Code != ExitVault || !strings.Contains(r.Err, "vault locked") {
		t.Errorf("no unlock: %+v, %v", r, err)
	}

	// a SOPS-only call never waits on the vault
	gotEnv = []string{"x"}
	if r, _ := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpSecret, Args: []string{"compare", "a.sops.yaml", "b.sops.yaml"}}); r.Out != "ok" || gotEnv != nil {
		t.Errorf("SOPS-only: %+v, env %q", r, gotEnv)
	}
}

// A session op no longer takes is forgotten, and the call waits for the
// next unlock once.
func TestBrokeredVaultForgetsAnExpiredSession(t *testing.T) {
	a := vaultApp(t, 20*time.Millisecond)
	k := secret.NewKeeper()
	_ = k.Unlock(testVaultSession, "old", time.Now())
	calls := 0
	h := a.brokeredVault(k, func([]string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			calls++
			return sandbox.Reply{Code: ExitVault, Err: "op: exit 1 ([ERROR] You are not currently signed in)"}, nil
		}
	})
	r, _ := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpSecret, Args: []string{"fingerprint", testVaultRef}})
	if calls != 1 || k.Env() != "" || r.Code != ExitVault || !strings.Contains(r.Err, "vault locked") {
		t.Errorf("expired session: %d calls, env %q, %+v", calls, k.Env(), r)
	}
}

// The unlock is the person's: refused in an agent session and without a
// terminal.
func TestUnlockIsThePersons(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	if err := personOnly("beekeeper secret unlock"); err == nil || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Errorf("in an agent session: %v", err)
	}
	for _, k := range agentMarkers {
		t.Setenv(k, "")
	}
	if err := personOnly("beekeeper secret unlock"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("without a terminal: %v", err)
	}
}

func TestStatusLineNamesVaultWaits(t *testing.T) {
	v := &statusView{VaultWaits: []string{agentOne}}
	if l := v.line(); !strings.Contains(l, "vault locked: 1 call waits (Agent one)") {
		t.Errorf("status line: %s", l)
	}
}
