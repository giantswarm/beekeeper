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

// flapped replays the two recorded readings of lab alternately, the way
// tonight's alert fired and cleared at every reading, and returns every
// line printed after the first look.
func flapped(t *testing.T, r Rules, readings int) ([]string, *Installation) {
	t.Helper()
	answers := []Answer{recorded(t, 1, lab), recorded(t, 2, lab)}
	_, st := r.Step(lab, nil, answers[0], now)
	var all []string
	for i := 1; i <= readings; i++ {
		var lines []string
		lines, st = r.Step(lab, st, answers[i%2], now.Add(time.Duration(i)*every))
		all = append(all, lines...)
	}
	return all, st
}

func TestFlappingAlertIsOneLine(t *testing.T) {
	undamped, _ := flapped(t, rules, 12)
	if len(undamped) < 12 {
		t.Fatalf("the recordings do not flap: %q", undamped)
	}
	damped := rules
	damped.Flap = Damper{Changes: 4, Window: time.Hour}
	lines, st := flapped(t, damped, 12)
	flapping, toggling := mentions(lines, "FLAPPING"), len(undamped)/12
	if len(flapping) != toggling {
		t.Errorf("FLAPPING lines = %q, want one for each of the %d toggling alerts", flapping, toggling)
	}
	// Three changes print, the fourth is the FLAPPING line, the next eight nothing.
	if len(lines) != 4*toggling {
		t.Errorf("lines of 12 flaps:\n%s", strings.Join(lines, "\n"))
	}
	if want := "since " + now.Add(every).UTC().Format("15:04Z"); !strings.HasSuffix(flapping[0], want) {
		t.Errorf("flapping line %q does not end in %q (its first change)", flapping[0], want)
	}
	for _, f := range st.Flaps {
		if !f.Flapping || len(f.Changes) != 12 {
			t.Errorf("record = %+v, want flapping with the 12 changes of the hour", f)
		}
	}

	// Stable for the window, the alert's next change prints again.
	last := now.Add(12 * every)
	quiet, _ := damped.Step(lab, st, recorded(t, 1, lab), last.Add(59*time.Minute))
	stable, st := damped.Step(lab, st, recorded(t, 1, lab), last.Add(time.Hour))
	if len(quiet)+len(stable) > 0 || len(st.Flaps) > 0 {
		t.Errorf("stable: lines %q %q, records %v", quiet, stable, st.Flaps)
	}
	again, _ := damped.Step(lab, st, recorded(t, 2, lab), last.Add(time.Hour+every))
	if len(again) != toggling || len(mentions(again, New)) != toggling {
		t.Errorf("after being stable: %q", again)
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
