package metrics

import (
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// teamBumblebee is the team of the busy snapshot.
const teamBumblebee = "bumblebee"

// contract is §5.3: every metric and its labels, spelled out here rather than taken from the
// code's constants, space-separated.
var contract = map[string]string{
	NameGitHubRequests:        "api class operation code conditional",
	NameGitHubGraphQLPoints:   "class",
	NameGitHubRateRemaining:   "api",
	NameWebhookDeliveries:     "event outcome",
	NameObservations:          "source kind",
	NameWebhookMissed:         "event",
	NameSourceDegraded:        "source",
	NamePollingActive:         "source",
	NameWaits:                 "on state",
	NameWaitWakes:             "on trigger",
	NameItems:                 "team class phase",
	NameDecisions:             "point mode outcome",
	NameRefusals:              "operation",
	NameInvariantViolations:   "invariant",
	NameLastDecisionTimestamp: "",
	NameHistoryDropped:        "",
	NameReconcileErrors:       "controller",
}

func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

// value sums the series of name whose labels include labels, and counts them.
func value(mfs map[string]*dto.MetricFamily, name string, labels map[string]string) (sum float64, series int) {
	mf, ok := mfs[name]
	if !ok {
		return 0, 0
	}
outer:
	for _, m := range mf.GetMetric() {
		have := map[string]string{}
		for _, lp := range m.GetLabel() {
			have[lp.GetName()] = lp.GetValue()
		}
		for k, v := range labels {
			if have[k] != v {
				continue outer
			}
		}
		series++
		switch {
		case m.Counter != nil:
			sum += m.GetCounter().GetValue()
		case m.Gauge != nil:
			sum += m.GetGauge().GetValue()
		}
	}
	return sum, series
}

func busySnapshot() Snapshot {
	s := healthy(at("09:30"))
	team := v1alpha1.Team{}
	team.Name = teamBumblebee
	team.Spec.BoardOption = "Bumblebee🐝"
	s.Teams = []v1alpha1.Team{team}
	s.Installation.Status.Sources[v1alpha1.SourceWebhooks] = v1alpha1.SourceStatus{
		State: v1alpha1.SourceDegraded, Reason: v1alpha1.SourceReasonDeliveriesFailed, Polling: true,
	}
	labelled := item("giantswarm/roadmap#103", v1alpha1.PhaseImplementing)
	labelled.Labels = map[string]string{v1alpha1.LabelTeam: teamBumblebee}
	outOfScope := item("giantswarm/other#1", v1alpha1.PhaseImplementing)
	outOfScope.Status.InScope = new(bool)
	s.Items = []v1alpha1.Item{
		item("giantswarm/roadmap#101", v1alpha1.PhasePublished, withChanges("giantswarm/klaus#55")),
		item("giantswarm/roadmap#102", v1alpha1.PhaseReady, blockedBy("giantswarm/roadmap#100"),
			itemWait(wait(v1alpha1.WaitOnDependency, "issue:giantswarm/roadmap#100", v1alpha1.WaitWatching, at("09:00").Add(168*time.Hour), false))),
		labelled,
		outOfScope,
	}
	s.Changes = []v1alpha1.Change{
		change("giantswarm/klaus#55", v1alpha1.PROpen, ci(v1alpha1.CIPending),
			changeWait(wait(v1alpha1.WaitOnCI, "ci:giantswarm/klaus@a1", v1alpha1.WaitWatching, at("10:00"), false))),
		change("giantswarm/klaus#56", v1alpha1.PROpen, ci(v1alpha1.CIPending)), // missing its wait
	}
	return s
}

func TestEveryContractMetricWithItsLabels(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewPedanticRegistry()
	m := New(reg)
	m.SetSnapshot(busySnapshot)
	m.ObserveGitHub(forge.APIREST, "observation", "pulls/get", 200, false, 0)
	m.SetRateRemaining(forge.APIREST, 4999)
	m.ObserveDelivery("check_run", DeliveryAccepted)
	m.Observations.WithLabelValues("webhook", "ci").Inc()
	m.WebhookMissed.WithLabelValues("check_run").Inc()
	m.WaitWakes.WithLabelValues("ci", "webhook").Inc()
	m.Decisions.WithLabelValues("ready?", "shadow", "agree").Inc()
	m.Refusals.WithLabelValues("pulls/get").Inc()
	m.HistoryDropped.Inc()
	m.ReconcileErrors.WithLabelValues("item").Inc()
	m.ObserveGitHub(forge.APIGraphQL, "observation", "BoardProbe", 200, false, 1)

	mfs := gather(t, reg)
	for name, labels := range contract {
		mf, ok := mfs[name]
		if !ok {
			t.Errorf("%s not exported", name)
			continue
		}
		for _, metric := range mf.GetMetric() {
			var got []string
			for _, lp := range metric.GetLabel() {
				got = append(got, lp.GetName())
			}
			want := strings.Fields(labels)
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s labels = %v, want %v", name, got, want)
			}
		}
	}
	if len(mfs) != len(contract) {
		t.Errorf("exported %d metrics, want %d", len(mfs), len(contract))
	}
}

