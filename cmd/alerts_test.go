package cmd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
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
	merged672    = `during merged giantswarm/agent-platform#672 by "Agent one" at 22:38Z`
	check        = `"Check glean and graveler clean, prepare gazelle (#37850)"`
	merged283    = `during merged giantswarm/giantswarm-platform-manager#283 by ` + check + ` at 22:51Z`
	merging283   = `during merging giantswarm/giantswarm-platform-manager#283 by ` + check + ` since 22:44Z`
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
		"ALERT RESOLVED gazelle notify atlas MimirRulerTooManyFailedQueries mimir/mimir-ruler since 01:02Z",
		"ALERT NEW gazelle notify atlas MimirContinuousTestFailed mimir/mimir-continuous-test since 03:01Z",
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

// alerts.kubeconfig is read behind the machine kubeconfig unless it names
// it; none configured leaves kubectl's own default.
func TestAlertKubeconfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	machine, own := filepath.Join(home, ".kube", "config"), "/own"
	for _, c := range []struct {
		files, want []string
	}{
		{nil, nil},
		{[]string{own}, []string{machine, own}},
		{[]string{own, machine}, []string{own, machine}},
	} {
		a := &app{cfg: &config.Config{Alerts: config.Alerts{Kubeconfig: c.files}}}
		if got := a.alertKubeconfig(); !slices.Equal(got, c.want) {
			t.Errorf("alerts.kubeconfig %q: files = %q, want %q", c.files, got, c.want)
		}
	}
}

// A discovered installation is logged into the first file of
// alerts.kubeconfig that is not the machine kubeconfig; none, nowhere.
func TestAlertLoginFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	machine, own := filepath.Join(home, ".kube", "config"), "/own"
	for _, c := range []struct {
		files []string
		want  string
	}{
		{nil, ""},
		{[]string{machine}, ""},
		{[]string{machine, own}, own},
	} {
		a := &app{cfg: &config.Config{Alerts: config.Alerts{Kubeconfig: c.files}}}
		if got := a.alertLoginFile(); got != c.want {
			t.Errorf("alerts.kubeconfig %q: login file %q, want %q", c.files, got, c.want)
		}
	}
}

// The installations a discovery kept join the configured ones, with alerts
// read in the context they resolve to; a configured one is not repeated, and
// nothing joins while discovery is off.
func TestAlertTargetsAddTheDiscoveredInstallations(t *testing.T) {
	const found, foundCtx = "antiz103", "tp-antiz103"
	dir := t.TempDir()
	if err := alerts.SaveDiscovery(dir, &alerts.Discovery{At: time.Now(), Installations: map[string]alerts.Found{
		gazelle: {Context: "tp-" + gazelle, Releases: []string{"muster"}},
		found:   {Context: foundCtx, Releases: []string{"kagent", "substrate"}},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, LeaseDir: filepath.Join(dir, "leases"), Kube: config.Kube{ContextTemplate: "tp-{installation}"},
		Alerts: config.Alerts{Team: ourTeam, Installations: []config.Installation{{Name: gazelle}},
			Discover: config.Discover{Every: config.Duration{Duration: time.Hour}}}}
	a := &app{cfg: cfg}
	contexts := []string{"tp-" + gazelle, foundCtx}
	got := a.alertTargetsIn(contexts)
	want := []alerts.Target{{Name: gazelle, Context: "tp-" + gazelle, Why: alerts.WhyConfigured},
		{Name: found, Context: foundCtx, Why: "carries " + ourTeam + "'s kagent, substrate"}}
	if !slices.Equal(got, want) {
		t.Errorf("targets = %+v, want %+v", got, want)
	}
	cfg.Alerts.Discover.Every.Duration = 0
	if got := a.alertTargetsIn(contexts); len(got) != 1 {
		t.Errorf("discovery off: targets = %+v", got)
	}
}

// The snapshot names the installations without a kube context in one line
// and keeps an unreachable one's own line with its cause.
func TestSnapshotLinesWithoutContext(t *testing.T) {
	targets := []alerts.Target{{Name: gazelle}, {Name: "far-one"}, {Name: mcName}, {Name: "far-two"}}
	answers := []alerts.Answer{
		{OK: true},
		{Why: `error: context "teleport.giantswarm.io-far-one" does not exist`, NoContext: true},
		{Why: "error: connection refused (3 attempts)"},
		{Why: "no kube context for far-two", NoContext: true},
	}
	now := time.Date(2026, 10, 21, 23, 22, 0, 0, time.UTC)
	want := []string{
		gazelle + " at 23:22Z: 0 active",
		mcName + " unreachable: error: connection refused (3 attempts)",
		"2 installations without a kube context, not read: far-one, far-two",
	}
	if got := snapshotLines(alerts.Rules{}, targets, answers, now); !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

// Namespace groups must stay distinct, and JSON must retain labels that the
// text's grouping columns do not name.
func TestAlertSnapshotLabels(t *testing.T) {
	const (
		ignored    = "Heartbeat"
		namespaceA = "snapshot-a"
		namespaceB = "snapshot-b"
	)
	labels := map[string]string{labelAlertname: "ManagementClusterJobFailed", litNamespace: namespaceA, labelSeverity: "notify", labelTeam: "snapshot-team", "route": "jobs", "job_name": "failed-job"}
	other := map[string]string{}
	for k, v := range labels {
		other[k] = v
	}
	other["namespace"] = namespaceB
	hidden := alerts.Raw{Fingerprint: "hidden", Labels: map[string]string{labelAlertname: ignored}}
	silenced := alerts.Raw{Fingerprint: "silenced", Labels: labels}
	silenced.Status.SilencedBy = []string{"silence"}
	targets := []alerts.Target{{Name: lab}, {Name: "snapshot-down"}, {Name: "no-context"}}
	answers := []alerts.Answer{
		{OK: true, Alerts: []alerts.Raw{{Fingerprint: namespaceA, Labels: labels}, {Fingerprint: namespaceB, Labels: other}, hidden, silenced}},
		{Why: "read failed"}, {NoContext: true, Why: "context missing"},
	}
	result := makeAlertSnapshot(alerts.Rules{Ignore: []string{ignored}}, targets, answers, time.Now())
	text := strings.Join(result.Lines, "\n")
	for _, ns := range []string{namespaceA, namespaceB} {
		if !strings.Contains(text, "namespace="+ns) {
			t.Errorf("missing namespace %s: %s", ns, text)
		}
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got alertSnapshotResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Installations) != 3 {
		t.Fatalf("installations: %s", b)
	}
	live := got.Installations[0]
	if !live.Reachable || len(live.Alerts) != 2 {
		t.Fatalf("visible alerts: %s", b)
	}
	if !reflect.DeepEqual(live.Alerts[0].Labels, labels) || !reflect.DeepEqual(live.Alerts[1].Labels, other) {
		t.Fatalf("labels lost: %s", b)
	}
	if got.Installations[1].Reachable || got.Installations[1].Why != "read failed" || !got.Installations[2].NoContext {
		t.Fatalf("read failures lost: %s", b)
	}
	// A floor still filters structured output just as it filters text.
	floor := makeAlertSnapshot(alerts.Rules{Ignore: []string{ignored}, Floors: map[string]string{lab: "page"}}, targets[:1], answers[:1], time.Now())
	if len(floor.Installations[0].Alerts) != 0 {
		t.Fatalf("below-floor alerts: %+v", floor)
	}
}
