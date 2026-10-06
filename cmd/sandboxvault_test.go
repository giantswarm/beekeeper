package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

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
// the watch, and the broker signs in once for every waiting call; they all
// run with the session, and give up with ExitVault after secret.unlockWait
// without it.
func TestBrokeredVaultSignsInOnce(t *testing.T) {
	a := vaultApp(t, 2*time.Second)
	k := secret.NewKeeper(time.Hour, nil)
	var asks atomic.Int32
	approved := make(chan error)
	v := &vaultBroker{k: k, wait: time.Minute, signin: func(context.Context) (string, string, error) {
		asks.Add(1)
		if err := <-approved; err != nil {
			return "", "", err
		}
		return testVaultSession, "tok", nil
	}}
	var mu sync.Mutex
	var envs [][]string
	h := a.brokeredVault(v, func(env []string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			mu.Lock()
			envs = append(envs, env)
			mu.Unlock()
			return sandbox.Reply{Out: "ok"}, nil
		}
	})
	req := sandbox.Request{Op: sandbox.OpSecret, Args: []string{"fingerprint", testVaultRef}}
	done := make(chan sandbox.Reply)
	for range 2 {
		go func() {
			r, _ := h(context.Background(), os.Getpid(), req)
			done <- r
		}()
	}
	path, _ := secret.WaitsPath()
	var ws []secret.VaultWait
	for range 400 {
		if ws, _ = secret.ReadWaits(path); len(ws) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(ws) != 2 || ws[0].Ref != testVaultRef {
		t.Fatalf("waits while locked: %+v", ws)
	}
	approved <- nil
	for range 2 {
		if r := <-done; r.Out != "ok" {
			t.Errorf("after the approval: %+v", r)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("%d sign-ins, want one", n)
	}
	if len(envs) != 2 || envs[0][0] != testVaultSession+"=tok" || envs[1][0] != testVaultSession+"=tok" {
		t.Errorf("env with the session: %q", envs)
	}
	if ws, _ = secret.ReadWaits(path); len(ws) != 0 {
		t.Errorf("waits after the approval: %+v", ws)
	}
	if r, _ := h(context.Background(), os.Getpid(), req); r.Out != "ok" || asks.Load() != 1 {
		t.Errorf("a later call asked again: %+v, %d asks", r, asks.Load())
	}

	k.Lock()
	a.cfg.Secret.UnlockWait.Duration = 20 * time.Millisecond
	go func() { approved <- errors.New("op-unlock: exit status 2") }()
	r, err := h(context.Background(), os.Getpid(), req)
	if err != nil || r.Code != ExitVault || !strings.Contains(r.Err, "vault locked") || !strings.Contains(r.Err, "sign-in") {
		t.Errorf("no sign-in: %+v, %v", r, err)
	}

	// a SOPS-only call never waits on the vault
	envs = append(envs, []string{"x"})
	if r, _ := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpSecret, Args: []string{"compare", "a.sops.yaml", "b.sops.yaml"}}); r.Out != "ok" || envs[len(envs)-1] != nil {
		t.Errorf("SOPS-only: %+v, env %q", r, envs[len(envs)-1])
	}
}

// A session op no longer takes is dropped, signed in again through the
// sign-in, and the call retried once with the new session; the drop shows
// in the state until the new sign-in.
func TestBrokeredVaultSignsInAgainForAnExpiredSession(t *testing.T) {
	const oldToken = "old"
	a := vaultApp(t, time.Second)
	var states []secret.VaultState
	var mu sync.Mutex
	k := secret.NewKeeper(time.Hour, func(st secret.VaultState) { mu.Lock(); states = append(states, st); mu.Unlock() })
	_ = k.Unlock(testVaultSession, oldToken, time.Now())
	var signins atomic.Int32
	v := &vaultBroker{k: k, wait: time.Second, signin: func(context.Context) (string, string, error) {
		signins.Add(1)
		return testVaultSession, "new", nil
	}}
	var envs []string
	h := a.brokeredVault(v, func(env []string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			envs = append(envs, env[0])
			if env[0] == testVaultSession+"="+oldToken {
				return sandbox.Reply{Code: ExitVault, Err: "op: exit 1 ([ERROR] You are not currently signed in)"}, nil
			}
			return sandbox.Reply{Out: "ok"}, nil
		}
	})
	r, _ := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpSecret, Args: []string{fingerprintOp, testVaultRef}})
	if r.Out != "ok" || signins.Load() != 1 || len(envs) != 2 || envs[1] != testVaultSession+"=new" {
		t.Fatalf("after the expiry: %+v, %d sign-ins, envs %q", r, signins.Load(), envs)
	}
	mu.Lock()
	dropped := slices.IndexFunc(states, func(st secret.VaultState) bool {
		return !st.Unlocked && strings.Contains(st.Dropped, "not currently signed in")
	})
	if dropped < 0 || !states[len(states)-1].Unlocked || states[len(states)-1].Dropped != "" {
		t.Errorf("states %+v: a drop, then an unlock that clears it", states)
	}
	mu.Unlock()

	// a session that the second try also finds expired is answered as is
	_ = k.Unlock(testVaultSession, oldToken, time.Now())
	v.signin = func(context.Context) (string, string, error) { return testVaultSession, oldToken, nil }
	envs = nil
	if r, _ := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpSecret, Args: []string{fingerprintOp, testVaultRef}}); r.Code != ExitVault || len(envs) != 2 {
		t.Errorf("expired twice: %+v, %d calls", r, len(envs))
	}
}

