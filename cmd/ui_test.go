package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Fixture names of the collector test.
const (
	uiSupName   = "Supervisor Sam"
	uiAgentName = "Agent nine"
	uiRepo      = "giantswarm/app"
	uiIssue     = "giantswarm/app#1"
	uiLab1      = "kind-1"
	uiLab2      = "kind-2"
	uiPurpose   = "e2e"
	uiPerson    = "teemow@lab"
	uiNoteText  = "merge the bump?"
)

// The screen's collector maps the state, the leases and the alerts
// baseline into one refresh without touching the network: the budget
// probe and the upgrade read are off.
func TestUICollectorReadsTheMachineState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "stateDir: " + dir + "\nleaseDir: " + filepath.Join(dir, "leases") +
		"\nresources: [kind-1, kind-2]\nlanes: [{name: ap, repositories: [giantswarm/app], installation: acme}]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sup := state.Party{Session: "gone-session", Name: uiSupName}
	since := now.Add(-2 * time.Hour)
	agent := state.Party{Session: "gone-agent", Name: uiAgentName}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: sup, Since: since}
		st.SupervisorCLI = &state.CLI{Supervisor: sup, Since: since, Gone: now.Add(-2 * time.Hour)}
		st.Holds = []state.Hold{{Target: uiRepo, Reason: "the release window", By: sup, At: now.Add(-time.Hour)}}
		st.Notes = []state.Note{{ID: 7, For: pat, Text: uiNoteText, Due: now.Add(-30 * time.Minute), By: sup, At: now.Add(-time.Hour)}}
		st.Timers = []state.Timer{{ID: 3, Due: now.Add(-3 * time.Minute), What: "check", By: sup, At: now.Add(-time.Hour)}}
		st.Agents = []state.Agent{{Party: agent, Registered: now.Add(-2 * time.Hour), Task: "count", IdleSince: now.Add(-time.Hour)}}
		st.Records = []state.Record{{Session: agent, Issue: uiIssue, Waits: "CI", By: sup, At: now}}
		st.Merges = []state.Merge{{Repo: uiRepo, PR: 1, Lane: "ap", By: sup,
			Phase: state.Waiting, Joined: now.Add(-10 * time.Minute), Seen: now.Add(-10 * time.Minute)}}
		st.Budget = &state.Budget{Remaining: 4100, Limit: 5000, Reset: now.Add(3 * time.Hour), At: now.Add(-10 * time.Minute)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// One lease held by a gone session, one by a person.
	for res, raw := range map[string]string{
		uiLab1: `{"env":"kind-1","holder":"teemow@lab","session":"gone-session","name":"gone-session","purpose":"e2e","since":"2026-09-26T09:00:00Z"}`,
		uiLab2: `{"env":"kind-2","holder":"teemow@lab","purpose":"by hand","since":"2026-09-26T10:00:00Z"}`,
	} {
		if err := os.MkdirAll(filepath.Join(cfg.LeaseDir, res), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.LeaseDir, res, "holder.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alerts := `{"installations":{` +
		`"acme":{"reachable":true,"alerts":{"fp1":{"severity":"page","team":"bumblebee",` +
		`"alertname":"KubePodCrashLooping","cluster":"c1","where":"giantswarm/app","since":"1h"}}},` +
		`"quiet":{"reachable":false,"alerts":null}}}`
	if err := os.WriteFile(filepath.Join(dir, "alerts.json"), []byte(alerts), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: cfg, store: store, now: now, out: &out}
	c := newCollector(a)
	c.budgetEvery, c.upgradesEvery = 0, 0

	d, err := c.Data(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.At.IsZero() {
		t.Error("the refresh says nothing about when it was taken")
	}
	if d.Status.Supervisor != uiSupName || d.Status.SupervisorLive || !d.Status.SupervisorGone {
		t.Errorf("supervisor = %+v, want Sam gone", d.Status)
	}
	if d.Status.LeaseCount != 2 || d.Status.HoldCount != 1 || d.Status.Due != 2 {
		t.Errorf("counts = %+v, want 2 leases, 1 hold, 2 due", d.Status)
	}
	var supRole bool
	for _, r := range d.Roles {
		if r.Name == supervisorRole.name {
			supRole = r.Holder == uiSupName && !r.Live && !r.Gone.IsZero()
		}
	}
	if !supRole {
		t.Errorf("the supervisor's role is not read as gone: %+v", d.Roles)
	}
	if len(d.Holds) != 1 || d.Holds[0].Target != uiRepo || d.Holds[0].Reason != "the release window" {
		t.Errorf("holds = %+v", d.Holds)
	}
	if len(d.Notes) != 1 || d.Notes[0].ID != 7 || d.Notes[0].For != pat || d.Notes[0].Text != uiNoteText {
		t.Errorf("notes = %+v", d.Notes)
	}
	if len(d.Timers) != 1 || d.Timers[0].ID != 3 || d.Timers[0].What != "check" {
		t.Errorf("timers = %+v", d.Timers)
	}
	if len(d.Agents) != 1 || d.Agents[0].Name != uiAgentName || d.Agents[0].Task != "count" || d.Agents[0].Reachable != "not running" {
		t.Errorf("agents = %+v", d.Agents)
	}
	if len(d.Records) != 1 || d.Records[0].Session != uiAgentName || d.Records[0].Issue != uiIssue || d.Records[0].Waits != "CI" {
		t.Errorf("records = %+v", d.Records)
	}
	var gone, person bool
	for _, l := range d.Leases.Held {
		switch l.Resource {
		case uiLab1:
			gone = l.State == "gone" && l.Name == "gone-session" && l.Purpose == uiPurpose && !l.Since.IsZero()
		case uiLab2:
			person = l.State == "person" && l.Name == uiPerson
		}
	}
	if !gone || !person || len(d.Leases.Held) != 2 {
		t.Errorf("leases = %+v", d.Leases.Held)
	}
	if !slices.Contains(d.Leases.Free, "browser") || slices.Contains(d.Leases.Free, uiLab1) {
		t.Errorf("free = %v", d.Leases.Free)
	}
	if len(d.Lanes) != 1 || d.Lanes[0].Name != "ap" || d.Lanes[0].Installation != "acme" ||
		len(d.Lanes[0].Waiting) != 1 || d.Lanes[0].Waiting[0].Key != uiIssue {
		t.Errorf("lanes = %+v", d.Lanes)
	}
	if len(d.Alerts) != 2 || d.Alerts[0].Installation != "acme" || !d.Alerts[0].Reachable ||
		len(d.Alerts[0].Alerts) != 1 || d.Alerts[0].Alerts[0].Alertname != "KubePodCrashLooping" ||
		d.Alerts[0].Alerts[0].Severity != "page" || d.Alerts[0].Alerts[0].Team != "bumblebee" ||
		d.Alerts[0].Alerts[0].Cluster != "c1" || d.Alerts[0].Alerts[0].Where != uiRepo ||
		d.Alerts[0].Alerts[0].Since != "1h" || d.Alerts[1].Installation != "quiet" || d.Alerts[1].Reachable {
		t.Errorf("alerts = %+v", d.Alerts)
	}
	if d.Budget.Remaining != 4100 || d.Budget.Limit != 5000 || d.Budget.Floor != 2500 ||
		!d.Budget.At.Equal(now.Add(-10*time.Minute)) || d.Budget.Err != "" || d.Budget.Held {
		t.Errorf("budget = %+v, want the state's reading with no probe error", d.Budget)
	}
	if len(d.Upgrades) != 0 {
		t.Errorf("upgrades = %v with the read off", d.Upgrades)
	}
	if len(d.Events) != 0 {
		t.Errorf("events = %+v with no events.jsonl", d.Events)
	}
	for _, e := range d.Errors {
		for _, want := range []string{"GitHub budget", "upgrades", "alerts baseline", "event log", "leases:"} {
			if strings.HasPrefix(e, want) {
				t.Errorf("Errors says %q; all of those read fine", e)
			}
		}
	}
	if _, err := c.Tail(context.Background(), "no-such-session", 3); err == nil {
		t.Error("Tail of an unknown session did not fail")
	}
}

// The screen is a watching command and says in its help that it reads only.
func TestUICommandIsTheWatchingScreen(t *testing.T) {
	var ui *cobra.Command
	for _, c := range New().Commands() {
		if c.Name() == "ui" {
			ui = c
		}
	}
	if ui == nil {
		t.Fatal("ui is not registered")
	}
	if ui.GroupID != "watching" {
		t.Errorf("ui is in group %q, want watching", ui.GroupID)
	}
	if !strings.Contains(ui.Long, "Reads only") {
		t.Errorf("the help does not say the screen reads only:\n%s", ui.Long)
	}
	if err := ui.Args(ui, []string{"extra"}); err == nil {
		t.Error("ui takes arguments")
	}
}
