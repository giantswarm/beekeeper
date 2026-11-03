//go:build linux

package secret

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	handoverTestName  = "OP_SESSION_test"
	handoverTestToken = "synthetic-session"
	handoverTestEnv   = handoverTestName + "=" + handoverTestToken
)

// The fake manager retains the descriptor and acknowledges barriers. Only
// synthetic credentials enter this fixture.
func fakeHandoverManager(t *testing.T) <-chan *os.File {
	t.Helper()
	dir := t.TempDir()
	path := "@bk-handover-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	script := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '1\\n'\n"), 0o700); err != nil { //nolint:gosec // executable fake manager in the test's private directory
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	received := make(chan *os.File, 4)
	go func() {
		for {
			b, oob := make([]byte, 256), make([]byte, 256)
			n, on, _, _, err := c.ReadMsgUnix(b, oob)
			if err != nil {
				return
			}
			messages, err := unix.ParseSocketControlMessage(oob[:on])
			if err != nil {
				return
			}
			for _, m := range messages {
				fds, err := unix.ParseUnixRights(&m)
				if err != nil {
					return
				}
				for _, fd := range fds {
					if strings.HasPrefix(string(b[:n]), "FDSTORE=1") {
						received <- os.NewFile(uintptr(fd), "synthetic-handover")
					} else {
						_ = unix.Close(fd)
					}
				}
			}
		}
	}()
	return received
}

