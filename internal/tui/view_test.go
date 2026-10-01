package tui

import (
	"strings"
	"testing"
	"time"
)

// stripANSI removes escape sequences from a rendered string.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		for i < len(s) && s[i] != 'm' {
			i++
		}
	}
	return b.String()
}

// fixture() is a fresh copy of the shared fixture data.
func fixture() *Data { return fixtureData() }

// The window shows the list's end while the last row is selected, so a
// tab's sections after its primary list are reachable, and the beginning
// before the first row.
func TestWindowReachesTheEndWithTheLastRow(t *testing.T) {
	for sel, want := range map[int]string{0: "row 0", 29: "the tail"} {
		tt := newTabLines(20, sel)
		for i := range 30 {
			tt.row(i, "row "+string(rune('0'+i%10))+strings.Repeat("x", i/10))
		}
		tt.head("the tail")
		got := tt.show(10)
		if !strings.Contains(got, want) {
			t.Errorf("sel %d misses %q:\n%s", sel, want, got)
		}
	}
}

func TestWatchingViewRendersTheMachine(t *testing.T) {
	got := stripANSI(watchingView(fixture(), 100, 40, 0))
	for _, want := range []string{
		"1.50 1.00 0.50", "2.5%", "12.0 GiB free of 32.0 GiB", "512 MiB/8.0 GiB",
		"4.0 GiB of 12G", "max 16G", "▱", "/tmp", "go test ./...", "42", "chrome", "7.8 GiB", "oomd: dry run",
		"1/2 free", "giantswarm-yolo", "3 nodes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("watching view misses %q:\n%s", want, got)
		}
	}
	// The waits section names its count and the OOM section too; an
	// empty one says so on its own rule line.
	if !strings.Contains(got, "waits · 1") || !strings.Contains(got, "oom kills · 1") {
		t.Errorf("section badges missing:\n%s", got)
	}
}

func TestSessionsViewRows(t *testing.T) {
	d := fixture()
	got := stripANSI(sessionsView(d, 130, 40, 0))
	for _, want := range []string{
		tBee, tWasp, "spare", "1.5 GiB", "45%·90k", "10t 1e $1.23",
		"[WARN] answer to the question",
		tIssue, "overlap",
		"name", "role", "idle", "mem", "ctx", "hour",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sessions view misses %q:\n%s", want, got)
		}
	}
	// A session with an empty hour says nothing about it: the old
	// "0t 0e $0.00" noise is gone. wasp is idle, priced nothing.
	wasp := strings.Split(got, "\n")
	for _, l := range wasp {
		if strings.Contains(l, tWasp) && strings.Contains(l, "0t") {
			t.Errorf("an idle session still shouts its empty hour: %q", l)
		}
		if strings.Contains(l, tWasp) && strings.Contains(l, "$0.00") {
			t.Errorf("an idle session still prints a zero cost: %q", l)
		}
	}
	// The idle ages render in the shared duration shape.
	if !strings.Contains(got, "1h30m") {
		t.Errorf("wasp's 90m idle did not render as 1h30m:\n%s", got)
	}
}
func TestSharingViewRendersLeasesHoldsLanes(t *testing.T) {
	got := stripANSI(sharingView(fixture(), 110, 40, 0))
	for _, want := range []string{
		tRepo, "push", tBee, "repo/fork", tWasp, "by sup",
		"release", "until ", "helm-apps", "5.3.0",
		"giantswarm/beekeeper#2", "settling  1", "waiting  4", "giantswarm/beekeeper#4", "waits behind",
		"unblocks beekeeper",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sharing view misses %q:\n%s", want, got)
		}
	}
	// The held-lease and holds sections carry their counts on the rule.
	if !strings.Contains(got, "leases held · 1") || !strings.Contains(got, "holds · 1") {
		t.Errorf("section badges missing:\n%s", got)
	}
}

func TestSupervisingViewRendersRolesAgentsNotes(t *testing.T) {
	got := stripANSI(supervisingView(fixture(), 110, 40, 1))
	for _, want := range []string{
		tSupervisor, tSup, "live", "120k", "relay at  400k",
		tGuide, "gone 5m", "waiting on person", "fix #9",
		"#3", "approve the bump", "overdue", "(default: yes)",
		"check the rollout", tBee, tIssue, "waits: review",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("supervising view misses %q:\n%s", want, got)
		}
	}
}

