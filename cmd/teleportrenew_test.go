package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/teleport"
	"github.com/gofrs/flock"
)

func renewalCommandApp(t *testing.T) *app {
	t.Helper()
	a := teleportApp(t)
	a.as = teleportKeeper
	a.out = io.Discard
	a.cfg.StateDir = a.store.Dir()
	a.cfg.Teleport.Proxy = "synthetic"
	a.cfg.Teleport.Home = filepath.Join(t.TempDir(), "profile")
	a.cfg.Teleport.Tsh = filepath.Join(t.TempDir(), "tsh")
	a.cfg.Teleport.LoginTimeout.Duration = time.Second * 5
	return a
}

func runRenewal(a *app, keeper bool) error {
	c := a.teleportRenewCmd()
	c.SetContext(context.Background())
	if keeper {
		_ = c.Flags().Set("keeper", "true")
	}
	return c.RunE(c, nil)
}

// A separate process must see the same lock, even with a newly opened store.
func TestTeleportRenewProcessContention(t *testing.T) {
	if dir := os.Getenv("BEEKEEPER_TEST_RENEW_DIR"); dir != "" {
		a := renewalCommandApp(t)
		var err error
		a.store, err = state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		a.cfg.StateDir = dir
		a.cfg.Teleport.Tsh = "must-not-run"
		if err := runRenewal(a, false); err == nil || !strings.Contains(err.Error(), "renewal is in progress") {
			t.Fatalf("manual contention: %v", err)
		}
		if err := runRenewal(a, true); err != nil {
			t.Fatalf("keeper contention: %v", err)
		}
		return
	}
	a := renewalCommandApp(t)
	rec := teleport.Record{Reason: "preserve", CheckedAt: a.now}
	if err := a.store.WriteFile(teleportFile, rec); err != nil {
		t.Fatal(err)
	}
	// Matching browser ownership must not admit a concurrent renewal.
	release, err := a.holdBrowser(state.Party{Name: teleportKeeper})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	before, _ := lease.Dir(a.cfg.LeaseDir).Get(config.Browser)
	lock := flock.New(filepath.Join(a.cfg.StateDir, "teleport-renew.lock"))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer lock.Close()
	// Also exercise a second store in this process.
	other := *a
	other.store, err = state.Open(a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := runRenewal(&other, false); err == nil {
		t.Fatal("concurrent renewal admitted")
	}
	if err := runRenewal(&other, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTeleportRenewProcessContention$")
	c.Env = append(os.Environ(), "BEEKEEPER_TEST_RENEW_DIR="+a.cfg.StateDir)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("subprocess: %v\n%s", err, out)
	}
	if got := mustRecord(t, a); !reflect.DeepEqual(got, rec) {
		t.Fatalf("record changed: %+v", got)
	}
	after, _ := lease.Dir(a.cfg.LeaseDir).Get(config.Browser)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("browser owner changed")
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "teleport.") })
	if err != nil || len(evs) != 0 {
		t.Fatalf("contention recorded renewal events: %v %v", evs, err)
	}
}

func TestTeleportRenewReleasesResources(t *testing.T) {
	for _, fail := range []bool{false, true} {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("failure=%t/owned=%t", fail, owned), func(t *testing.T) {
				a := renewalCommandApp(t)
				script := "#!/bin/sh\nif [ \"$1\" = status ]; then\n echo '{\"active\":{\"cluster\":\"synthetic\",\"username\":\"test\",\"valid_until\":\"2099-01-01T00:00:00Z\"}}'\nelse\n"
				// Run real competing commands from the fake login process while
				// the outer command owns both the operation lock and browser.
				script += fmt.Sprintf("BEEKEEPER_TEST_RENEW_DIR='%s' '%s' -test.run=^TestTeleportRenewProcessContention$ || exit 2\n", a.cfg.StateDir, os.Args[0])
				if fail {
					script += "exit 1\n"
				} else {
					script += "touch \"$TELEPORT_HOME/renewed\"\n"
				}
				script += "fi\n"
				if err := os.WriteFile(a.cfg.Teleport.Tsh, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				if owned {
					for _, key := range []string{"BEEKEEPER_AGY_AGENT", "BEEKEEPER_CODEX_AGENT", "BEEKEEPER_OMP_AGENT", "CLAUDE_CODE_HOST_SESSION_ID"} {
						t.Setenv(key, "")
					}
					t.Setenv("CLAUDE_CODE_SESSION_ID", "renewal-session")
					t.Setenv("CLAUDE_CODE_SESSION_NAME", teleportKeeper)
					a.as = ""
					if _, err := lease.Dir(a.cfg.LeaseDir).Claim(config.Browser, lease.Holder{Env: config.Browser, Session: "renewal-session", Name: teleportKeeper, Purpose: "existing"}); err != nil {
						t.Fatal(err)
					}
				}
				err := runRenewal(a, false)
				if (err != nil) != fail {
					t.Fatalf("renew: %v", err)
				}
				rec := mustRecord(t, a)
				if fail && rec.FailedAt.IsZero() || !fail && rec.RenewedAt.IsZero() {
					t.Fatalf("result not recorded: %+v", rec)
				}
				lock := flock.New(filepath.Join(a.cfg.StateDir, "teleport-renew.lock"))
				defer lock.Close()
				if ok, err := lock.TryLock(); err != nil || !ok {
					t.Fatalf("lock retained: %v %v", ok, err)
				}
				h, err := lease.Dir(a.cfg.LeaseDir).Get(config.Browser)
				if err != nil || (h != nil) != owned {
					t.Fatalf("browser: %+v %v", h, err)
				}
			})
		}
	}
}
