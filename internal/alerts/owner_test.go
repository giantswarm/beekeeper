package alerts

import (
	"strings"
	"testing"
	"time"
)

func pageState(start time.Time) *State {
	since := start.UTC().Format(time.RFC3339Nano)
	return &State{Installations: map[string]*Installation{
		"alpha": {Reachable: true, Alerts: Set{
			"fp1": {Severity: Page, Team: "t", Alertname: "PodRestarting", Where: "ns/app", Since: since},
			"fp2": {Severity: "notify", Team: "t", Alertname: "DiskFilling", Where: "ns/vol", Since: since},
		}},
		"beta": {Reachable: false, Alerts: Set{
			"fp3": {Severity: Page, Team: "t", Alertname: "Stale", Where: "ns/x", Since: since},
		}},
	}}
}

func neverOwned(string) (time.Time, bool) { return time.Time{}, false }

// A page unowned for the grace is due once per grace period, each with its
// own mark; a non-paging alert and an unreachable installation's last set
// never are.
func TestUnownedPagesOncePerGrace(t *testing.T) {
	start := time.Date(2026, 10, 2, 3, 11, 0, 0, time.UTC)
	pages := pageState(start).Firing(Page)
	if len(pages) != 1 || pages[0].Name() != "alpha/PodRestarting" {
		t.Fatalf("Firing(page) = %+v, want only alpha's page", pages)
	}
	grace := 15 * time.Minute
	marks := map[string]bool{}
	var lines []string
	for m := 0; m <= 47; m++ {
		for _, u := range UnownedPages(pages, grace, neverOwned, start.Add(time.Duration(m)*time.Minute)) {
			if !marks[u.Mark] {
				marks[u.Mark] = true
				lines = append(lines, u.Line())
			}
		}
	}
	want := []string{
		"PAGE UNOWNED alpha PodRestarting ns/app for 15m: beekeeper alerts own alpha/PodRestarting",
		"PAGE UNOWNED alpha PodRestarting ns/app for 30m: beekeeper alerts own alpha/PodRestarting",
		"PAGE UNOWNED alpha PodRestarting ns/app for 45m: beekeeper alerts own alpha/PodRestarting",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines over 47 minutes:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// One installation's unowned alerts of one name are one page, said with
// the longest unowned of them.
func TestUnownedPagesOnePerAlertname(t *testing.T) {
	start := time.Date(2026, 10, 2, 3, 11, 0, 0, time.UTC)
	st := pageState(start)
	st.Installations["alpha"].Alerts["fp4"] = Alert{Severity: Page, Team: "t", Alertname: "PodRestarting", Where: "ns/other",
		Since: start.Add(-time.Hour).Format(time.RFC3339Nano)}
	got := UnownedPages(st.Firing(Page), 15*time.Minute, neverOwned, start.Add(20*time.Minute))
	if len(got) != 1 || got[0].Line() != "PAGE UNOWNED alpha PodRestarting ns/other (+1) for 1h20m: beekeeper alerts own alpha/PodRestarting" {
		t.Fatalf("two unowned alerts of one name: %+v", got)
	}
}

// An owned page is never due; once its owner stopped owning it, the count
// starts from that moment.
func TestUnownedPagesOwnerStopsAndRestartsTheCount(t *testing.T) {
	start := time.Date(2026, 10, 2, 3, 11, 0, 0, time.UTC)
	pages := pageState(start).Firing(Page)
	key := pages[0].Key
	owned := func(k string) (time.Time, bool) { return time.Time{}, k == key }
	if got := UnownedPages(pages, 15*time.Minute, owned, start.Add(3*time.Hour)); len(got) != 0 {
		t.Fatalf("an owned page is due: %+v", got)
	}
	ended := start.Add(time.Hour)
	until := func(k string) (time.Time, bool) { return ended, k == key }
	if got := UnownedPages(pages, 15*time.Minute, until, ended.Add(14*time.Minute)); len(got) != 0 {
		t.Fatalf("due 14m after its owner ended: %+v", got)
	}
	got := UnownedPages(pages, 15*time.Minute, until, ended.Add(15*time.Minute))
	if len(got) != 1 || !strings.Contains(got[0].Line(), " for 15m: ") {
		t.Fatalf("15m after its owner ended: %+v", got)
	}
}

// A page that resolves and fires again is a new alert: its key differs.
func TestKeyChangesWithTheStart(t *testing.T) {
	a := Alert{Since: "2026-10-02T03:11:00Z"}
	b := a
	b.Since = "2026-10-02T05:00:00Z"
	if Key("fp", a) == Key("fp", b) {
		t.Error("a page firing again has the old key")
	}
}

func TestMatch(t *testing.T) {
	firing := pageState(time.Now()).Firing("")
	for q, want := range map[string]int{"alpha/PodRestarting": 1, "PodRestarting": 1, "fp2": 1, "alpha/Nope": 0, "beta/Stale": 0} {
		if got := len(Match(firing, q)); got != want {
			t.Errorf("Match(%q) = %d alerts, want %d", q, got, want)
		}
	}
}
