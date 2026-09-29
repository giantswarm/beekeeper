package alerts

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The cases are wake-ups of the supervisor's recorded runs of 24–29
// September, which a first filter was measured on: other teams' alerts on
// the e2e test clusters (t-…) of a management cluster woke it for nothing,
// the team's pages and pages outside the test clusters needed it.
const (
	mc              = "alpha"
	ourTeam         = "bumblebee"
	testClusterGlob = "t-*"
)

var (
	testClusters = Rules{Team: ourTeam, Collapse: 3, Quiet: []Quiet{{Cluster: testClusterGlob}}}

	onTestCluster = []Raw{
		alert("q1", "MonitoringAgentDown", "page", "atlas", "", "cluster_id", "t-gr5x1yijsdi6vhrh77"),
		alert("q2", "IncorrectResourceUsageData", "page", "tenet", "", "cluster_id", "t-ahz0dnsqiqlqt96zmb"),
		alert("q3", "CloudDaemonSetAvailability", "notify", "phoenix", "", "cluster_id", "t-4crh5033hc2ubjnfyw",
			"namespace", "kube-system", "daemonset", "aws-cloud-controller-manager"),
		alert("q4", "InhibitionControlPlaneUnhealthy", "none", "tenet", "", "cluster_id", "t-x88cqdpkx4u10oxzmi",
			"namespace", "org-t-x88cqdpkx4u10oxzmi", "name", "t-x88cqdpkx4u10oxzmi"),
	}
	oursOnTestCluster = alert("o1", "AgentPlatformContainerRestartingTooOften", "page", ourTeam, "", "cluster_id", "t-bx7aynmir71j1x8pdg",
		"namespace", "agent-platform")
	pageOnMC   = alert("p1", "LoggingAgentMissingOnNode", "page", "atlas", "", "cluster_id", mc, "node", "ip-10-0-164-17")
	notifyOnMC = alert("n1", "KarpenterServiceDegraded", "notify", "phoenix", "", "cluster_id", mc,
		"pod", "karpenter-taint-remover-h67cd")
)

func triage(t *testing.T, r Rules, prev, cur []Raw) (lines, quiet []string, next *Installation) {
	t.Helper()
	_, st := r.Step(mc, nil, ok(prev...), now)
	return r.Triage(mc, st, ok(cur...), now.Add(every))
}

func TestOtherTeamsTestClustersAreQuiet(t *testing.T) {
	lines, quiet, _ := triage(t, testClusters, nil, append(slices.Clone(onTestCluster), oursOnTestCluster, pageOnMC, notifyOnMC))
	for _, l := range lines {
		if strings.Contains(l, "@t-") && !strings.Contains(l, "BUMBLEBEE") {
			t.Errorf("another team's test-cluster alert woke: %s", l)
		}
	}
	for _, name := range []string{"AgentPlatformContainerRestartingTooOften", "LoggingAgentMissingOnNode", "KarpenterServiceDegraded"} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, name) }) {
			t.Errorf("%s did not wake: lines %q", name, lines)
		}
	}
	if len(quiet) != len(onTestCluster) {
		t.Fatalf("quiet = %q, want the %d test-cluster alerts", quiet, len(onTestCluster))
	}
	for _, l := range quiet {
		if !strings.HasSuffix(l, "(quiet: quiet rule cluster t-*)") {
			t.Errorf("quiet line without its rule: %s", l)
		}
	}

	// Resolving is as quiet as firing.
	lines, quiet, _ = triage(t, testClusters, onTestCluster, nil)
	if len(lines) != 0 || len(quiet) != len(onTestCluster) {
		t.Errorf("resolved: lines %q, quiet %q", lines, quiet)
	}
}

func TestInstallationInPlayQuietsNothing(t *testing.T) {
	r := testClusters
	r.InPlay = func(installation string) bool { return installation == mc }
	lines, quiet, _ := triage(t, r, nil, onTestCluster)
	if len(quiet) != 0 || len(lines) != len(onTestCluster) {
		t.Errorf("in play: lines %q, quiet %q", lines, quiet)
	}
}

