package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		attempts int
		limit    int64 // seconds, including time for status reads and cleanup
	}{
		{"default login", 3 * time.Minute, 1, 900},
		{"long login", 20 * time.Minute, 1, 2460},
		{"callback retry", 10 * time.Minute, 2, 1260},
		{"fractional seconds", 20*time.Minute + time.Nanosecond, 1, 2462},
		{"largest duration", time.Duration(1<<63 - 1), 1, 18_446_744_134},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := teleportStartLimit(t, TeleportServiceForLoginTimeout(tc.timeout))
			if limit != tc.limit {
				t.Errorf("unit timeout is %ds, want %ds", limit, tc.limit)
			}
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
			if float64(limit) <= budget.Seconds() {
				t.Errorf("oneshot service stops after %ds, before its %d login attempt(s) can use their %s budget and finish cleanup", limit, calls, budget.Round(time.Second))
			}
		})
	}
}

// Read the seconds/minutes used by the unit. A missing or unbounded limit
// would lose the service's guard against a hung tsh status.
func teleportStartLimit(t *testing.T, unit string) int64 {
	t.Helper()
	for line := range strings.Lines(unit) {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "TimeoutStartSec=")
		if !ok {
			continue
		}
		multiplier := int64(1)
		value, ok := strings.CutSuffix(raw, "s")
		if !ok {
			value, ok = strings.CutSuffix(raw, "min")
			multiplier = 60
		}
		if !ok {
			t.Fatalf("service has no finite timeout in seconds or minutes: %q", raw)
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("invalid service timeout: %q: %v", raw, err)
		}
		return n * multiplier
	}
	t.Fatal("service has no explicit start timeout")
	return 0
}
