package alerts

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// The installations of the tests.
const (
	instA = "alpha"
	instB = "beta"
	instC = "gamma"
	instD = "delta"
	ctxA  = teleportPrefix + instA
	ctxB  = teleportPrefix + instB
	ctxD  = "admin@" + instD
)

var (
	now   = time.Date(2026, 9, 24, 18, 50, 0, 0, time.UTC)
	rules = Rules{Ignore: []string{"Heartbeat", "InhibitionOutsideWorkingHours", "Watchdog"}, Team: "bumblebee", Collapse: 3}
)

// alert is an alert shaped like the Alertmanager API's, labels as the
// fleet's rules set them; kv are extra label pairs.
func alert(fp, alertname, severity, team, starts string, kv ...string) Raw {
	l := map[string]string{"alertname": alertname, "severity": severity, "team": team}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return Raw{Fingerprint: fp, Labels: l, StartsAt: cmp.Or(starts, "2026-09-24T14:37:21.703Z")}
}

var (
	gateway = alert("b6dd", "DeploymentNotSatisfiedBumblebee", "page", "bumblebee", "2026-09-24T17:40:06Z",
		"cluster_id", instA, "installation", instA, "namespace", "agent-platform",
		"deployment", "klaus-gateway", "pod", "kube-prometheus-stack-kube-state-metrics-6d594cf44f-7jdsc")
	fluxWC = alert("046b", "FluxCustomerHelmReleaseFailed", "page", "shield", "",
		"cluster_id", "acme0230", "installation", instB, "namespace", "org-acme", "exported_namespace", "org-acme",
		"name", "acme0230-kubescape")
	noise = []Raw{
		alert("h1", "Heartbeat", "none", "atlas", ""),
		alert("w1", "Watchdog", "none", "atlas", ""),
		alert("i1", "InhibitionOutsideWorkingHours", "none", "atlas", ""),
	}
)

func where(t *testing.T, s Set, fp, want string) {
	t.Helper()
	if got := s[fp].Where; got != want {
		t.Errorf("%s: where = %q, want %q", fp, got, want)
	}
}

func TestNormalizeNoiseObjectAndCluster(t *testing.T) {
	got := rules.Normalize(append(slices.Clone(noise), gateway, fluxWC), instA)
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, []string{"046b", "b6dd"}) {
		t.Fatalf("keys = %v", keys)
	}
	where(t, got, "b6dd", "agent-platform/klaus-gateway") // the deployment, not ksm's pod
	where(t, got, "046b", "org-acme/acme0230-kubescape@acme0230")
}

func TestNormalizeGenericLabelsOnlyWhenNothingSpecific(t *testing.T) {
	got := rules.Normalize([]Raw{
		alert("l", "LoggingAgentMissingOnNode", "page", "atlas", "", "cluster_id", instB, "node", "ip-10-0-70-239"),
		alert("r", "ManagementClusterContainerIsRestartingTooFrequently", "notify", "rocket", "",
			"cluster_id", instB, "namespace", "kubescape", "pod", "node-agent-bvx2b", "node", "ip-10-0-162-113",
			"job", "kube-state-metrics"),
	}, instB)
	where(t, got, "l", "ip-10-0-70-239")
	where(t, got, "r", "kubescape/node-agent-bvx2b")
}

func TestNormalizeTheExportersOwnPodIsNotTheObject(t *testing.T) {
	ksm := []string{"service", "kube-prometheus-stack-kube-state-metrics", "job", "kube-state-metrics",
		"pod", "kube-prometheus-stack-kube-state-metrics-75c65ff446-h4l5s", "node", "ip-10-0-174-134"}
	got := rules.Normalize([]Raw{
		alert("v", "VclusterOnlineTooLong", "notify", "tenet", "",
			append([]string{"cluster_id", "cicd", "namespace", "mctl-xatest1-vcluster"}, ksm...)...),
		alert("c", "ChartOrphanConfigMap", "notify", "honeybadger", "", "cluster_id", instC,
			"namespace", "giantswarm", "service", "chart-operator", "job", "chart-operator",
			"pod", "chart-operator-585f7bf7c4-6dw65"),
	}, instC)
	where(t, got, "v", "mctl-xatest1-vcluster@cicd")
	where(t, got, "c", "giantswarm")
}

