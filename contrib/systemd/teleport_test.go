package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/teleport"
)

func TestTeleportServiceAllowsLoginBudgets(t *testing.T) {
	// A oneshot stays in its start phase for the whole renewal, including
	// the callback retry. Its outer deadline must allow the login budgets
	// the command actually gives its attempts. No real tsh or clock wait is
	// needed: inspect the deadlines inside synthetic successful renewals.
	var limit time.Duration
	for line := range strings.Lines(TeleportService) {
		if raw, ok := strings.CutPrefix(strings.TrimSpace(line), "TimeoutStartSec="); ok {
			if raw == "infinity" {
				limit = 0
				continue
			}
			var err error
			limit, err = time.ParseDuration(strings.ReplaceAll(raw, "min", "m"))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		attempts int
	}{
		{"long login", 20 * time.Minute, 1},
		{"callback retry", 10 * time.Minute, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Now()
			calls := 0
			var budget time.Duration
			r := teleport.Renewal{
				Home: filepath.Join(root, "profile"), Dir: filepath.Join(root, "renewal"), Timeout: tc.timeout,
				Login: func(ctx context.Context, _ string, log *os.File) error {
					calls++
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("login attempt has no deadline")
					}
					budget += time.Until(deadline)
					if calls < tc.attempts {
						// These are tsh's callback failure markers, with no
						// login URL, profile or credential material.
						if _, err := log.WriteString("identity provider callback failed\nClient.Timeout exceeded while awaiting headers\n"); err != nil {
							t.Fatal(err)
						}
						return errors.New("synthetic callback failure")
					}
					return nil
				},
				Status: func(context.Context, string) (teleport.Profile, error) {
					return teleport.Profile{ValidUntil: now.Add(24 * time.Hour)}, nil
				},
			}
			if _, err := r.Renew(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if calls != tc.attempts {
				t.Fatalf("got %d attempts, want %d", calls, tc.attempts)
			}
			if limit > 0 && limit < budget {
				t.Errorf("oneshot service stops after %s, before its %d login attempt(s) can use their %s budget", limit, calls, budget.Round(time.Second))
			}
		})
	}
}
