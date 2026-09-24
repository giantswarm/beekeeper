package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	issue     = "giantswarm/agent-platform#682"
	agentFour = "Agent four"

	hostFour     = "local_4"
	rollout      = "check the rollout"
	devctlRepo   = "giantswarm/devctl"
	modelManager = "giantswarm/model-manager"
	graveler     = "graveler"
	gazelle      = "gazelle"
	serving      = "serving"
)

var (
	relayNow = time.Date(2026, 9, 25, 2, 0, 0, 0, time.Local)
	four     = state.Party{Session: "s4", HostSession: hostFour, Name: agentFour}
	gone     = state.Party{Session: "s9", HostSession: "local_9", Name: "Continue #37639"}
)

func pendingState() *state.State {
	return &state.State{
		Notes: []state.Note{
			{ID: 1, For: "Timo", Text: "pick a threshold", Due: relayNow.Add(-time.Minute), Default: "the alert stays as is"},
			{ID: 2, Text: "later", Due: relayNow.Add(time.Hour)},
			{ID: 3, Text: "no deadline"},
		},
		Timers: []state.Timer{
			{ID: 1, Due: relayNow.Add(-time.Second), What: rollout},
			{ID: 2, Due: relayNow.Add(time.Hour), What: "not yet"},
		},
		Records: []state.Record{
			{Session: four, Issue: "giantswarm/model-manager#180", Waits: "cluster-manager#104"},
			{Session: gone, Issue: issue, Waits: "a GPU node"},
		},
	}
}

func TestFirePendingReportsEachOnce(t *testing.T) {
	st := pendingState()
	live := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}
	lines, evs := firePending(st, live, relayNow)
	got := strings.Join(lines, "\n")
	if len(lines) != 3 || len(evs) != 3 {
		t.Fatalf("got %d lines, %d events:\n%s", len(lines), len(evs), got)
	}
	for _, want := range []string{
		"NOTE DUE: #1 for Timo", "if unanswered: the alert stays as is",
		"TIMER: #1", rollout,
		`SESSION ENDED: "Continue #37639", which serves ` + issue + ", waiting on a GPU node: re-query " + issue,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lines lack %q:\n%s", want, got)
		}
	}
	if lines, evs := firePending(st, live, relayNow.Add(time.Minute)); len(lines)+len(evs) != 0 {
		t.Errorf("reported again: %v", lines)
	}
	// The ended session runs again (a resumed desktop session): the record
	// is live again without a line, and its next end is a line again.
	resumed := append(live, &claude.Session{ID: "s10", HostID: "local_9", Name: "Continue #37639"})
	if lines, evs := firePending(st, resumed, relayNow); len(lines) != 0 || len(evs) != 1 || !st.Records[1].Ended.IsZero() {
		t.Errorf("resume: %v, %v, %+v", lines, evs, st.Records[1])
	}
	if lines, _ := firePending(st, live, relayNow); len(lines) != 1 {
		t.Errorf("second end: %v", lines)
	}
}

func TestSessionChangesLeavesARecordedEndToItsLine(t *testing.T) {
	var out bytes.Buffer
	w := &watcher{app: &app{out: &out}, records: []state.Record{{Session: four, Issue: issue}}}
	w.sessions = map[string]*claude.Session{
		hostFour:  {ID: "s4", HostID: hostFour, Name: agentFour},
		"local_5": {ID: "s5", HostID: "local_5", Name: "Agent five"},
	}
	w.sessionChanges(nil)
	if got := out.String(); strings.Contains(got, agentFour) || !strings.Contains(got, "Agent five") {
		t.Errorf("SESSIONS ended line:\n%s", got)
	}
}

func TestIssueRef(t *testing.T) {
	for in, want := range map[string]bool{
		issue: true, "giantswarm/giantswarm#37639": true, "teemow/klaus-lab#1": true,
		"#682": false, "agent-platform#682": false, "giantswarm/agent-platform": false, "https://github.com/a/b/issues/1": false,
	} {
		if got := issueRef.MatchString(in); got != want {
			t.Errorf("issueRef(%q) = %v", in, got)
		}
	}
}