// A SOPS file whose age identity is a field of the vault takes the vault on
// the broker as on the client: the call runs with the session, its file
// named relative to the requester's directory. One with a file:// identity
// runs without it.
func TestBrokeredVaultForAnAgeIdentityInTheVault(t *testing.T) {
	for _, e := range []string{"SOPS_AGE_KEY", "SOPS_AGE_KEY_FILE", "SOPS_AGE_KEY_CMD", "SOPS_AGE_SSH_PRIVATE_KEY_FILE"} {
		t.Setenv(e, "")
		if err := os.Unsetenv(e); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	inVault, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	inFile, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, r := range map[string]string{"vault.sops.yaml": inVault.Recipient().String(), "file.sops.yaml": inFile.Recipient().String()} {
		body := "stringData:\n  password: ENC[AES256_GCM,data:x,type:str]\nsops:\n  age:\n    - recipient: " + r + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	a := vaultApp(t, time.Second)
	a.cfg.Secret.AgeIdentities = []config.AgeIdentity{
		{Recipient: inVault.Recipient().String(), Ref: testVaultRef},
		{Recipient: inFile.Recipient().String(), Ref: "file:///nowhere/identity.txt"},
	}
	k := secret.NewKeeper(time.Hour, nil)
	_ = k.Unlock(testVaultSession, "tok", time.Now())
	v := &vaultBroker{k: k, wait: time.Second, signin: func(context.Context) (string, string, error) {
		t.Error("signed in with an unlocked keeper")
		return "", "", errors.New("no sign-in")
	}}
	var env []string
	h := a.brokeredVault(v, func(e []string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			env = e
			return sandbox.Reply{Out: "ok"}, nil
		}
	})
	for file, want := range map[string]bool{"vault.sops.yaml": true, "file.sops.yaml": false} {
		args := []string{"get", file + "#stringData.password"}
		if got := a.secretNeedsVault(dir, args); got != want {
			t.Errorf("%s: secretNeedsVault = %v, want %v", file, got, want)
		}
		env = []string{"unset"}
		if r, err := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpSecret, Args: args}); err != nil || r.Out != "ok" {
			t.Fatalf("%s: %+v, %v", file, r, err)
		}
		if got := len(env) == 1 && env[0] == testVaultSession+"=tok"; got != want {
			t.Errorf("%s: broker env %q, want the session %v", file, env, want)
		}
	}
}

