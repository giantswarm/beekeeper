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
	if len(c.Notify.Kinds) != 6 || c.Notify.Policy().Quiet != nil {
		t.Errorf("notify defaults = %+v", c.Notify)
	}
	if !c.IsLeasable(Browser) || c.IsLeasable("kind-1") {
		t.Error("only the browser is leasable without resources")
	}
}

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	raw := `resources: [kind-1, staging]
grantTTL: 10m
github: {floor: 3000}
watch: {interval: 1m}
alerts:
  installations: [alpha, {name: beta, context: admin@beta, floor: warning}]
  team: bumblebee
  flap: {changes: 3}
notify:
  kinds: [due, oom-kill]
  quietHours: "22:00-07:00"
  urgency: {due: critical}
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLeasable("staging") || c.GrantTTL.Duration != 10*time.Minute || c.GitHub.Floor != 3000 ||
		c.Watch.Interval.Duration != time.Minute {
		t.Errorf("config = %+v", c)
	}
	if n := c.Notify; len(n.Kinds) != 2 || n.Repeat.Duration != 30*time.Minute || n.Policy().Quiet == nil || n.Urgency["due"] != "critical" {
		t.Errorf("notify = %+v", n)
	}
	al := c.Alerts
	if len(al.Installations) != 2 || al.Installations[0] != (Installation{Name: "alpha"}) ||
		al.Installations[1] != (Installation{Name: "beta", Context: "admin@beta", Floor: "warning"}) {
		t.Errorf("installations = %+v", al.Installations)
	}
	if len(al.Ignore) != 3 || al.Collapse != 3 || al.Every.Duration != 5*time.Minute || al.Timeout.Duration != time.Minute ||
		al.Flap.Changes != 3 || al.Flap.Window.Duration != time.Hour {
		t.Errorf("alerts defaults = %+v", al)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"browser as resource": "resources: [browser]",
		"path as resource":    "resources: [../x]",
		"nameless install":    "alerts: {installations: [{context: x}]}",
		"unknown floor":       "alerts: {installations: [{name: x, floor: low}]}",
		"flapping at once":    "alerts: {flap: {changes: 1}}",
		"bad duration":        "grantTTL: soon",
		"skill and file":      "supervisor: {skill: supervise, instructions: /x.md}",
		"unknown notify kind": "notify: {kinds: [due, alerts]}",
		"unknown urgency":     "notify: {urgency: {due: urgent}}",
		"urgency of no kind":  "notify: {urgency: {sessions: low}}",
		"bad quiet hours":     "notify: {quietHours: 22-7}",
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

const (
	backstage = "giantswarm/backstage"
	portal    = "portal-tools"
)

func TestLanes(t *testing.T) {
	c := &Config{Lanes: []Lane{
		{Name: portal, Repositories: []string{backstage, "marge"}, Installation: "gazelle"},
		{Name: "serving", Repositories: []string{"giantswarm/model-manager"}},
	}}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for repo, lane := range map[string]string{
		backstage:                  portal,
		"giantswarm/marge":         portal,
		"giantswarm/model-manager": "serving",
		"giantswarm/devctl":        "giantswarm/devctl",
	} {
		if got := c.LaneOf(repo).Name; got != lane {
			t.Errorf("%s: lane %s, want %s", repo, got, lane)
		}
	}
	c.Lanes = append(c.Lanes, Lane{Name: "dup", Repositories: []string{backstage}})
	if err := c.validate(); err == nil {
		t.Error("a repository in two lanes validates")
	}
}
