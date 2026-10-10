package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testSession and testToken are a made-up session; no op ever issued it.
const (
	testSession = "OP_SESSION_TESTACCOUNT"
	testToken   = "tok"
	// testValue is what a store hands the keeper.
	testValue = "s3cret"
	// hmacOne is the fingerprint the fake stores answer with.
	hmacOne = "hmac:1"
)

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
	if err := k.Unlock(testSession, testToken, time.Now()); err != nil {
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
		if err != nil || name != testSession || token != testToken {
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
	go func() { _ = ServeVault(ctx, path, k, nil, nil) }()
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
	if st, err = AskVault(path, VaultRequest{Op: VaultUnlock, Name: testSession, Token: testToken}); err != nil || !st.Unlocked {
		t.Fatalf("unlock: %+v, %v", st, err)
	}
	if k.Env() != testSession+"=tok" {
		t.Error("the keeper did not take the session")
	}
	if _, err = AskVault(path, VaultRequest{Op: "read"}); err == nil {
		t.Error("an unknown request was answered")
	}
	if _, err = AskVault(path, VaultRequest{Op: VaultStore, Ref: "op://Shared/app/x", Value: "v"}); err == nil || !strings.Contains(err.Error(), "stores nothing") {
		t.Errorf("a store without a storer: %v", err)
	}
	if st, err = AskVault(path, VaultRequest{Op: VaultLock}); err != nil || st.Unlocked || k.Env() != "" {
		t.Errorf("lock: %+v, %v", st, err)
	}
}

// A store over the socket reaches the storer with the keeper's session and
// answers what it wrote, never the value, which a failure's message loses
// too; a locked keeper refuses it.
func TestVaultSocketStores(t *testing.T) {
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
	var got []string
	store := func(_ context.Context, env, ref, value string) (Stored, error) {
		got = append(got, env, ref, value)
		if strings.Contains(ref, "bad") {
			return Stored{}, errors.New("op refused " + value)
		}
		return Stored{Ref: ref, Bytes: len(value), Fingerprint: hmacOne}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ServeVault(ctx, path, k, store, nil) }()
	for range 100 {
		if _, err = AskVault(path, VaultRequest{Op: VaultStatus}); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	req := VaultRequest{Op: VaultStore, Ref: "op://Shared/app/client-secret", Value: testValue}
	if _, err := AskVault(path, req); err == nil || !strings.Contains(err.Error(), "holds no session") || len(got) > 0 {
		t.Errorf("a store while locked: %v, storer got %q", err, got)
	}
	if _, err := AskVault(path, VaultRequest{Op: VaultUnlock, Name: testSession, Token: testToken}); err != nil {
		t.Fatal(err)
	}
	st, err := AskVault(path, req)
	if err != nil || st.Stored == nil || *st.Stored != (Stored{Ref: req.Ref, Bytes: 6, Fingerprint: hmacOne}) {
		t.Fatalf("store: %+v, %v", st, err)
	}
	if want := []string{testSession + "=" + testToken, req.Ref, testValue}; !slices.Equal(got, want) {
		t.Errorf("the storer got %q, want %q", got, want)
	}
	if _, err := AskVault(path, VaultRequest{Op: VaultStore, Ref: "op://Shared/bad/x", Value: testValue}); err == nil || err.Error() != "op refused [value]" {
		t.Errorf("a failed store: %v", err)
	}
	big := VaultRequest{Op: VaultStore, Ref: "op://Shared/app/private-key", Value: strings.Repeat("\n", MaxStoreBytes)}
	if st, err := AskVault(path, big); err != nil || st.Stored == nil || st.Stored.Bytes != MaxStoreBytes {
		t.Errorf("a value of MaxStoreBytes: %+v, %v", st, err)
	}
}

// A store into an item over the socket reaches the item storer with the
// keeper's session and every field, and answers what it wrote per field,
// never a value, which a failure's message loses too; a locked keeper and
// a keeper without an item storer refuse it.
func TestVaultSocketStoresItems(t *testing.T) {
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
	var gotEnv, gotItem string
	var gotFields []ItemField
	storeItem := func(_ context.Context, env, item string, fields []ItemField) ([]Stored, error) {
		gotEnv, gotItem, gotFields = env, item, fields
		if strings.Contains(item, "bad") {
			return nil, errors.New("op refused " + fields[0].Value)
		}
		var out []Stored
		for _, f := range fields {
			out = append(out, Stored{Ref: item + "/" + f.Label, Bytes: len(f.Value), Fingerprint: hmacOne})
		}
		return out, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ServeVault(ctx, path, k, nil, storeItem) }()
	for range 100 {
		if _, err = AskVault(path, VaultRequest{Op: VaultStatus}); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := []ItemField{{Label: "app-id", Value: "4242", Plain: true}, {Label: "client-secret", Value: testValue}, {Label: "private-key", Value: strings.Repeat("\n", MaxStoreBytes)}}
	req := VaultRequest{Op: VaultStoreItem, Item: "op://Shared/app", Fields: fields}
	if _, err := AskVault(path, req); err == nil || !strings.Contains(err.Error(), "holds no session") || gotItem != "" {
		t.Errorf("a store while locked: %v, storer got %q", err, gotItem)
	}
	if _, err := AskVault(path, VaultRequest{Op: VaultUnlock, Name: testSession, Token: testToken}); err != nil {
		t.Fatal(err)
	}
	st, err := AskVault(path, req)
	if err != nil || len(st.Fields) != 3 || st.Fields[0] != (Stored{Ref: "op://Shared/app/app-id", Bytes: 4, Fingerprint: hmacOne}) || st.Fields[2].Bytes != MaxStoreBytes {
		t.Fatalf("store-item: %+v, %v", st, err)
	}
	if gotEnv != testSession+"="+testToken || gotItem != req.Item || !slices.Equal(gotFields, fields) {
		t.Errorf("the storer got %q %q %+v", gotEnv, gotItem, gotFields)
	}
	if _, err := AskVault(path, VaultRequest{Op: VaultStoreItem, Item: "op://Shared/bad", Fields: fields[1:]}); err == nil || err.Error() != "op refused [value]" {
		t.Errorf("a failed store-item: %v", err)
	}
	if _, err := AskVault(path, VaultRequest{Op: VaultStore, Ref: "op://Shared/app/x", Value: "v"}); err == nil || !strings.Contains(err.Error(), "stores nothing") {
		t.Errorf("a store without a storer: %v", err)
	}
}

func TestKeeperLifetime(t *testing.T) {
	const lifetime = 50 * time.Millisecond
	changes := make(chan VaultState, 4)
	k := NewKeeper(lifetime, func(st VaultState) { changes <- st })
	now := time.Now()
	if err := k.Unlock(testSession, testToken, now); err != nil {
		t.Fatal(err)
	}
	if st := k.State(); !st.Unlocked || !st.Since.Equal(now) || !st.Until.Equal(now.Add(lifetime)) {
		t.Fatalf("state after the unlock: %+v", st)
	}
	var states []VaultState
	for range 2 {
		select {
		case st := <-changes:
			states = append(states, st)
		case <-time.After(5 * time.Second):
			t.Fatalf("no lock at the end of the lifetime: %+v", k.State())
		}
	}
	if locked := time.Since(now); locked < lifetime || locked > lifetime+time.Second {
		t.Errorf("locked %s after the unlock, want %s", locked, lifetime)
	}
	if k.State().Unlocked || k.Env() != "" || !states[0].Unlocked || states[1].Unlocked {
		t.Fatalf("after the lifetime: %+v, changes %+v", k.State(), states)
	}

	// a session that came after keeps its own lifetime
	k = NewKeeper(time.Hour, nil)
	_ = k.Unlock(testSession, "old", now)
	_ = k.Unlock(testSession, "new", now.Add(time.Minute))
	k.lockSession(now)
	if k.Env() != testSession+"=new" {
		t.Error("the end of an earlier session's lifetime locked a later one")
	}
	if st, err := ReadState(filepath.Join(t.TempDir(), "absent.json")); err != nil || st.Unlocked {
		t.Errorf("an absent state: %+v, %v", st, err)
	}
	path := filepath.Join(t.TempDir(), "d", "vault-state.json")
	if err := WriteState(path, states[0]); err != nil {
		t.Fatal(err)
	}
	if st, err := ReadState(path); err != nil || !st.Until.Equal(now.Add(lifetime)) {
		t.Errorf("state round trip: %+v, %v", st, err)
	}
	raw, _ := os.ReadFile(path) //nolint:gosec // the test's own file
	if strings.Contains(string(raw), testToken) {
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
