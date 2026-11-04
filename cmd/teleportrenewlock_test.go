package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/teleport"
)

const teleportLockTestDir = "BEEKEEPER_TEST_TELEPORT_LOCK_DIR"

func TestTeleportRenewLockContention(t *testing.T) {
	a := teleportApp(t)
	a.as = teleportKeeper
	a.cfg.StateDir = a.store.Dir()
	a.cfg.Teleport.Proxy = "login.example.invalid"
	// Any status or login invocation would fail: contention must return
	// before either, and before reading or writing the keeper's record.
	a.cfg.Teleport.Tsh = filepath.Join(t.TempDir(), "missing-tsh")
	before := teleport.Record{RenewedAt: a.now.UTC(), ValidUntil: a.now.Add(time.Hour).UTC(), CheckedAt: a.now.UTC()}
	if err := a.store.WriteFile(teleportFile, before); err != nil {
		t.Fatal(err)
	}
	dir := lease.Dir(a.cfg.LeaseDir)
	if cur, err := dir.Claim(config.Browser, lease.Holder{Env: config.Browser, Name: a.as, Purpose: teleportPurpose}); err != nil || cur != nil {
		t.Fatalf("claim: %+v, %v", cur, err)
	}
	lock := flock.New(filepath.Join(a.cfg.StateDir, teleportRenewLock))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("hold first renewal: %v, %v", ok, err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
	for _, keeper := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "keeper"}[keeper], func(t *testing.T) {
			var out bytes.Buffer
			other := *a
			other.out = &out
			var err error
			other.store, err = state.Open(a.cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			c := other.teleportRenewCmd()
			c.SetContext(t.Context())
			if err := c.Flags().Set("keeper", map[bool]string{false: "false", true: "true"}[keeper]); err != nil {
				t.Fatal(err)
			}
			err = c.RunE(c, nil)
			if keeper {
				if err != nil || !strings.Contains(out.String(), "already running") {
					t.Fatalf("keeper contention: %v, %q", err, out.String())
				}
			} else if err == nil || !strings.Contains(err.Error(), "already running") {
				t.Fatalf("manual contention: %v", err)
			}
			if rec := mustRecord(t, &other); rec != before {
				t.Fatalf("contention changed the keeper record: %+v", rec)
			}
			if h, err := dir.Get(config.Browser); err != nil || h == nil || h.Name != a.as {
				t.Fatalf("contention released the active browser: %+v, %v", h, err)
			}
		})
	}
	// A separate process must observe the same lock, not just a shared Go
	// object in this process.
	t.Setenv(teleportLockTestDir, a.cfg.StateDir)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTeleportRenewLockProcess$") //nolint:gosec // this test binary, synthetic state directory
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("process contention: %v\n%s", err, out)
	}
}

func TestTeleportRenewLockProcess(t *testing.T) {
	dir := os.Getenv(teleportLockTestDir)
	if dir == "" {
		return
	}
	lock := flock.New(filepath.Join(dir, teleportRenewLock))
	ok, err := lock.TryLock()
	if ok {
		_ = lock.Unlock()
	}
	if err != nil || ok {
		t.Fatalf("other process acquired the renewal lock: %v, %v", ok, err)
	}
}

func TestTeleportRenewLockReleased(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell tsh fixture")
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			a := teleportApp(t)
			a.as = teleportKeeper
			a.cfg.StateDir = a.store.Dir()
			a.cfg.Teleport.Proxy = "login.example.invalid"
			a.cfg.Teleport.Home = filepath.Join(t.TempDir(), "home")
			a.cfg.Teleport.LoginTimeout.Duration = time.Second
			a.cfg.Teleport.Tsh = filepath.Join(t.TempDir(), "tsh")
			status := `{"active":{"valid_until":"` + a.now.Add(12*time.Hour).Format(time.RFC3339) + `"}}`
			login := "exit 0"
			if fail {
				login = "exit 1"
			}
			script := "#!/bin/sh\ncase \"$1\" in\nstatus) printf '%s\\n' '" + status + "';;\nlogin) " + login + ";;\n*) exit 2;;\nesac\n"
			if err := os.WriteFile(a.cfg.Teleport.Tsh, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			c := a.teleportRenewCmd()
			c.SetContext(t.Context())
			if err := c.RunE(c, nil); (err != nil) != fail {
				t.Fatalf("renewal error = %v, want failure = %v", err, fail)
			}
			lock := flock.New(filepath.Join(a.cfg.StateDir, teleportRenewLock))
			ok, err := lock.TryLock()
			if ok {
				_ = lock.Unlock()
			}
			if err != nil || !ok {
				t.Fatalf("completed renewal kept its lock: %v, %v", ok, err)
			}
			if h, err := lease.Dir(a.cfg.LeaseDir).Get(config.Browser); err != nil || h != nil {
				t.Fatalf("completed renewal kept its browser: %+v, %v", h, err)
			}
		})
	}
}
