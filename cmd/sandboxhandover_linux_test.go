//go:build linux

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
)

const (
	handoverMalformedMode = "malformed"
	handoverExpiredMode   = "expired"
)

// Each replacement gets an inherited anonymous fixture, never a live session.
// The notification socket is absent to pin serving through cleanup failures.
func TestKeepVaultRevokedHandoverKeepsServing(t *testing.T) {
	if mode := os.Getenv("TEST_KEEP_VAULT_HANDOVER"); mode != "" {
		t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
		t.Setenv("NOTIFY_SOCKET", "")
		a := vaultApp(t, time.Second)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		v, err := a.keepVault(ctx)
		if mode == handoverMalformedMode {
			if err == nil {
				t.Fatal("malformed nonempty handover accepted")
			}
			return
		}
		if err != nil {
			t.Fatal("revoked snapshot stopped broker startup", err)
		}
		if v.k.State().Unlocked {
			t.Fatal("revoked snapshot unlocked broker")
		}
		reply, err := brokeredVaultState(v)(ctx, os.Getpid(), sandbox.Request{})
		if err != nil || reply.Code != 0 {
			t.Fatal("locked broker cannot serve status")
		}
		path, err := secret.StatePath()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // keeper state metadata in the test runtime directory, never session values
		if err != nil {
			t.Fatal(err)
		}
		var st secret.VaultState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if st.Unlocked || st.Error == "" {
			t.Fatal("locked handover reason was not recorded")
		}
		return
	}
	for _, mode := range []string{"truncated", handoverExpiredMode, handoverMalformedMode} {
		t.Run(mode, func(t *testing.T) {
			fd, err := unix.MemfdCreate("fake-vault", unix.MFD_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			f := os.NewFile(uintptr(fd), "fake-vault")
			defer func() { _ = f.Close() }()
			switch mode {
			case handoverExpiredMode:
				h := map[string]any{"Env": testVaultSession + "=" + testVaultToken, "Since": time.Now().Add(-time.Hour), "Until": time.Now().Add(-time.Second)}
				if err := json.NewEncoder(f).Encode(h); err != nil {
					t.Fatal(err)
				}
			case handoverMalformedMode:
				if _, err := f.WriteString("malformed fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			for attempt := range 2 {
				childMode := mode
				if attempt > 0 {
					childMode = "truncated"
				}
				child := exec.Command(os.Args[0], "-test.run=^TestKeepVaultRevokedHandoverKeepsServing$") //nolint:gosec // this test executable
				child.Env = append(os.Environ(), "TEST_KEEP_VAULT_HANDOVER="+childMode, "LISTEN_FDS=1", "LISTEN_FDNAMES=beekeeper-vault")
				child.ExtraFiles = []*os.File{f}
				if out, err := child.CombinedOutput(); err != nil {
					t.Fatalf("replacement failed: %v: %s", err, out)
				}
				st, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if st.Size() != 0 {
					t.Fatal("replacement retained readable snapshot bytes")
				}
			}
		})
	}
}