func TestPromptHasThePendingStateAndNoLiveValue(t *testing.T) {
	st := pendingState()
	st.Supervisor = &state.Supervisor{Party: state.Party{Session: "s0", Name: "Supervisor run 11"}}
	st.Records[1].Ended = relayNow.Add(-time.Minute)
	st.Agents = []state.Agent{{Party: four, Task: "model-manager#180", AssignedAt: relayNow.Add(-time.Hour)}}
	st.Holds = []state.Hold{
		{Target: "lane:serving", Reason: "L4 round first", By: st.Supervisor.Party},
		{Target: devctlRepo, Reason: "tool window", Except: devctlRepo, Tool: "devctl", ToolFrom: "v8.98.7", ToolRelease: "v8.98.8"},
	}
	st.Merges = []state.Merge{
		{Repo: modelManager, PR: 172, Lane: serving, Phase: state.Waiting, By: four, Joined: relayNow},
		{Repo: "giantswarm/backstage", PR: 900, Lane: "portal", Phase: state.Settling, Release: "v1.94.0", Finished: relayNow},
	}
	var out bytes.Buffer
	a := &app{out: &out, now: relayNow, cfg: &config.Config{
		Resources:  []string{"agentlab-1", graveler},
		Lanes:      []config.Lane{{Name: serving, Installation: gazelle, Repositories: []string{modelManager}}, {Name: "portal", Repositories: []string{"giantswarm/backstage"}}},
		Supervisor: config.Supervisor{Skill: "supervise"},
		Alerts:     config.Alerts{Ignore: []string{"Heartbeat"}, Team: "bumblebee", Collapse: 3, Every: config.Duration{Duration: 5 * time.Minute}},
		StateDir:   t.TempDir(),
	}}
	l := &leaseList{
		Held:   []leaseView{{Holder: lease.Holder{Env: graveler, Name: agentFour, Purpose: "PR dev artifacts", Since: relayNow.Format(time.RFC3339)}, State: holderLive}},
		Free:   []string{"agentlab-1", "browser"},
		Queues: map[string][]state.Grant{graveler: {{Resource: graveler, To: gone, By: st.Supervisor.Party, At: relayNow}}},
	}
	al := &alertsView{
		Every:   "5m",
		Targets: []alerts.Target{{Name: gazelle, Context: "teleport.giantswarm.io-gazelle", Why: "configured"}},
		Ignore:  []string{"Heartbeat"}, Team: "bumblebee", Collapse: 3,
		Baselines: map[string]*alerts.Installation{gazelle: {Reachable: true, Alerts: alerts.Set{
			"a": {Alertname: "AppWithoutTeamAnnotation"}, "b": {Alertname: "AppWithoutTeamAnnotation"}, "c": {Alertname: "ChartOrphanConfigMap"},
		}}},
	}
	if err := a.printPrompt(context.Background(), &view{st: st, raw: []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}}}, l, al); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`Run /supervise: you take over the supervisor's watch from "Supervisor run 11".`,
		"## Scope", "the leases of agentlab-1, graveler, browser; the merge lanes serving, portal; the alerts of gazelle",
		"#1 (for Timo, due", "overdue): pick a threshold; if unanswered: the alert stays as is", "#3: no deadline",
		`#1 (at`, rollout, "not yet",
		`graveler held by "Agent four"`, "PR dev artifacts", `graveler is granted to "Continue #37639"`, "free: agentlab-1, browser",
		"lane:serving, until lifted", "L4 round first", "the release window of devctl",
		`serving (gazelle): held (see Holds); 1. giantswarm/model-manager#172 by "Agent four"`,
		"portal: settling giantswarm/backstage#900 until its release rolls",
		`"Agent four" serves giantswarm/model-manager#180, waiting on cluster-manager#104`,
		`"Continue #37639" has ended; it serves ` + issue + ", waiting on a GPU node: re-query " + issue,
		`"Agent four" on model-manager#180`,
		"gazelle (configured; context teleport.giantswarm.io-gazelle): 3 known: AppWithoutTeamAnnotation ×2, ChartOrphanConfigMap",
		"Ignored alert names: Heartbeat.", "Marked team: bumblebee",
		"## Live values", "`beekeeper sessions`", "`beekeeper lanes`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	for _, live := range []string{"v8.98.7", "v8.98.8", "v1.94.0", "MiB", "GiB", "pid"} {
		if strings.Contains(got, live) {
			t.Errorf("prompt carries the live value %q:\n%s", live, got)
		}
	}
}