func TestAlertsViewRendersBudgetAndUpgrades(t *testing.T) {
	got := stripANSI(alertsView(fixture(), 110, 40, 0))
	for _, want := range []string{
		tOrg, "unreachable", "mgmt", "critical", "TargetDown", "warning",
		"KubePodCrashLooping", "devctl 1.2.0 -> 1.3.0", "4000/5000", "held: release window",
		"gh pr list",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("alerts view misses %q:\n%s", want, got)
		}
	}
	// The critical alert is listed before the warning one.
	if strings.Index(got, "TargetDown") > strings.Index(got, "KubePodCrashLooping") {
		t.Errorf("critical alert not grouped first:\n%s", got)
	}
}

func TestAlertsViewMarksFloorBreach(t *testing.T) {
	d := fixture()
	d.Budget.Remaining = 100
	got := stripANSI(alertsView(d, 110, 40, 0))
	if !strings.Contains(got, "under floor") {
		t.Errorf("a budget under its floor is not marked:\n%s", got)
	}
}

func TestEventsViewIsNewestFirst(t *testing.T) {
	got := stripANSI(eventsView(fixture(), 110, 40, 0))
	lines := strings.Split(got, "\n")
	var seen []string
	for _, l := range lines {
		for _, verb := range []string{tGrant, tClaim, tHoldSet} {
			if strings.Contains(l, verb) {
				seen = append(seen, verb)
			}
		}
	}
	if len(seen) != 3 || seen[0] != tGrant || seen[1] != tClaim || seen[2] != tHoldSet {
		t.Errorf("events not newest first: %v", seen)
	}
}

// The cryptic codes of the old screen have legible shapes: a short
// role badge and merge counts read as words, not as m:3q2m2f.
func TestBadgesAndMergeCountsReadAsWords(t *testing.T) {
	for role, want := range map[string]string{
		tSupervisor: tSup, tGuide: tGuide, "agent: fix #9": "agent",
		"agent (idle)": "agent", "": "-",
	} {
		if got := roleBadge(role); got != want {
			t.Errorf("roleBadge(%q) = %q, want %q", role, got, want)
		}
	}
	if got := stripANSI(mergeCell(Merges{Queued: 3, Merged: 2, Failed: 2})); got != "merge 3q/2m/2f" {
		t.Errorf("mergeCell = %q, want merge 3q/2m/2f", got)
	}
	if got := mergeCell(Merges{}); got != "" {
		t.Errorf("a session with no merges shows %q, want nothing", got)
	}
}

func TestWaitingSessionRowStandsOut(t *testing.T) {
	d := fixture()
	// Wide enough that the waiting text itself fits the what column.
	waiting, idle := sessionRow(d.Sessions[0], 140, d.At), sessionRow(d.Sessions[1], 140, d.At)
	if got := stripANSI(waiting); !strings.Contains(got, "[WARN] answer to the question") {
		t.Errorf("waiting marker lost through styling:\n%s", got)
	}
	if strings.Contains(idle, "WARN") {
		t.Errorf("a session waiting on nobody shows a waiting mark:\n%s", idle)
	}
	// Narrow windows lose the waiting text, never the flag: the row
	// stays findable at a glance.
	narrow := stripANSI(sessionRow(d.Sessions[0], 70, d.At))
	if !strings.Contains(narrow, "[WARN]") {
		t.Errorf("the waiting flag vanished at 70 columns:\n%s", narrow)
	}
}

// Identical firings collapse to one row that counts them, and the
// alertmanager stamp renders as an age — never a raw ISO string.
func TestAlertsViewDeduplicatesAndHumanizesTimes(t *testing.T) {
	d := fixture()
	since := testAt.Add(-3*time.Hour - 12*time.Minute).Format(time.RFC3339Nano)
	var as []Alert
	for range 24 {
		as = append(as, Alert{Severity: "page", Team: "phoenix",
			Alertname: "CertificateSecretWillExpireInNightly", Cluster: "glean",
			Where: "mc", Since: since})
	}
	d.Alerts = []AlertsFor{{Installation: "glean", Reachable: true, Alerts: as}}
	got := stripANSI(alertsView(d, 130, 40, 0))
	if n := strings.Count(got, "CertificateSecretWillExpireIn"); n != 1 {
		t.Errorf("the same firing shows %d times:\n%s", n, got)
	}
	if !strings.Contains(got, "×24") {
		t.Errorf("the collapsed row does not count its firings:\n%s", got)
	}
	if !strings.Contains(got, "since 3h12m") {
		t.Errorf("the since stamp did not become an age:\n%s", got)
	}
	if strings.Contains(got, since) {
		t.Errorf("a raw ISO stamp leaked into the body:\n%s", got)
	}
}

