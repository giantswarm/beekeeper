package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEffortRoleConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effort.yaml")
	for _, level := range []string{"", "low", "medium", "high", "xhigh", "max", "typo"} {
		for _, role := range []string{"guide", "supervisor"} {
			if err := os.WriteFile(path, []byte(fmt.Sprintf("%s: {effort: %q}\n", role, level)), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if level == "typo" {
				if err == nil || !strings.Contains(err.Error(), role+".effort") {
					t.Fatalf("invalid %s effort: %v", role, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Guide.Effort
			if role == "supervisor" {
				got = cfg.Supervisor.Effort
			}
			if got != level {
				t.Fatalf("%s.effort=%q want %q", role, got, level)
			}
		}
	}
}
