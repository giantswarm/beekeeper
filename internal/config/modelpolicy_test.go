package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentModelRules(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `agents:
  requireModel: true
  model: fallback
  models:
    - {match: '^scan'}
    - {match: '^fix', model: '  '}
    - {match: '^scan', kind: audit, model: careful}
    - {match: '^scan', model: small}
guide: {model: guide-model}
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, kind, want string }{
		{"scan dependencies", "audit", "careful"},
		{"scan dependencies", "worker", "small"},
		{"fix", "worker", "fallback"},
	} {
		if got := c.Agents.ModelFor(tc.name, tc.kind); got != tc.want {
			t.Errorf("%+v: got %q", tc, got)
		}
	}
	if !c.Agents.RequireModel || c.Guide.Model != "guide-model" {
		t.Fatalf("config did not retain guard and guide model")
	}
	c.Agents.Model = ""
	if got := c.Agents.ModelFor("fix", "worker"); got != "" {
		t.Fatalf("model-free rule invented a model: %q", got)
	}
	for _, bad := range []string{`agents: {models: [{match: '[', model: small}]}`, `agents: {models: [{model: small}]}`, `capacity: {taskPolicies: {audit: {minClass: -1}}}`} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