func TestNothingOverflowsTheWidth(t *testing.T) {
	m := newTestModel(t, &fakeSource{data: fixture()})
	for tab := range tabs {
		m.tab = tab
		m.sel[tab] = 0
		for _, line := range strings.Split(render(m, 60, 20), "\n") {
			if w := ansiWidth(line); w > 60 {
				t.Errorf("tab %s line %d columns wide: %q", tabs[tab], w, line)
			}
		}
	}
}

func TestBodyScrollsWithinHeight(t *testing.T) {
	d := fixture()
	for i := range 40 {
		d.Events = append(d.Events, Event{At: testAt.Add(-time.Duration(i) * time.Minute), Verb: "note-add", By: tBee, Detail: "a long note line to make the list scroll and overflow the window"})
	}
	m := newTestModel(t, &fakeSource{data: d})
	m.tab = 5
	m.sel[5] = 39
	out := strings.Split(render(m, 100, 12), "\n")
	if len(out) != 12 {
		t.Fatalf("render came out %d lines, want the window's 12", len(out))
	}
	if !strings.Contains(out[len(out)-1], "updated") {
		t.Errorf("the last line is not the footer: %q", out[len(out)-1])
	}
	// The selected last event must be visible after the scroll.
	if !strings.Contains(strings.Join(out, "\n"), "a long note line") {
		t.Errorf("the scrolled selection is missing:\n%s", strings.Join(out, "\n"))
	}
}

func TestDurShapes(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{4 * time.Second, "4s"},
		{59 * time.Second, "59s"},
		{12 * time.Minute, "12m"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
		{2 * 24 * time.Hour, "2d"},
		{-3 * time.Second, "0s"},
	} {
		if got := dur(tc.d); got != tc.want {
			t.Errorf("dur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestMibShapes(t *testing.T) {
	for _, tc := range []struct {
		m    int
		want string
	}{
		{512, "512 MiB"},
		{1023, "1023 MiB"},
		{1024, "1.0 GiB"},
		{1536, "1.5 GiB"},
	} {
		if got := mib(tc.m); got != tc.want {
			t.Errorf("mib(%d) = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestClockShapes(t *testing.T) {
	now := testAt // 2026-09-26 15:04 UTC
	// The fixtures sit in the machine's zone so only the day mark, not
	// the hour digits, is under test.
	for _, tc := range []struct {
		name string
		t    time.Time
		want string
	}{
		{"same day", now.Add(-3 * time.Hour), ""},
		{"tomorrow", now.Add(30 * time.Hour), "+1d "},
		{"last week", now.Add(-7 * 24 * time.Hour), "Sep 19 "},
	} {
		want := tc.want + tc.t.Local().Format("15:04")
		if got := clock(tc.t, now); got != want {
			t.Errorf("clock(%s) = %q, want %q", tc.name, got, want)
		}
	}
	if got := clock(time.Time{}, now); got != "-" {
		t.Errorf("clock(zero) = %q, want -", got)
	}
}

func TestFitCutsOnRuneBoundaries(t *testing.T) {
	if got := fit("hello", 10); got != "hello" {
		t.Errorf("fit of a short string = %q", got)
	}
	if got := fit("hello world", 5); got != "hell…" || ansiWidth(got) != 5 {
		t.Errorf("fit = %q, want hell… at 5 columns", got)
	}
	if got := fit("日本語です", 5); ansiWidth(got) > 5 || !strings.HasSuffix(got, "…") {
		t.Errorf("fit of wide runes = %q, %d columns", got, ansiWidth(got))
	}
	if got := fit("ab", 1); got != "…" {
		t.Errorf("fit to one column = %q, want the ellipsis alone", got)
	}
	if got := fit("x", 0); got != "" {
		t.Errorf("fit to no columns = %q", got)
	}
	if got := fitLeft("/very/long/path/to/repo", 10); got != "…o/repo" && !strings.HasPrefix(got, "…") {
		t.Errorf("fitLeft = %q, want an ellipsis-led tail", got)
	}
}

func TestCostAndPct(t *testing.T) {
	c := 2.5
	if got := cost(&c); got != "$2.50" {
		t.Errorf("cost = %q", got)
	}
	if got := cost(nil); got != "?" {
		t.Errorf("cost(nil) = %q, want ?", got)
	}
	if got := pct(0.446); got != "45%" {
		t.Errorf("pct = %q", got)
	}
}
