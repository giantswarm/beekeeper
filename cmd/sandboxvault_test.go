package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	testVaultSession = "OP_SESSION_TESTACCOUNT"
	testVaultToken   = "tok"
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
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &app{store: store, cfg: &config.Config{Secret: config.Secret{Vault: "Shared", Session: true, UnlockWait: config.Duration{Duration: wait}}}}
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
		return testVaultSession, testVaultToken, nil
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
	// the call that ran no process is logged by the broker, as the requester's
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "secret.") })
	if err != nil || len(evs) != 1 || evs[0].Verb != "secret.fingerprint" || evs[0].By.Name == "" ||
		!strings.HasPrefix(evs[0].Detail, testVaultRef+": failed: the vault stayed locked for 20ms (") {
		t.Errorf("the locked call's log = %+v, %v", evs, err)
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
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox broker runs on Linux only")
	}
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
	_ = k.Unlock(testVaultSession, testVaultToken, time.Now())
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
	_ = k.Unlock(testVaultSession, testVaultToken, now)
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
	name, token, err := runSignin(context.Background(), []string{"sh", "-c", `echo signing in >&2; echo 'export ` + testVaultSession + `="` + testVaultToken + `"'`})
	if err != nil || name != testVaultSession || token != testVaultToken {
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

// A sign-in that fails for a reason that passes by itself (the network) is
// tried again after the backoff, each failure one status for the watch,
// and nothing reaches the person: no note, no final failure.
func TestVaultSigninRetriesATransientFailure(t *testing.T) {
	a := vaultApp(t, time.Second)
	k := secret.NewKeeper(time.Hour, nil)
	var tries atomic.Int32
	var mu sync.Mutex
	var statuses []string
	var gaveUp []secret.SigninCause
	v := &vaultBroker{k: k, wait: 5 * time.Second, backoff: []time.Duration{time.Millisecond},
		signin: func(context.Context) (string, string, error) {
			if tries.Add(1) < 3 {
				return "", "", errors.New("op-unlock: exit status 1: dial tcp: lookup my.1password.com: no such host")
			}
			return testVaultSession, testVaultToken, nil
		},
		retrying: func(s string) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() },
		failed:   func(c secret.SigninCause, _ int, _ error) { mu.Lock(); gaveUp = append(gaveUp, c); mu.Unlock() },
	}
	v.ask(context.Background())
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := k.Wait(wctx); err != nil {
		t.Fatalf("not unlocked after the retries: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tries.Load() != 3 || len(gaveUp) != 0 {
		t.Errorf("%d tries, gave up %v", tries.Load(), gaveUp)
	}
	if len(statuses) != 2 || !strings.Contains(statuses[0], "the network did not reach 1Password (op-unlock: exit status 1: dial tcp: lookup my.1password.com: no such host); try 2 at ") ||
		!strings.Contains(statuses[1], "try 3 at ") {
		t.Errorf("statuses %q", statuses)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Notes) != 0 {
		t.Errorf("notes after a transient failure: %+v, %v", st.Notes, err)
	}
}

// A password 1Password rejects while the credential store answered survives
// the retries and reaches the person: one sign-in note, whose probe is
// beekeeper secret status, filed once; the failure is in the event log.
func TestVaultSigninRejectedReachesThePerson(t *testing.T) {
	a := vaultApp(t, time.Second)
	a.cfg.Guide.Person = personTimo
	k := secret.NewKeeper(time.Hour, nil)
	var tries atomic.Int32
	gaveUp := make(chan secret.SigninCause, 2)
	const exe = "/opt/bee keeper/beekeeper"
	v := &vaultBroker{k: k, wait: 200 * time.Millisecond, backoff: []time.Duration{10 * time.Millisecond},
		store: func(context.Context) (secret.StoreState, error) { return secret.StoreReady, nil },
		signin: func(context.Context) (string, string, error) {
			tries.Add(1)
			return "", "", errors.New("op-unlock: exit status 1: op-unlock: 1Password rejected the password from entry 'x' (rotated? update the entry)")
		},
		failed: func(c secret.SigninCause, n int, err error) {
			a.vaultSigninFailed(exe, c, n, err)
			gaveUp <- c
		},
	}
	gaveUpWith := func() secret.SigninCause {
		t.Helper()
		select {
		case c := <-gaveUp:
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("the sign-in never gave up")
			return secret.SigninUnknown
		}
	}
	v.ask(context.Background())
	if c := gaveUpWith(); c != secret.SigninRejected {
		t.Fatalf("gave up with %s", c)
	}
	if n := tries.Load(); n < 2 {
		t.Errorf("%d tries, want retries before the person hears", n)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Notes) != 1 {
		t.Fatalf("notes %+v, %v", st.Notes, err)
	}
	n := st.Notes[0]
	if n.Kind != noteLogin || n.For != personTimo || n.Until != guard.ShellQuote(exe)+" secret status" || n.By != vaultParty || n.Default == "" ||
		!strings.Contains(n.Text, "1Password rejected the password") || !strings.Contains(n.Text, "rotated? update the entry") {
		t.Errorf("the note: %+v", n)
	}
	// a second sign-in that fails the same way files no second note
	v.ask(context.Background())
	gaveUpWith()
	if st, _ = a.store.Read(); len(st.Notes) != 1 {
		t.Errorf("notes after the second failure: %+v", st.Notes)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "vault.signin" })
	if err != nil || len(evs) != 2 || !strings.HasPrefix(evs[0].Detail, "failed (1Password rejected the password) after ") || !strings.HasSuffix(evs[0].Detail, "(rotated? update the entry)") {
		t.Errorf("the log: %+v, %v", evs, err)
	}
}

