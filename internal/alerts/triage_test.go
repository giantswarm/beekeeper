package alerts

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// testdata/reading<n>/<installation>.json are two installations' answers,
// recorded with beekeeper alerts capture on 2026-09-25 a few minutes apart:
// gazelle, a Giant Swarm management cluster (page, notify and no severity),
// and lab, a kube-prometheus-stack cluster (warning, info, none). They are
// reduced to the labels beekeeper reads, with person, organization and
// workload-cluster names and node addresses replaced.
const (
	gazelle = "gazelle"
	lab     = "lab"
	every   = 5 * time.Minute
	info    = "info"
	warning = "warning"
	notify  = "notify"
)

func recorded(t *testing.T, n int, installation string) Answer {
	t.Helper()
	all, err := ReadAnswers(fmt.Sprintf("testdata/reading%d", n))
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := all[installation]
	if !ok {
		t.Fatalf("reading%d has no answer of %s", n, installation)
	}
	return Answer{OK: true, Alerts: raw}
}

// without is the answer less the alerts of that severity, for a reading in
// which they had not fired yet.
func without(ans Answer, severity string) Answer {
	ans.Alerts = slices.DeleteFunc(slices.Clone(ans.Alerts), func(r Raw) bool { return r.Labels["severity"] == severity })
	return ans
}

func count(ans Answer, severity string) int {
	n := 0
	for _, r := range ans.Alerts {
		if r.Labels["severity"] == severity {
			n++
		}
	}
	return n
}

func mentions(lines []string, word string) []string {
	var out []string
	for _, l := range lines {
		if slices.Contains(strings.Fields(l), word) {
			out = append(out, l)
		}
	}
	return out
}

func TestFloorHidesTheAlertsBelowItInWatchAndSnapshot(t *testing.T) {
	floored := rules
	floored.Floors = map[string]string{lab: warning}
	ans := recorded(t, 1, lab)
	below := 0
	for _, a := range floored.Normalize(ans.Alerts, lab) {
		if a.Severity == info || a.Severity == "none" {
			below++
		}
	}
	if count(ans, info) == 0 {
		t.Fatal("the recording has no info alert")
	}

	lines, st := floored.Step(lab, nil, ans, now)
	if got := mentions(lines, info); len(got) > 0 {
		t.Errorf("first look shows info alerts: %q", got)
	}
	if want := fmt.Sprintf("%d below the floor warning", below); !strings.Contains(lines[0], want) {
		t.Errorf("head = %q, want %q", lines[0], want)
	}
	if len(st.Alerts) != len(floored.Normalize(ans.Alerts, lab)) {
		t.Error("the baseline lacks the alerts below the floor")
	}

	// Info alerts that fire after the first look print nothing either.
	_, st = floored.Step(lab, nil, without(ans, info), now)
	if lines, _ := floored.Step(lab, st, ans, now.Add(every)); len(lines) > 0 {
		t.Errorf("new info alerts printed: %q", lines)
	}

	snap := floored.SnapshotLines(lab, ans, now)
	if got := mentions(snap, info); len(got) > 0 {
		t.Errorf("snapshot shows info alerts: %q", got)
	}
	if !strings.Contains(snap[0], "below the floor warning") {
		t.Errorf("snapshot head = %q", snap[0])
	}
	// Without a floor the same answer shows them.
	if got := mentions(rules.SnapshotLines(lab, ans, now), info); len(got) == 0 {
		t.Error("no floor hides info alerts")
	}
}

func TestBelow(t *testing.T) {
	for _, c := range []struct {
		severity, floor string
		want            bool
	}{
		{info, warning, true}, {"none", info, true}, {notify, Page, true}, {warning, notify, true},
		{warning, warning, false}, {Page, notify, false}, {"critical", notify, false},
		// No floor, and a severity without a rank, hide nothing.
		{info, "", false}, {"-", Page, false}, {"high", info, false},
	} {
		if got := Below(c.severity, c.floor); got != c.want {
			t.Errorf("Below(%q, %q) = %v", c.severity, c.floor, got)
		}
	}
}

