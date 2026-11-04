package systemd_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/contrib/systemd"
	"github.com/giantswarm/beekeeper/internal/config"
)

func TestTeleportServiceAllowsConfiguredLoginBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		attempts int
	}{
		{"long login", 20 * time.Minute, 1},
		{"callback retry", 10 * time.Minute, 2},
		{"long login with callback retry", time.Hour, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			// A proxy enables validation of the Teleport configuration.
			data := fmt.Sprintf("teleport:\n  proxy: test-proxy\n  loginTimeout: %s\n", tc.timeout)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("supported login timeout rejected: %v", err)
			}
			if cfg.Teleport.LoginTimeout.Duration != tc.timeout {
				t.Fatalf("login timeout = %s, want %s", cfg.Teleport.LoginTimeout.Duration, tc.timeout)
			}
			budget := time.Duration(tc.attempts) * cfg.Teleport.LoginTimeout.Duration
			// A oneshot remains in its start phase for the entire renewal.
			// Explicit infinity lets the per-attempt deadlines govern, with
			// time for retry, cleanup and recording the outcome as well.
			section := ""
			settings := make(map[string]string)
			for _, line := range strings.Split(systemd.TeleportService, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") {
					section = line
				}
				if section == "[Service]" {
					if key, value, ok := strings.Cut(line, "="); ok {
						settings[strings.TrimSpace(key)] = strings.TrimSpace(value)
					}
				}
			}
			if settings["Type"] != "oneshot" {
				t.Fatalf("service type = %q, want oneshot", settings["Type"])
			}
			if settings["TimeoutStartSec"] != "infinity" {
				t.Errorf("TimeoutStartSec = %q, want infinity to allow the %s login budget (%d attempts) and cleanup", settings["TimeoutStartSec"], budget, tc.attempts)
			}
		})
	}
}