func TestVaultHandoverPreservesExpiryAndRevocation(t *testing.T) {
	received := fakeHandoverManager(t)
	k := NewKeeper(time.Hour, nil)
	t.Cleanup(k.Lock)
	since := time.Now().Add(-time.Minute)
	if err := k.Unlock(handoverTestName, handoverTestToken, since); err != nil {
		t.Fatal(err)
	}
	before := k.State()
	if err := k.PrepareHandover(); err != nil {
		t.Fatal(err)
	}
	var f *os.File
	select {
	case f = <-received:
	case <-time.After(time.Second):
		t.Fatal("manager got no descriptor")
	}
	defer func() { _ = f.Close() }()
	next := NewKeeper(24*time.Hour, nil)
	defer next.Lock()
	if err := next.restoreHandover(f, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st := next.State(); !st.Unlocked || !st.Since.Equal(before.Since) || !st.Until.Equal(before.Until) {
		t.Fatal("handover changed the original lifetime")
	}
	if next.Env() != k.Env() {
		t.Fatal("fake vault no longer accepts the inherited session")
	}
	k.Lock()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	revoked := NewKeeper(time.Hour, nil)
	if err := revoked.restoreHandover(f, time.Now()); err == nil || revoked.State().Unlocked {
		t.Fatal("revoked handover unlocked a new broker")
	}
}

func TestVaultHandoverRejectsInvalidOrExpiredMemory(t *testing.T) {
	now := time.Now()
	cases := []vaultHandover{
		{handoverTestEnv, now.Add(-time.Hour), now.Add(-time.Second)},
		{"OTHER=synthetic-session", now, now.Add(time.Hour)},
		{handoverTestEnv, now.Add(time.Hour), now.Add(2 * time.Hour)},
		{"OP_SESSION_test=", now, now.Add(time.Hour)},
	}
	for _, h := range cases {
		raw, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		k := NewKeeper(time.Hour, nil)
		if err := k.restoreHandover(bytes.NewReader(raw), now); err == nil || k.State().Unlocked {
			t.Fatal("invalid handover accepted")
		}
	}
	k := NewKeeper(time.Hour, nil)
	if err := k.restoreHandover(strings.NewReader("malformed"), now); err == nil {
		t.Fatal("malformed handover accepted")
	}
}

func TestVaultHandoverUnavailableFailsClosed(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	k := NewKeeper(time.Hour, nil)
	if err := k.PrepareHandover(); err != nil {
		t.Fatal("locked keeper needs no handover")
	}
	if err := k.Unlock(handoverTestName, handoverTestToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	defer k.Lock()
	if err := k.PrepareHandover(); err == nil {
		t.Fatal("handover without a manager succeeded")
	}
	if !k.State().Unlocked {
		t.Fatal("failed preparation dropped the live session")
	}
}

func TestVaultHandoverInheritedByReplacement(t *testing.T) {
	if os.Getenv("TEST_VAULT_HANDOVER_CHILD") == "1" {
		_ = os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
		k := NewKeeper(24*time.Hour, nil)
		defer k.Lock()
		if err := k.RestoreHandover(); err != nil {
			t.Fatal(err)
		}
		if !k.State().Unlocked {
			t.Fatal("replacement stayed locked")
		}
		called := false
		fakeVault := func(_ context.Context, env, ref, _ string) (Stored, error) {
			called = true
			if env != handoverTestEnv {
				t.Fatal("fake vault refused restored session")
			}
			return Stored{Ref: ref, Bytes: 4}, nil
		}
		if _, msg := storeFor(context.Background(), k, fakeVault, VaultRequest{Ref: "op://test/item/field", Value: "fake"}); msg != "" || !called {
			t.Fatal("restored broker cannot use the fake vault")
		}
		if time.Until(k.State().Until) > time.Hour {
			t.Fatal("restart extended lifetime")
		}
		if os.Getenv("LISTEN_FDS") != "" {
			t.Fatal("activation metadata reaches children")
		}
		return
	}
	received := fakeHandoverManager(t)
	k := NewKeeper(time.Hour, nil)
	defer k.Lock()
	if err := k.Unlock(handoverTestName, handoverTestToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := k.PrepareHandover(); err != nil {
		t.Fatal(err)
	}
	f := receiveHandover(t, received)
	defer func() { _ = f.Close() }()
	child := exec.Command(os.Args[0], "-test.run=^TestVaultHandoverInheritedByReplacement$") //nolint:gosec // this test executable
	child.Env = append(os.Environ(), "TEST_VAULT_HANDOVER_CHILD=1", "LISTEN_FDS=1", "LISTEN_FDNAMES="+handoverName)
	child.ExtraFiles = []*os.File{f}
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("replacement failed: %v: %s", err, out)
	}
}

func TestVaultHandoverInvalidatedWhenSessionChanges(t *testing.T) {
	for _, reason := range []string{"replaced", "dropped", "expired"} {
		t.Run(reason, func(t *testing.T) {
			received := fakeHandoverManager(t)
			k := NewKeeper(time.Hour, nil)
			defer k.Lock()
			if err := k.Unlock(handoverTestName, handoverTestToken, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := k.PrepareHandover(); err != nil {
				t.Fatal(err)
			}
			f := receiveHandover(t, received)
			defer func() { _ = f.Close() }()
			switch reason {
			case "replaced":
				if err := k.Unlock(handoverTestName, "synthetic-new-session", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "dropped":
				k.Drop("fake vault rejected the session", time.Now())
			case "expired":
				k.lockSession(k.State().Since)
			}
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != 0 {
				t.Fatal("old session survived revocation")
			}
		})
	}
}

func TestVaultHandoverManagerRefusalRevokesMemory(t *testing.T) {
	received := fakeHandoverManager(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf '0\\n'\n"), 0o700); err != nil { //nolint:gosec // executable fake manager in the test's private directory
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	k := NewKeeper(time.Hour, nil)
	defer k.Lock()
	if err := k.Unlock(handoverTestName, handoverTestToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := k.PrepareHandover(); err == nil {
		t.Fatal("unconfirmed descriptor accepted")
	}
	f := receiveHandover(t, received)
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Fatal("failed preparation left a usable session in the descriptor store")
	}
	if !k.State().Unlocked {
		t.Fatal("preflight failure dropped the running session")
	}
}

func receiveHandover(t *testing.T, received <-chan *os.File) *os.File {
	t.Helper()
	select {
	case f := <-received:
		return f
	case <-time.After(time.Second):
		t.Fatal("manager received no descriptor")
		return nil
	}
}

func TestVaultHandoverRevokeWipesEvenWhenManagerFails(t *testing.T) {
	received := fakeHandoverManager(t)
	k := NewKeeper(time.Hour, nil)
	defer k.Lock()
	if err := k.Unlock(handoverTestName, handoverTestToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := k.PrepareHandover(); err != nil {
		t.Fatal(err)
	}
	f := receiveHandover(t, received)
	defer func() { _ = f.Close() }()
	t.Setenv("NOTIFY_SOCKET", "")
	if err := k.RevokeHandover(); err == nil {
		t.Fatal("manager failure was not reported")
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		t.Fatal("failed cleanup left readable session bytes")
	}
	if !k.State().Unlocked {
		t.Fatal("revoking snapshot dropped the live session")
	}
}

func TestVaultHandoverLeaseCannotRevokeLaterPreparation(t *testing.T) {
	received := fakeHandoverManager(t)
	k := NewKeeper(time.Hour, nil)
	defer k.Lock()
	if err := k.Unlock(handoverTestName, handoverTestToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := k.PrepareHandover(); err != nil {
		t.Fatal(err)
	}
	old := k.handover
	first := receiveHandover(t, received)
	defer func() { _ = first.Close() }()
	if err := k.PrepareHandover(); err != nil {
		t.Fatal(err)
	}
	current := k.handover
	second := receiveHandover(t, received)
	defer func() { _ = second.Close() }()
	// Simulate the superseded lease firing after another install prepared.
	k.expireHandover(old)
	st, err := second.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("old lease revoked the later snapshot")
	}
	// The current lease expiry wipes bytes but keeps the live vault unlocked.
	k.expireHandover(current)
	st, err = second.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 || !k.State().Unlocked {
		t.Fatal("preparation lease failed to wipe only the snapshot")
	}
}