func TestNormalizeNoObject(t *testing.T) {
	where(t, rules.Normalize([]Raw{alert("x", "ClusterDNSZoneMissing", "notify", "phoenix", "", "cluster_id", instA)}, instA), "x", "-")
}

func TestNormalizeConfiguredIgnore(t *testing.T) {
	r := rules
	r.Ignore = []string{"DeploymentNotSatisfiedBumblebee"}
	got := r.Normalize(append(slices.Clone(noise), gateway), instA)
	if _, ok := got["b6dd"]; ok || len(got) != 3 {
		t.Errorf("an ignored name appears, or the default still applies: %v", slices.Sorted(maps.Keys(got)))
	}
}

func ok(raw ...Raw) Answer { return Answer{OK: true, Alerts: raw} }

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStepFirstLookGroupsAndMarks(t *testing.T) {
	raw := slices.Clone(noise)
	for i := range 28 {
		raw = append(raw, alert(fmt.Sprintf("c%d", i), "CertificateSecretWillExpireInLessThanTwoWeeks", "page", "phoenix",
			"2026-09-18T08:00:00Z", "namespace", "ns", "certificatename", fmt.Sprintf("cert-%d", i)))
	}
	lines, st := rules.Step(instA, nil, ok(append(raw, gateway)...), now)
	equal(t, lines, []string{
		"ALERTS alpha first look: 29 active, 29 page, 1 bumblebee",
		"ALERT OPEN alpha PAGE BUMBLEBEE DeploymentNotSatisfiedBumblebee agent-platform/klaus-gateway since 17:40Z",
		"ALERT OPEN alpha PAGE phoenix CertificateSecretWillExpireInLessThanTwoWeeks x28 (ns/cert-0, ...) since 09-18 08:00Z",
	})
	if !st.Reachable || len(st.Alerts) != 29 {
		t.Errorf("state = %v, %d alerts", st.Reachable, len(st.Alerts))
	}
}

func TestStepNewAndResolved(t *testing.T) {
	_, st := rules.Step(instB, nil, ok(fluxWC), now)
	restarted := alert("r1", "ManagementClusterContainerIsRestartingTooFrequently", "notify", "rocket",
		"2026-09-24T18:45:00Z", "cluster_id", instB, "namespace", "kube-system")
	lines, st := rules.Step(instB, st, ok(restarted), now)
	equal(t, lines, []string{
		"ALERT NEW beta notify rocket ManagementClusterContainerIsRestartingTooFrequently kube-system since 18:45Z",
		"ALERT RESOLVED beta PAGE shield FluxCustomerHelmReleaseFailed org-acme/acme0230-kubescape@acme0230",
	})
	lines, _ = rules.Step(instB, st, ok(restarted), now)
	equal(t, lines, nil) // silent when nothing changed
}

func TestStepBurstCollapsesAboveTheThreshold(t *testing.T) {
	missing := func(n int) []Raw {
		var out []Raw
		for i := range n {
			out = append(out, alert(fmt.Sprintf("n%d", i), "LoggingAgentMissingOnNode", "page", "atlas", "",
				"name", fmt.Sprintf("node-%d", i), "job", "ksm"))
		}
		return out
	}
	_, st := rules.Step(instA, nil, ok(), now)
	lines, st := rules.Step(instA, st, ok(missing(3)...), now)
	equal(t, lines, []string{
		"ALERT NEW alpha PAGE atlas LoggingAgentMissingOnNode node-0 since 14:37Z",
		"ALERT NEW alpha PAGE atlas LoggingAgentMissingOnNode node-1 since 14:37Z",
		"ALERT NEW alpha PAGE atlas LoggingAgentMissingOnNode node-2 since 14:37Z",
	})
	lines, st = rules.Step(instA, st, ok(missing(5)...), now)
	if len(lines) != 2 { // two more: still one line each
		t.Errorf("lines = %v", lines)
	}
	lines, _ = rules.Step(instA, st, ok(), now)
	equal(t, lines, []string{"ALERT RESOLVED alpha PAGE atlas LoggingAgentMissingOnNode x5 (node-0, ...)"})
}

