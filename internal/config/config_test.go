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
		c.Watch.Interval.Duration != 30*time.Second || c.Watch.LoadMax != 45 || c.Supervisor.RelayAt != 400_000 {
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
supervisor: {relayAt: 1.5M}
`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsLeasable("staging") || c.GrantTTL.Duration != 10*time.Minute || c.GitHub.Floor != 3000 ||
		c.Watch.Interval.Duration != time.Minute || c.Supervisor.RelayAt != 1_500_000 {
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
		"bad relayAt":         "supervisor: {relayAt: 400kb}",
		"negative relayAt":    "supervisor: {relayAt: -1}",
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

func TestParseTokens(t *testing.T) {
	for in, want := range map[string]Tokens{"400k": 400_000, "400K": 400_000, "1m": 1_000_000, "0.5M": 500_000, "250000": 250_000, " 80k ": 80_000} {
		if got, err := ParseTokens(in); err != nil || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "k", "0", "-5k", "lots", "4e", "inf", "NaN", "0.5"} {
		if _, err := ParseTokens(in); err == nil {
			t.Errorf("%q accepted", in)
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

func TestMetricsModels(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := "metrics:\n  models:\n    claude-opus-5-5: {input: 1, output: 2, contextWindow: 500000}\n    my-model: {input: 3}\n  runaway: {sameErrorRepeats: -1}\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Metrics
	if got, _ := m.Model("claude-opus-5-5"); got.Input != 1 || got.ContextWindow != 500000 {
		t.Errorf("a configured model replaces the default: %+v", got)
	}
	if got, ok := m.Model("claude-haiku-4-5-20251001"); !ok || got.Input != 1 {
		t.Errorf("a dated snapshot is priced like its model: %+v %v", got, ok)
	}
	for _, unknown := range []string{"claude-opus-5-6", "claude-opus-5-5-fast", "claude"} {
		if _, ok := m.Model(unknown); ok {
			t.Errorf("%s has no price", unknown)
		}
	}
	if _, ok := m.Model("my-model"); !ok {
		t.Error("a configured model is priced")
	}
	if r := m.Runaway; r.SameErrorRepeats != -1 || r.GitHubCallsPerHour != 1000 || r.ContextFill != 0.9 {
		t.Errorf("runaway %+v", r)
	}
}
