package cmd

import (
	"bufio"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// events-gazelle-lanes.jsonl are the merge and lease events of the machine's
// event log from 22:28Z to 23:04Z on 2026-09-24, the sessions' ids left out.
func readEvents(t *testing.T) []state.Event {
	t.Helper()
	f, err := os.Open("testdata/events-gazelle-lanes.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []state.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e state.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// machineLanes are the lanes of the machine the events were logged on.
var machineLanes = &config.Config{Lanes: []config.Lane{
	{Name: "portal-tools", Installation: gazelle, Repositories: []string{"giantswarm/giantswarm-platform-manager"}},
	{Name: "agent-platform", Installation: gazelle, Repositories: []string{"giantswarm/agent-platform"}},
	{Name: serving, Installation: gazelle, Repositories: []string{modelManager, "giantswarm/cluster-manager"}},
}}

const (
	lab          = "lab"
	merged672    = `merged giantswarm/agent-platform#672 by "Agent one" at 22:38Z`
	check        = `"Check glean and graveler clean, prepare gazelle (#37850)"`
	merged283    = `merged giantswarm/giantswarm-platform-manager#283 by ` + check + ` at 22:51Z`
	merging283   = `merging giantswarm/giantswarm-platform-manager#283 by ` + check + ` since 22:44Z`
	claimedByOne = `claimed by "Agent one" at 23:00Z`
)

func TestOwnerHintsNameTheLanesMergesAndTheClaims(t *testing.T) {
	events := readEvents(t)
	for _, c := range []struct {
		installation, at string
		want             []string
	}{
		// Only queued and dropped merges: nothing ran into a lane yet.
		{gazelle, "22:33", nil},
		// A running merge and a merged one, newest first; the model-manager
		// merges only queued, the dropped ones and the browser claim naming
		// gazelle in its purpose are none.
		{gazelle, "22:45", []string{merging283, merged672}},
		{gazelle, "22:52", []string{merged283, merged672}},
		// 30 minutes back: #672 merged at 22:38 has left the window.
		{gazelle, "23:10", []string{merged283}},
		{lab, "23:01", []string{claimedByOne}},
		// The beekeeper lane has no installation.
		{"glean", "23:01", nil},
	} {
		now, err := time.Parse(time.RFC3339, "2026-09-24T"+c.at+":00Z")
		if err != nil {
			t.Fatal(err)
		}
		if got := ownerHints(events, machineLanes.LaneOf, c.installation, now); !slices.Equal(got, c.want) {
			t.Errorf("%s at %s:\n got %q\nwant %q", c.installation, c.at, got, c.want)
		}
	}
}

func TestDecorateGivesTheHintsToNewLinesOnly(t *testing.T) {
	asked := 0
	hints := func() []string { asked++; return []string{merged672} }
	lines := decorate([]string{
		"ALERT NEW gazelle notify honeybadger ChartOrphanConfigMap giantswarm/chart-operator since 02:55Z",
		"ALERT RESOLVED gazelle notify atlas MimirRulerTooManyFailedQueries mimir/mimir-ruler since 01:02Z",
		"ALERT NEW gazelle notify atlas MimirContinuousTestFailed mimir/mimir-continuous-test since 03:01Z",
	}, []string{`lease "Agent one"`}, hints)
	want := []string{
		`ALERT NEW gazelle notify honeybadger ChartOrphanConfigMap giantswarm/chart-operator since 02:55Z [lease "Agent one"; ` + merged672 + `]`,
		`ALERT RESOLVED gazelle notify atlas MimirRulerTooManyFailedQueries mimir/mimir-ruler since 01:02Z [lease "Agent one"]`,
		`ALERT NEW gazelle notify atlas MimirContinuousTestFailed mimir/mimir-continuous-test since 03:01Z [lease "Agent one"; ` + merged672 + `]`,
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines:\n got %q\nwant %q", lines, want)
	}
	if asked != 1 {
		t.Errorf("hints asked %d times, want once", asked)
	}
	if got := decorate([]string{"ALERTS gazelle reachable again"}, nil, hints); got[0] != "ALERTS gazelle reachable again" {
		t.Errorf("nobody on it: %q", got[0])
	}
}
