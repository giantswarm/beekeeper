package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
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
	kubectlBin   = "kubectl"
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
			{ID: 1, Kind: noteDecision, For: "Timo", Text: "pick a threshold", Due: relayNow.Add(-time.Minute), Default: "the alert stays as is"},
			{ID: 2, Kind: noteDecision, Text: "later", Due: relayNow.Add(time.Hour)},
			{ID: 3, Kind: noteDecision, Text: "no deadline"},
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
	w := &watcher{app: &app{out: &out, cfg: &config.Config{}}, records: []state.Record{{Session: four, Issue: issue}}}
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