func TestDerivedGaugesFromTheSnapshot(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewPedanticRegistry()
	m := New(reg)
	m.SetSnapshot(busySnapshot)
	mfs := gather(t, reg)

	checks := []struct {
		name   string
		labels map[string]string
		value  float64
		series int
	}{
		{NameWaits, nil, 2, 2},
		{NameWaits, map[string]string{labelOn: "ci", labelState: "watching"}, 1, 1},
		{NameWaits, map[string]string{labelOn: "dependency", labelState: "watching"}, 1, 1},
		{NameItems, map[string]string{labelTeam: teamBumblebee}, 3, 3},
		{NameItems, map[string]string{labelTeam: teamBumblebee, labelClass: "Implementable", labelPhase: "Ready"}, 1, 1},
		{NameSourceDegraded, map[string]string{labelSource: "webhooks"}, 1, 1},
		{NameSourceDegraded, map[string]string{labelSource: "board"}, 0, 1},
		{NamePollingActive, map[string]string{labelSource: "webhooks"}, 1, 1},
		{NamePollingActive, map[string]string{labelSource: "board"}, 0, 1},
		{NameInvariantViolations, nil, 1, 9},
		{NameInvariantViolations, map[string]string{labelInvariant: InvWIPHasRunOrWait}, 1, 1},
		{NameInvariantViolations, map[string]string{labelInvariant: InvStageAgeUnderP90}, 0, 1},
		{NameLastDecisionTimestamp, nil, float64(at("09:30").Unix()), 1},
	}
	for _, c := range checks {
		v, n := value(mfs, c.name, c.labels)
		if v != c.value || n != c.series {
			t.Errorf("%s%v = %v over %d series, want %v over %d", c.name, c.labels, v, n, c.value, c.series)
		}
	}

	// The next collection reflects the next snapshot: nothing is left over from the last.
	m.SetSnapshot(func() Snapshot { return healthy(at("09:40")) })
	mfs = gather(t, reg)
	if v, n := value(mfs, NameWaits, nil); v != 0 || n != 0 {
		t.Errorf("engine_waits after the waits ended = %v over %d series, want none", v, n)
	}
	if v, n := value(mfs, NameInvariantViolations, nil); v != 0 || n != 9 {
		t.Errorf("engine_invariant_violations = %v over %d series, want 0 over 9", v, n)
	}
}

func TestNonLeaderExportsNothingDerived(t *testing.T) {
	t.Parallel()
	for name, snap := range map[string]func() Snapshot{
		"no snapshot func": nil,
		"not leading":      func() Snapshot { return Snapshot{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := prometheus.NewPedanticRegistry()
			m := New(reg)
			if snap != nil {
				m.SetSnapshot(snap)
			}
			m.HistoryDropped.Add(0)
			mfs := gather(t, reg)
			for _, n := range []string{NameWaits, NameItems, NameSourceDegraded, NamePollingActive, NameInvariantViolations, NameLastDecisionTimestamp} {
				if _, ok := mfs[n]; ok {
					t.Errorf("%s exported by a replica that doesn't lead", n)
				}
			}
			if _, ok := mfs[NameHistoryDropped]; !ok {
				t.Errorf("%s not exported", NameHistoryDropped)
			}
		})
	}
}

func TestObserveGitHub(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewPedanticRegistry()
	m := New(reg)
	m.ObserveGitHub(forge.APIREST, "observation", "pulls/get", 200, false, 0)
	m.ObserveGitHub(forge.APIREST, "observation", "pulls/get", 304, true, 0)
	m.ObserveGitHub(forge.APIREST, "observation", "pulls/get", 304, true, 0)
	m.ObserveGitHub(forge.APIREST, "observation", "pulls/get", 502, false, 0)
	m.ObserveGitHub(forge.APIGraphQL, "observation", "BoardTeamItems", 200, false, 2)
	m.ObserveGitHub(forge.APIGraphQL, "observation", "BoardProbe", 200, false, 1)
	m.ObserveGitHub(forge.APIApp, "observation", "apps/list-webhook-deliveries", 200, false, 0)
	m.SetRateRemaining(forge.APIREST, 3000)
	mfs := gather(t, reg)

	checks := []struct {
		name   string
		labels map[string]string
		value  float64
	}{
		{NameGitHubRequests, map[string]string{labelAPI: forge.APIREST, labelConditional: "true", labelCode: "304"}, 2},
		{NameGitHubRequests, map[string]string{labelAPI: forge.APIREST, labelConditional: "false", labelCode: "200", labelOperation: "pulls/get", labelClass: "observation"}, 1},
		{NameGitHubRequests, map[string]string{labelAPI: forge.APIREST, labelCode: "502"}, 1},
		{NameGitHubRequests, map[string]string{labelAPI: forge.APIApp, labelOperation: "apps/list-webhook-deliveries"}, 1},
		{NameGitHubGraphQLPoints, map[string]string{labelClass: "observation"}, 3},
		{NameGitHubRateRemaining, map[string]string{labelAPI: forge.APIREST}, 3000},
	}
	for _, c := range checks {
		if v, _ := value(mfs, c.name, c.labels); v != c.value {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, v, c.value)
		}
	}
}

func TestDeliveryEvent(t *testing.T) {
	t.Parallel()
	for event, want := range map[string]string{
		"check_run": "check_run", "projects_v2_item": "projects_v2_item", "ping": "ping",
		"installation_repositories": "installation_repositories", "fork": "other", "": "other",
	} {
		if got := DeliveryEvent(event); got != want {
			t.Errorf("DeliveryEvent(%q) = %q, want %q", event, got, want)
		}
	}
}
