package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretMoveVaults(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"allowed", `secret: {vault: Personal, moveVaults: [Team Shared]}`, true},
		{"unset", `secret: {vault: Personal}`, true},
		{"empty", `secret: {vault: Personal, moveVaults: [""]}`, false},
		{"path", `secret: {vault: Personal, moveVaults: [Team/item]}`, false},
		{"no source", `secret: {moveVaults: [Team]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "allowed" && (len(cfg.Secret.MoveVaults) != 1 || cfg.Secret.MoveVaults[0] != "Team Shared") {
					t.Fatal("move destination not loaded")
				}
			} else if err == nil || !strings.Contains(err.Error(), "secret.moveVaults") {
				t.Fatalf("expected moveVaults refusal: %v", err)
			}
		})
	}
}