func TestQuietRuleWithoutClusterNeverQuietsAPage(t *testing.T) {
	r := Rules{Team: ourTeam, Collapse: 3, Quiet: []Quiet{{Installation: mc}}}
	lines, quiet, _ := triage(t, r, nil, []Raw{pageOnMC, notifyOnMC})
	if len(lines) != 1 || !strings.Contains(lines[0], "LoggingAgentMissingOnNode") {
		t.Errorf("lines = %q, want the page", lines)
	}
	if len(quiet) != 1 || !strings.Contains(quiet[0], "KarpenterServiceDegraded") {
		t.Errorf("quiet = %q, want the notify alert", quiet)
	}
}

func TestQuietRuleMatchesByNameAndSeverity(t *testing.T) {
	r := Rules{Team: ourTeam, Collapse: 3, Quiet: []Quiet{{Alertname: "Karpenter*", Severity: "notify"}}}
	lines, quiet, _ := triage(t, r, nil, []Raw{notifyOnMC, pageOnMC})
	if len(quiet) != 1 || len(lines) != 1 {
		t.Errorf("lines %q, quiet %q", lines, quiet)
	}
}

// A reading that misses an alert (an Alertmanager restart, a reconnect)
// resolves it; the next reading has it again with its old start. That NEW
// is quiet, a real re-fire with a new start is not, and neither is the
// team's alert or a page.
func TestRepeatAfterAMissedReadingIsQuiet(t *testing.T) {
	r := Rules{Team: ourTeam, Collapse: 3, Flap: Damper{Window: time.Hour}}
	ours := alert("b1", "FluxGiantswarmHelmReleaseFailed", "page", ourTeam, "", "cluster_id", mc, "namespace", "flux-giantswarm", "name", "repo-manager")
	all := []Raw{notifyOnMC, pageOnMC, ours}
	_, st := r.Step(mc, nil, ok(all...), now)
	lines, _, st := r.Triage(mc, st, ok(), now.Add(every))
	if len(lines) != 3 {
		t.Fatalf("missed reading: %q", lines)
	}
	lines, quiet, st := r.Triage(mc, st, ok(all...), now.Add(2*every))
	if len(quiet) != 1 || !strings.Contains(quiet[0], "KarpenterServiceDegraded") || !strings.HasSuffix(quiet[0], "(quiet: "+Repeat+")") {
		t.Errorf("quiet = %q, want the notify alert's repeat", quiet)
	}
	if len(lines) != 2 {
		t.Errorf("lines = %q, want the page and the team's alert", lines)
	}
	if len(st.Resolved) != 0 {
		t.Errorf("resolved = %v, want none: all are back", st.Resolved)
	}

	// Fired again with a new start: it wakes.
	_, _, st = r.Triage(mc, st, ok(), now.Add(3*every))
	again := notifyOnMC
	again.StartsAt = now.Add(4 * every).Format(time.RFC3339)
	lines, quiet, st = r.Triage(mc, st, ok(again), now.Add(4*every))
	if len(quiet) != 0 || len(lines) != 1 {
		t.Errorf("re-fire: lines %q, quiet %q", lines, quiet)
	}

	// Past the window it is forgotten.
	_, _, st = r.Triage(mc, st, ok(), now.Add(5*every))
	st = roundTrip(t, st)
	if st.Resolved["n1"].Since != again.StartsAt {
		t.Fatalf("resolved = %v, want the notify alert's re-fire", st.Resolved)
	}
	lines, quiet, _ = r.Triage(mc, st, ok(again), now.Add(5*every+time.Hour))
	if len(quiet) != 0 || len(lines) != 1 {
		t.Errorf("after the window: lines %q, quiet %q", lines, quiet)
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"", "anything", true},
		{testClusterGlob, "t-gr5x1yijsdi6vhrh77", true},
		{testClusterGlob, mc, false},
		{"test: *", "test: beekeeper#12 delegation 8", true},
		{"*/b*", "a/x/bc", true},
		{"a*c*e", "abcde", true},
		{"a*c*e", "abcd", false},
		{mc, mc, true},
		{mc, mc + "s", false},
	} {
		if got := Glob(c.pattern, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
