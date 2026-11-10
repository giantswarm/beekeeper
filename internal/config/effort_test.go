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

func TestHarnessScopedEffort(t *testing.T) {
	a := Agents{Models: []AgentModelRule{{Match: ".*", Harness: "codex", Effort: testHighEffort}}}
	for _, harness := range []string{"codex", "agy", "claude", ""} {
		want := ""
		if harness == "codex" {
			want = testHighEffort
		}
		if got := a.EffortFor("worker", testWorkerKind, harness); got != want {
			t.Errorf("%s: %q want %q", harness, got, want)
		}
	}
}

func TestCapacityChoiceEffortConfiguration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct {
		harness, effort string
		invalid         bool
	}{
		{"codex", testHighEffort, false}, {"agy", "", false}, {"agy", testHighEffort, true}, {"codex", "typo", true},
	} {
		raw := fmt.Sprintf("capacity:\n  providers:\n    %s: {}\n  choices:\n    - {harness: %s, model: test-model, effort: %q}\n", tc.harness, tc.harness, tc.effort)
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if (err != nil) != tc.invalid {
			t.Fatalf("%+v: %v", tc, err)
		}
		if err == nil && c.Capacity.Choices[0].Effort != tc.effort {
			t.Fatal("choice effort lost")
		}
	}
}