// Within the boot grace the keeper waits for the credential store to come up
// before it runs the command, and a locked store is waited for at any time:
// once the store answers unlocked the sign-in runs, once.
func TestVaultSigninWaitsForTheCredentialStore(t *testing.T) {
	for name, tc := range map[string]struct {
		uptime time.Duration
		states []secret.StoreState
	}{
		"not up yet after the boot": {14 * time.Second, []secret.StoreState{secret.StoreAbsent, secret.StoreAbsent, secret.StoreReady}},
		"locked":                    {3 * time.Hour, []secret.StoreState{secret.StoreLocked, secret.StoreReady}},
	} {
		t.Run(name, func(t *testing.T) {
			k := secret.NewKeeper(time.Hour, nil)
			var looks, signins atomic.Int32
			var mu sync.Mutex
			var statuses []string
			v := &vaultBroker{k: k, wait: 5 * time.Second, backoff: []time.Duration{time.Millisecond}, storeEvery: time.Millisecond,
				uptime: func() (time.Duration, error) { return tc.uptime, nil },
				store: func(context.Context) (secret.StoreState, error) {
					return tc.states[min(int(looks.Add(1)), len(tc.states))-1], nil
				},
				signin: func(context.Context) (string, string, error) {
					if int(looks.Load()) != len(tc.states) {
						t.Errorf("signed in after %d looks at the store, want %d", looks.Load(), len(tc.states))
					}
					signins.Add(1)
					return testVaultSession, testVaultToken, nil
				},
				retrying: func(s string) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() },
			}
			v.ask(context.Background())
			wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := k.Wait(wctx); err != nil {
				t.Fatalf("not unlocked: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if signins.Load() != 1 || len(statuses) != 1 || !strings.HasPrefix(statuses[0], "waiting for the credential store: "+tc.states[0].String()+"; looking every ") {
				t.Errorf("%d sign-ins, statuses %q", signins.Load(), statuses)
			}
		})
	}
}

// An absent store after the boot grace holds nothing: it may be no Secret
// Service at all, and the command runs at once.
func TestVaultSigninRunsWithoutASecretServiceAfterTheBoot(t *testing.T) {
	k := secret.NewKeeper(time.Hour, nil)
	var looks atomic.Int32
	v := &vaultBroker{k: k, wait: time.Second, storeEvery: time.Millisecond,
		uptime: func() (time.Duration, error) { return 3 * time.Hour, nil },
		store:  func(context.Context) (secret.StoreState, error) { looks.Add(1); return secret.StoreAbsent, nil },
		signin: func(context.Context) (string, string, error) { return testVaultSession, testVaultToken, nil },
	}
	v.ask(context.Background())
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := k.Wait(wctx); err != nil || looks.Load() != 1 {
		t.Errorf("unlocked %v after %d looks at the store", err, looks.Load())
	}
}

// A store that never comes up within the window is the failure's cause,
// the command never ran, and no note asks the person.
func TestVaultSigninGivesUpOnAStoreThatNeverComes(t *testing.T) {
	a := vaultApp(t, time.Second)
	k := secret.NewKeeper(time.Hour, nil)
	gaveUp := make(chan error, 1)
	var cause secret.SigninCause
	v := &vaultBroker{k: k, wait: 100 * time.Millisecond, storeEvery: time.Millisecond,
		uptime: func() (time.Duration, error) { return 10 * time.Second, nil },
		store:  func(context.Context) (secret.StoreState, error) { return secret.StoreAbsent, nil },
		signin: func(context.Context) (string, string, error) {
			t.Error("signed in without the store")
			return "", "", errors.New("no store")
		},
		failed: func(c secret.SigninCause, n int, err error) {
			cause = c
			a.vaultSigninFailed("/x/beekeeper", c, n, err)
			gaveUp <- err
		},
	}
	v.ask(context.Background())
	select {
	case err := <-gaveUp:
		if cause != secret.SigninStoreAbsent || !strings.HasPrefix(err.Error(), "no credential store answers on the session bus: waited ") {
			t.Errorf("gave up with %s: %v", cause, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sign-in never gave up")
	}
	if st, _ := a.store.Read(); len(st.Notes) != 0 {
		t.Errorf("a note for a store that is not up: %+v", st.Notes)
	}
}

// The watch says a sign-in the broker retries, and its end once it unlocked.
func TestWatchSaysARetriedSignin(t *testing.T) {
	a := vaultApp(t, time.Minute)
	var out bytes.Buffer
	a.out = &out
	w := &watcher{app: a, last: map[string]time.Time{}}
	statePath, _ := secret.StatePath()
	_ = secret.WriteState(statePath, secret.VaultState{Retrying: "the network did not reach 1Password (no such host); try 2 at 14:25:51"})
	w.vaultWaits()
	_ = secret.WriteState(statePath, secret.VaultState{Unlocked: true, Since: time.Now(), Until: time.Now().Add(time.Hour)})
	w.vaultWaits()
	if l := out.String(); !strings.Contains(l, "VAULT SIGN-IN RETRYING: the network did not reach 1Password (no such host); try 2 at 14:25:51") ||
		!strings.Contains(l, "ENDED VAULT SIGN-IN RETRYING") || strings.Contains(l, "SIGN-IN FAILED") {
		t.Errorf("a retried sign-in: %s", l)
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