func TestStepUnreachableOnceSetKept(t *testing.T) {
	down := Answer{Why: "port-forward: connection refused"}
	_, st := rules.Step(instA, nil, ok(gateway), now)
	lines, st := rules.Step(instA, st, down, now)
	equal(t, lines, []string{"ALERTS alpha unreachable: port-forward: connection refused"})
	lines, st = rules.Step(instA, st, down, now)
	equal(t, lines, nil)
	lines, _ = rules.Step(instA, st, ok(gateway), now)
	equal(t, lines, []string{"ALERTS alpha reachable again"}) // the kept set: nothing new, nothing resolved
}

func TestStepFirstRunUnreachableThenFirstLook(t *testing.T) {
	lines, st := rules.Step(instD, nil, Answer{Why: "no kube context for delta"}, now)
	equal(t, lines, []string{"ALERTS delta unreachable: no kube context for delta"})
	lines, _ = rules.Step(instD, st, ok(gateway), now)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "ALERTS delta first look: 1 active") {
		t.Errorf("lines = %v", lines)
	}
}

func TestStepWithoutTeam(t *testing.T) {
	r := rules
	r.Team = ""
	lines, _ := r.Step(instA, nil, ok(gateway), now)
	equal(t, lines, []string{
		"ALERTS alpha first look: 1 active, 1 page",
		"ALERT OPEN alpha PAGE bumblebee DeploymentNotSatisfiedBumblebee agent-platform/klaus-gateway since 17:40Z",
	})
}

func TestSnapshotGroupedPageAndTeamFirst(t *testing.T) {
	notify := alert("r1", "ManagementClusterContainerIsRestartingTooFrequently", "notify", "rocket", "", "cluster_id", instA)
	a, b := fluxWC, fluxWC
	a.Fingerprint, b.Fingerprint = "a", "b"
	raw := append(slices.Clone(noise), notify, a, b, gateway)
	lines := rules.SnapshotLines(instB, ok(raw...), now)
	var got []string
	for _, l := range lines[1:] {
		got = append(got, strings.Join(strings.Fields(l), " "))
	}
	if lines[0] != "beta at 18:50Z: 4 active" {
		t.Errorf("head = %q", lines[0])
	}
	equal(t, got, []string{
		"PAGE BUMBLEBEE DeploymentNotSatisfiedBumblebee alpha 1",
		"PAGE shield FluxCustomerHelmReleaseFailed acme0230 2",
		"notify rocket ManagementClusterContainerIsRestartingTooFrequently alpha 1",
	})
}

func TestSnapshotUnreachable(t *testing.T) {
	equal(t, rules.SnapshotLines(instA, Answer{Why: "timed out"}, now), []string{"alpha unreachable: timed out"})
}

func TestTargetsConfiguredThenLeased(t *testing.T) {
	contexts := []string{ctxA, ctxB, ctxD, "kind-lab-1"}
	got := Targets(
		[]Target{{Name: instA}, {Name: instC}, {Name: instD, Context: "other"}, {Name: instA}},
		map[string]string{instB: `"one"`, "lab-1": `"two"`, "browser": `"three"`, instA: `"four"`},
		contexts)
	want := []Target{
		{Name: instA, Context: ctxA, Why: `configured, leased by "four"`},
		{Name: instC, Why: "configured"},
		{Name: instD, Context: "other", Why: "configured"},
		{Name: instB, Context: ctxB, Why: `leased by "one"`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("targets =\n%+v\nwant\n%+v", got, want)
	}
}

func TestResolveContext(t *testing.T) {
	contexts := []string{ctxA, ctxD, instB, ctxB}
	for name, want := range map[string]string{instA: ctxA, instD: ctxD,
		instB: ctxB, instC: ""} {
		if got := ResolveContext(name, "", contexts); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got := ResolveContext(instA, "explicit", contexts); got != "explicit" {
		t.Errorf("explicit: %q", got)
	}
}
