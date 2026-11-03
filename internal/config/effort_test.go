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
	for _, level := range []string{"", "low", "medium", testHighEffort, "xhigh", "max", "typo"} {
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

func TestAgentEffortRules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `agents:
  models:
    - {match: '^scan', model: small}
    - {match: '^scan', kind: audit, effort: xhigh}
    - {match: '^(scan|file)', effort: high}
    - {match: '^scan', effort: low}
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, kind, want string }{
		{"scan deps", "audit", "xhigh"},
		{"scan deps", testWorkerKind, testHighEffort},
		{"file finding", testWorkerKind, testHighEffort},
		{"fix", testWorkerKind, ""},
	} {
		if got := c.Agents.EffortFor(tc.name, tc.kind); got != tc.want {
			t.Errorf("%+v: got %q", tc, got)
		}
	}
	if got := c.Agents.ModelFor("scan deps", "audit"); got != "small" {
		t.Fatalf("effort-only rule masked the model rule: %q", got)
	}
	if err := os.WriteFile(p, []byte(`agents: {models: [{match: '^scan', effort: extreme}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "agents.models[0].effort") {
		t.Fatalf("invalid rule effort: %v", err)
	}
}

const (
	testWorkerKind = "worker"
	testHighEffort = "high"
)
