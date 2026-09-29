package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The team and management cluster of the quiet tests.
const (
	ourTeam = "bumblebee"
	mcName  = "mc-one"
)

// quietApp is an app with the default quiet rules, a store and a lease
// directory.
func quietApp(t *testing.T, out *bytes.Buffer) *app {
	t.Helper()
	cfg := &config.Config{StateDir: t.TempDir(), LeaseDir: t.TempDir(),
		Alerts: config.Alerts{Team: ourTeam, Collapse: 3, Quiet: config.DefaultQuiet},
		Watch:  config.Watch{QuietSessions: config.DefaultQuietSessions}}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return &app{out: out, cfg: cfg, store: store, now: time.Now()}
}

func quietLog(t *testing.T, a *app) []string {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == quietVerb })
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Detail)
	}
	return out
}

// Recorded wake-ups: a test session started and ended within a poll, next
// to a worker's start.
func TestShortLivedSessionsAreQuiet(t *testing.T) {
	var out bytes.Buffer
	a := quietApp(t, &out)
	w := &watcher{app: a}
	w.sessions = map[string]*claude.Session{"local_1": {ID: "s1", HostID: "local_1", Name: "test: wake-target"}}
	w.sessionChanges([]*claude.Session{
		{ID: "s2", HostID: "local_2", Name: "test: beekeeper#54 probe sender 3"},
		{ID: "s3", HostID: "local_3", Name: "Agent eleven"},
	})
	if got := out.String(); !strings.Contains(got, `SESSIONS started: "Agent eleven"`) || strings.Contains(got, "test:") || strings.Contains(got, "ended") {
		t.Errorf("watch lines:\n%s", got)
	}
	log := quietLog(t, a)
	want := []string{
		`SESSIONS started: "test: beekeeper#54 probe sender 3" (quiet: watch.quietSessions)`,
		`SESSIONS ended: "test: wake-target" (quiet: watch.quietSessions)`,
	}
	if strings.Join(log, "\n") != strings.Join(want, "\n") {
		t.Errorf("quiet log = %q, want %q", log, want)
	}
	if n := a.quietSince(time.Now().Add(-time.Hour)); n != 2 {
		t.Errorf("quietSince = %d, want 2", n)
	}
}

// A page on another team's test cluster wakes nobody, the team's own and a
// page on the management cluster do; a lease on the installation puts it in
// play and every alert of it wakes.
func TestAlertLinesHoldBackTestClustersUnlessInPlay(t *testing.T) {
	raw := func(fp, name, severity, team, cluster string) alerts.Raw {
		return alerts.Raw{Fingerprint: fp, StartsAt: "2026-09-29T08:31:00Z",
			Labels: map[string]string{"alertname": name, "severity": severity, "team": team, "cluster_id": cluster}}
	}
	firing := []alerts.Raw{
		raw("a", "IncorrectResourceUsageData", "page", "tenet", "t-ahz0dnsqiqlqt96zmb"),
		raw("b", "MonitoringAgentDown", "page", "atlas", "t-gr5x1yijsdi6vhrh77"),
		raw("c", "AgentPlatformContainerRestartingTooOften", "page", ourTeam, "t-bx7aynmir71j1x8pdg"),
		raw("d", "LoggingAgentMissingOnNode", "page", "atlas", mcName),
	}
	targets := []alerts.Target{{Name: mcName}}
	run := func(a *app) []string {
		st := &alerts.State{Installations: map[string]*alerts.Installation{mcName: {Reachable: true, Alerts: alerts.Set{}}}}
		return a.alertLines(st, targets, []alerts.Answer{{OK: true, Alerts: firing}}, time.Now())
	}

	var out bytes.Buffer
	a := quietApp(t, &out)
	lines := run(a)
	if len(lines) != 2 || !strings.Contains(strings.Join(lines, "\n"), "BUMBLEBEE") || !strings.Contains(strings.Join(lines, "\n"), "LoggingAgentMissingOnNode") {
		t.Errorf("lines = %q, want the team's page and the management cluster's", lines)
	}
	if log := quietLog(t, a); len(log) != 2 || !strings.Contains(log[0], "(quiet: quiet rule cluster t-*)") {
		t.Errorf("quiet log = %q", log)
	}

	a = quietApp(t, &out)
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(mcName, lease.Holder{Env: mcName, Name: "Agent one", Purpose: "e2e"}); err != nil {
		t.Fatal(err)
	}
	if lines := run(a); len(lines) != len(firing) {
		t.Errorf("leased: lines = %q, want every alert", lines)
	}
	if log := quietLog(t, a); len(log) != 0 {
		t.Errorf("leased: quiet log = %q", log)
	}
}
