package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != "/state/beekeeper" || c.LeaseDir != "/state/beekeeper/leases" || c.GitHub.Floor != 2500 ||
		c.Watch.Interval.Duration != 30*time.Second || c.Watch.LoadMax != 45 {
		t.Errorf("defaults = %+v", c)
	}
	if !c.IsLeasable(Browser) || c.IsLeasable("agentlab-1") {
		t.Error("only the browser is leasable without resources")
	}
}

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `resources: [agentlab-1, gazelle]
grantTTL: 10m
github: {floor: 3000}
watch: {interval: 1m}
checks:
  - name: alerts
    watch: [python3, alerts.py, watch]
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLeasable("gazelle") || c.GrantTTL.Duration != 10*time.Minute || c.GitHub.Floor != 3000 ||
		c.Watch.Interval.Duration != time.Minute || c.Checks[0].Every.Duration != 5*time.Minute {
		t.Errorf("config = %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"browser as resource": "resources: [browser]",
		"path as resource":    "resources: [../x]",
		"check without cmd":   "checks: [{name: x}]",
		"bad duration":        "grantTTL: soon",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