func TestFloorAboveEverySeverityLeavesTheHead(t *testing.T) {
	floored := rules
	floored.Floors = map[string]string{gazelle: Page}
	lines, _ := floored.Step(gazelle, nil, recorded(t, 1, gazelle), now)
	want := "ALERTS gazelle first look: 0 active, 0 page, 0 bumblebee, 9 below the floor page"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

func TestFloorOrDamperChangePrintsNoBurst(t *testing.T) {
	for _, installation := range []string{gazelle, lab} {
		first, second := recorded(t, 1, installation), recorded(t, 2, installation)
		_, st := rules.Step(installation, nil, first, now)
		plain, _ := rules.Step(installation, st, second, now.Add(every))

		changed := rules
		changed.Floors = map[string]string{installation: notify}
		changed.Flap = Damper{Changes: 2, Window: time.Hour}
		floored, st := changed.Step(installation, st, first, now.Add(every))
		if len(floored) > 0 {
			t.Errorf("%s: a floor and a damper on the same answer printed %q", installation, floored)
		}
		// Back to no floor: the alerts hidden meanwhile are known, not NEW.
		if lines, _ := rules.Step(installation, st, second, now.Add(2*every)); !slices.Equal(lines, plain) {
			t.Errorf("%s: floor lifted:\n got %q\nwant %q", installation, lines, plain)
		}
	}
}

// flapped replays the two recorded readings of lab alternately, each twice
// (a single reading that misses an alert only holds it), the way tonight's
// alert fired and cleared, and returns every line printed after the first
// look. Each reading starts its alerts at its own time: an alert that fires
// again starts again.
func flapped(t *testing.T, r Rules, readings int) ([]string, *Installation) {
	t.Helper()
	answers := []Answer{recorded(t, 1, lab), recorded(t, 2, lab)}
	_, st := r.Step(lab, nil, answers[0], now)
	var all []string
	for i := 1; i <= readings; i++ {
		var lines []string
		at := now.Add(time.Duration(i) * every)
		lines, st = r.Step(lab, st, startedAt(answers[(i+1)/2%2], at), at)
		all = append(all, lines...)
	}
	return all, st
}

// changes are the lines about an alert's change, the held lines left out.
func changes(lines []string) []string { return mentions(lines, "ALERT") }

// startedAt is the answer with every alert started at t.
func startedAt(ans Answer, t time.Time) Answer {
	ans.Alerts = slices.Clone(ans.Alerts)
	for i := range ans.Alerts {
		ans.Alerts[i].StartsAt = t.UTC().Format(time.RFC3339Nano)
	}
	return ans
}

func TestFlappingAlertIsOneLine(t *testing.T) {
	undamped, _ := flapped(t, rules, 12)
	toggling := len(changes(undamped)) / 6
	if toggling == 0 || len(mentions(undamped, "FLAPPING")) > 0 {
		t.Fatalf("the recordings do not flap: %q", undamped)
	}
	damped := rules
	damped.Flap = Damper{Changes: 4, Window: time.Hour}
	lines, st := flapped(t, damped, 12)
	flapping := mentions(lines, "FLAPPING")
	if len(flapping) != toggling {
		t.Errorf("FLAPPING lines = %q, want one for each of the %d toggling alerts", flapping, toggling)
	}
	// A resolve and a re-fire within the window is the FLAPPING line, with
	// the alert's first start; later readings print nothing.
	if late, _ := flapped(t, damped, 12); len(changes(late)) != len(changes(lines)) {
		t.Fatal("the replay is not deterministic")
	}
	early, _ := flapped(t, damped, 5)
	if len(changes(early)) != len(changes(lines)) {
		t.Errorf("changes after the FLAPPING lines:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range flapping {
		for i := 3; i <= 5; i++ {
			if reading := "since " + now.Add(time.Duration(i)*every).UTC().Format("15:04Z"); strings.HasSuffix(l, reading) {
				t.Errorf("flapping line %q has the re-fire's start, not the alert's first", l)
			}
		}
	}
	for _, f := range st.Flaps {
		if !f.Flapping || len(f.Changes) != 6 {
			t.Errorf("record = %+v, want flapping with the 6 changes of the hour", f)
		}
	}

	// Stable for the window, the alert's next change prints again.
	last := now.Add(12 * every)
	quiet, _ := damped.Step(lab, st, recorded(t, 1, lab), last.Add(59*time.Minute))
	stable, st := damped.Step(lab, st, recorded(t, 1, lab), last.Add(time.Hour))
	if len(quiet)+len(stable) > 0 || len(st.Flaps) > 0 {
		t.Errorf("stable: lines %q %q, records %v", quiet, stable, st.Flaps)
	}
	again, st := damped.Step(lab, st, recorded(t, 2, lab), last.Add(time.Hour+every))
	gone, _ := damped.Step(lab, st, recorded(t, 2, lab), last.Add(time.Hour+2*every))
	both := changes(append(again, gone...))
	if len(both) != toggling || len(mentions(both, "FLAPPING")) > 0 {
		t.Errorf("after being stable: %q %q", again, gone)
	}
}

// A reading cut short by a slowed or failed read, between two readings with
// the same alerts, changes no alert and no start; a missed alert resolves
// only when the next reading misses it too.
func TestStretchedReadingResolvesNothing(t *testing.T) {
	damped := rules
	damped.Flap = Damper{Changes: 4, Window: time.Hour}
	restarted := alert("r1", "ManagementClusterContainerIsRestartingTooFrequently", "notify", "rocket",
		"2026-09-24T18:45:00Z", "cluster_id", instB, "namespace", "kube-system")
	later := func(r Raw) Raw {
		r.StartsAt = now.Add(20 * time.Minute).Format(time.RFC3339)
		return r
	}
	held := "ALERTS beta 1 of 2 alerts missing from this reading, kept until the next reading confirms them resolved"
	for _, c := range []struct {
		name     string
		readings []Answer
		want     [][]string
		// since is the start the baseline keeps of fluxWC.
		since string
	}{{
		name:     "failed read",
		readings: []Answer{{Why: "timed out"}, ok(fluxWC, restarted)},
		want: [][]string{
			{"ALERTS beta unreachable, alerts unseen for 5m (since 18:50Z): timed out"},
			{"ALERTS beta reachable again after 10m unseen"},
		},
		since: fluxWC.StartsAt,
	}, {
		name:     "partial read",
		readings: []Answer{ok(restarted), ok(later(fluxWC), restarted)},
		want:     [][]string{{held}, nil},
		since:    fluxWC.StartsAt,
	}, {
		name:     "partial read, then a failed one",
		readings: []Answer{ok(restarted), {Why: "timed out"}, ok(later(fluxWC), restarted)},
		want: [][]string{
			{held},
			{"ALERTS beta unreachable, alerts unseen for 5m (since 18:55Z): timed out"},
			{"ALERTS beta reachable again after 10m unseen"},
		},
		since: fluxWC.StartsAt,
	}, {
		name:     "true resolve",
		readings: []Answer{ok(restarted), ok(restarted)},
		want: [][]string{
			{held},
			{"ALERT RESOLVED beta PAGE shield FluxCustomerHelmReleaseFailed org-acme/acme0230-kubescape@acme0230"},
		},
	}, {
		name:     "true resolve, re-fire within the window",
		readings: []Answer{ok(restarted), ok(restarted), ok(later(fluxWC), restarted), ok(restarted), ok(restarted)},
		want: [][]string{
			{held},
			{"ALERT RESOLVED beta PAGE shield FluxCustomerHelmReleaseFailed org-acme/acme0230-kubescape@acme0230"},
			{"ALERT FLAPPING beta PAGE shield FluxCustomerHelmReleaseFailed org-acme/acme0230-kubescape@acme0230 since 14:37Z"},
			{held},
			nil,
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			_, st := damped.Step(instB, nil, ok(fluxWC, restarted), now)
			for i, ans := range c.readings {
				var lines []string
				lines, st = damped.Step(instB, roundTrip(t, st), ans, now.Add(time.Duration(i+1)*every))
				equal(t, lines, c.want[i])
			}
			if got := st.Alerts[fluxWC.Fingerprint].Since; got != c.since {
				t.Errorf("baseline start = %q, want %q", got, c.since)
			}
		})
	}
}

func TestDamperRecordsSurviveUnreachableAndTheState(t *testing.T) {
	damped := rules
	damped.Flap = Damper{Changes: 4, Window: time.Hour}
	_, st := flapped(t, damped, 5)
	lines, down := damped.Step(lab, st, Answer{Why: "timed out"}, now.Add(6*every))
	if len(lines) != 1 || len(down.Flaps) != len(st.Flaps) {
		t.Errorf("unreachable: %q, records %v", lines, down.Flaps)
	}
	store := NewStore(t.TempDir())
	if err := store.Save(&State{Installations: map[string]*Installation{lab: down}}); err != nil {
		t.Fatal(err)
	}
	back, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for fp, f := range down.Flaps {
		g := back.Installations[lab].Flaps[fp]
		if g == nil || g.Flapping != f.Flapping || len(g.Changes) != len(f.Changes) || !g.Changes[0].Equal(f.Changes[0]) {
			t.Errorf("%s: saved %+v, loaded %+v", fp, f, g)
		}
	}
	// Still flapping when it answers again: its changes print nothing.
	if lines, _ := damped.Step(lab, back.Installations[lab], recorded(t, 1, lab), now.Add(7*every)); len(mentions(lines, "ALERT")) > 0 {
		t.Errorf("back: %q", lines)
	}
}
