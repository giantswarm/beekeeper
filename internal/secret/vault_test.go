package secret

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSession is a made-up session variable; no op ever issued it.
const testSession = "OP_SESSION_TESTACCOUNT"

func TestKeeperWaitsForTheUnlock(t *testing.T) {
	k := NewKeeper(time.Hour, nil)
	if k.State().Unlocked || k.Env() != "" {
		t.Fatal("a new keeper holds a session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := k.Wait(ctx); err != ErrLocked {
		t.Fatalf("Wait on a locked keeper: %v, want ErrLocked", err)
	}
	done := make(chan error)
	go func() { done <- k.Wait(context.Background()) }()
	if err := k.Unlock(testSession, "tok", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Wait across the unlock: %v", err)
	}
	if got := k.Env(); got != testSession+"=tok" {
		t.Errorf("Env = %q", got)
	}
	k.Lock()
	if k.State().Unlocked || k.Env() != "" {
		t.Error("the lock kept the session")
	}
	for _, bad := range [][2]string{{"PATH", "x"}, {testSession, ""}, {testSession, "a\nb"}} {
		if err := k.Unlock(bad[0], bad[1], time.Now()); err == nil {
			t.Errorf("Unlock(%q, %q): want a refusal", bad[0], bad[1])
		}
	}
}

func TestParseSignin(t *testing.T) {
	for _, out := range []string{
		"export " + testSession + "=\"tok\"\n# This command is meant to be used with your shell's eval function.\n",
		testSession + "=tok\n",
	} {
		name, token, err := ParseSignin([]byte(out))
		if err != nil || name != testSession || token != "tok" {
			t.Errorf("ParseSignin(%q) = %q, %q, %v", out, name, token, err)
		}
	}
	if _, _, err := ParseSignin([]byte("[ERROR] 401: Unauthorized\n")); err == nil {
		t.Error("an error parsed as a session")
	}
}

func TestNeedsVaultAndExpired(t *testing.T) {
	for args, want := range map[string]bool{
		"fingerprint op://Shared/i/f":                          true,
		"set a.sops.yaml p --generate=true --vault=op://S/i/f": true,
		"compare a.sops.yaml b.sops.yaml":                      false,
	} {
		if got := NeedsVault(strings.Fields(args)); got != want {
			t.Errorf("NeedsVault(%q) = %v", args, got)
		}
	}
	if !Expired("op: exit 1 ([ERROR] You are not currently signed in. Please run `op signin --help` for instructions)") || Expired("op: exit 1 (isn't an item)") {
		t.Error("Expired misreads op's errors")
	}
}

// The socket takes the person's session and answers only its state; the
// file is the user's alone.
func TestVaultSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "bkv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	path, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	k := NewKeeper(time.Hour, nil)
	asBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ServeVault(ctx, path, k) }()
	var st VaultState
	for range 100 {
		if st, err = AskVault(path, VaultRequest{Op: VaultStatus}); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || st.Unlocked {
		t.Fatalf("status: %+v, %v", st, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v, %v", fi.Mode(), err)
	}
	if st, err = AskVault(path, VaultRequest{Op: VaultUnlock, Name: testSession, Token: "tok"}); err != nil || !st.Unlocked {
		t.Fatalf("unlock: %+v, %v", st, err)
	}
	if k.Env() != testSession+"=tok" {
		t.Error("the keeper did not take the session")
	}
	if _, err = AskVault(path, VaultRequest{Op: "read"}); err == nil {
		t.Error("an unknown request was answered")
	}
	if st, err = AskVault(path, VaultRequest{Op: VaultLock}); err != nil || st.Unlocked || k.Env() != "" {
		t.Errorf("lock: %+v, %v", st, err)
	}
}

func TestKeeperLifetime(t *testing.T) {
	var states []VaultState
	k := NewKeeper(time.Hour, func(st VaultState) { states = append(states, st) })
	now := time.Date(2026, 10, 5, 22, 10, 0, 0, time.UTC)
	if err := k.Unlock(testSession, "tok", now); err != nil {
		t.Fatal(err)
	}
	if st := k.State(); !st.Unlocked || !st.Since.Equal(now) || !st.Until.Equal(now.Add(time.Hour)) {
		t.Fatalf("state after the unlock: %+v", st)
	}
	if k.Expire(now.Add(59 * time.Minute)) {
		t.Fatal("expired within its lifetime")
	}
	if !k.Expire(now.Add(time.Hour)) || k.State().Unlocked || k.Env() != "" {
		t.Fatal("kept past its lifetime")
	}
	if k.Expire(now.Add(2 * time.Hour)) {
		t.Error("a locked keeper expired again")
	}
	if len(states) != 2 || !states[0].Unlocked || states[1].Unlocked {
		t.Errorf("changes heard: %+v", states)
	}
	if st, err := ReadState(filepath.Join(t.TempDir(), "absent.json")); err != nil || st.Unlocked {
		t.Errorf("an absent state: %+v, %v", st, err)
	}
	path := filepath.Join(t.TempDir(), "d", "vault-state.json")
	if err := WriteState(path, states[0]); err != nil {
		t.Fatal(err)
	}
	if st, err := ReadState(path); err != nil || !st.Until.Equal(now.Add(time.Hour)) {
		t.Errorf("state round trip: %+v, %v", st, err)
	}
	raw, _ := os.ReadFile(path) //nolint:gosec // the test's own file
	if strings.Contains(string(raw), "tok") {
		t.Error("the state file carries the session")
	}
}

func TestWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w", "vault-waits.json")
	if ws, err := ReadWaits(path); err != nil || ws != nil {
		t.Fatalf("no file: %v, %v", ws, err)
	}
	want := []VaultWait{{Who: "Agent one", Ref: "op://Shared/i/f", Since: time.Unix(100, 0).UTC()}}
	if err := WriteWaits(path, want); err != nil {
		t.Fatal(err)
	}
	ws, err := ReadWaits(path)
	if err != nil || len(ws) != 1 || ws[0] != want[0] {
		t.Errorf("ReadWaits = %+v, %v", ws, err)
	}
}