// The keeper touches the session every vaultTouchEvery; one op no longer
// takes is dropped and signed in again at once.
func TestTendVault(t *testing.T) {
	now := time.Date(2026, 10, 5, 22, 10, 0, 0, time.UTC)
	k := secret.NewKeeper(time.Hour, nil)
	_ = k.Unlock(testVaultSession, "tok", now)
	signedIn := make(chan struct{}, 1)
	v := &vaultBroker{k: k, wait: time.Second, signin: func(context.Context) (string, string, error) {
		signedIn <- struct{}{}
		return testVaultSession, "new", nil
	}}
	var touches int
	var gone error
	touch := func(context.Context, string) error { touches++; return gone }
	touched := tendVault(context.Background(), v, now.Add(time.Minute), time.Time{}, touch)
	if touches != 0 || !touched.Equal(now) {
		t.Fatalf("a fresh session was touched: %d, %v", touches, touched)
	}
	touched = tendVault(context.Background(), v, now.Add(vaultTouchEvery), touched, touch)
	if touches != 1 || !touched.Equal(now.Add(vaultTouchEvery)) || !k.State().Unlocked {
		t.Fatalf("the touch: %d, %v, %+v", touches, touched, k.State())
	}
	gone = errors.New("You are not currently signed in")
	tendVault(context.Background(), v, now.Add(2*vaultTouchEvery), touched, touch)
	select {
	case <-signedIn:
	case <-time.After(5 * time.Second):
		t.Fatal("no sign-in after the drop")
	}
	if err := k.Wait(context.Background()); err != nil || k.Env() != testVaultSession+"=new" {
		t.Errorf("after the drop: %v, a new session %v", err, k.Env() == testVaultSession+"=new")
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

// The watch says the approval with its end, each waiting call, and a call
// that gave up as timed out with the vault still locked, never as ENDED.
func TestWatchSaysTheVault(t *testing.T) {
	a := vaultApp(t, time.Minute)
	var out bytes.Buffer
	a.out = &out
	w := &watcher{app: a, last: map[string]time.Time{}}
	statePath, _ := secret.StatePath()
	waitsPath, _ := secret.WaitsPath()
	since := time.Now().Add(-8 * time.Minute)
	if err := secret.WriteWaits(waitsPath, []secret.VaultWait{{Who: "AP 252", Ref: testVaultRef, Since: since}}); err != nil {
		t.Fatal(err)
	}
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "VAULT LOCKED: AP 252 waits on "+testVaultRef) || !strings.Contains(l, "the broker signs in") {
		t.Fatalf("a waiting call: %s", l)
	}
	out.Reset()
	_ = secret.WriteWaits(waitsPath, nil)
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "AP 252's call on "+testVaultRef+" timed out (seen waiting since") || strings.Contains(l, "ENDED") {
		t.Fatalf("a call that gave up: %s", l)
	}
	out.Reset()
	until := time.Now().Add(12 * time.Hour)
	_ = secret.WriteState(statePath, secret.VaultState{Unlocked: true, Since: time.Now(), Until: until})
	_ = secret.WriteWaits(waitsPath, []secret.VaultWait{{Who: "AP 252", Ref: testVaultRef, Since: time.Now()}})
	w.vaultWaits()
	_ = secret.WriteWaits(waitsPath, nil)
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "VAULT UNLOCKED: the broker holds the vault session since") ||
		!strings.Contains(l, "until "+until.Local().Format("15:04")) || !strings.Contains(l, "ENDED VAULT LOCKED") || strings.Contains(l, "timed out") {
		t.Fatalf("an approval: %s", l)
	}
	out.Reset()
	_ = secret.WriteState(statePath, secret.VaultState{})
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "ENDED VAULT UNLOCKED") {
		t.Errorf("the approval's end: %s", l)
	}
}

// The sign-in command's stdout is read into memory as op signin prints it;
// a failure names the command's last stderr line.
func TestRunSignin(t *testing.T) {
	name, token, err := runSignin(context.Background(), []string{"sh", "-c", `echo signing in >&2; echo 'export ` + testVaultSession + `="tok"'`})
	if err != nil || name != testVaultSession || token != "tok" {
		t.Fatalf("runSignin: %q, %q, %v", name, token, err)
	}
	_, _, err = runSignin(context.Background(), []string{"sh", "-c", "echo 'op-unlock: no terminal to ask' >&2; exit 2"})
	if err == nil || !strings.Contains(err.Error(), "no terminal to ask") {
		t.Errorf("a failed sign-in: %v", err)
	}
	if _, _, err = runSignin(context.Background(), []string{"true"}); err == nil {
		t.Error("a sign-in that printed no session was taken")
	}
}

// The watch says a failed sign-in with its reason, and its end.
func TestWatchSaysAFailedSignin(t *testing.T) {
	a := vaultApp(t, time.Minute)
	var out bytes.Buffer
	a.out = &out
	w := &watcher{app: a, last: map[string]time.Time{}}
	statePath, _ := secret.StatePath()
	_ = secret.WriteState(statePath, secret.VaultState{Error: "op-unlock: exit status 2: no terminal to ask"})
	w.vaultWaits()
	_ = secret.WriteState(statePath, secret.VaultState{Unlocked: true, Since: time.Now(), Until: time.Now().Add(time.Hour)})
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "VAULT SIGN-IN FAILED: op-unlock: exit status 2: no terminal to ask") || !strings.Contains(l, "ENDED VAULT SIGN-IN FAILED") {
		t.Errorf("a failed sign-in: %s", l)
	}
}

// The watch says a session op stopped taking while the broker signs in
// again, and its end once the new sign-in unlocked.
func TestWatchSaysADroppedSession(t *testing.T) {
	a := vaultApp(t, time.Minute)
	var out bytes.Buffer
	a.out = &out
	w := &watcher{app: a, last: map[string]time.Time{}}
	statePath, _ := secret.StatePath()
	_ = secret.WriteState(statePath, secret.VaultState{Dropped: "op: exit 1 (You are not currently signed in)", DroppedAt: time.Now()})
	w.vaultWaits()
	_ = secret.WriteState(statePath, secret.VaultState{Unlocked: true, Since: time.Now(), Until: time.Now().Add(time.Hour)})
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "VAULT SESSION DROPPED at ") || !strings.Contains(l, "not currently signed in") || !strings.Contains(l, "ENDED VAULT SESSION DROPPED") {
		t.Errorf("a dropped session: %s", l)
	}
}
