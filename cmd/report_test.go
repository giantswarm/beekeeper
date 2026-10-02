package cmd

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fixtureEvents reads the fixture event log of the report.
func fixtureEvents(t *testing.T) []state.Event {
	t.Helper()
	f, err := os.Open("testdata/report-events.jsonl")
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

// reportFixture is the facts of the hour 01:00–02:00 EEST: the merges of the
// fixture event log, three workers, the person's queue and the machine.
func reportFixture(t *testing.T) *reportFacts {
	t.Helper()
	zone, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 27, 22, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	lanes := []state.Merge{
		{Repo: "example/portal", PR: 8, Lane: "example/portal", Phase: state.Settling},
		{Repo: "example/tools", PR: 43, Lane: "example/tools", Phase: state.Waiting},
	}
	f := &reportFacts{
		From: from, To: to, Zone: zone, Person: "Ada",
		Merged: windowMerges(fixtureEvents(t), lanes, from, to),
		Workers: []reportWorker{
			{Name: "Supervisor run 3", Doing: "supervisor"},
			{Name: "Worker tools#43", Issue: "example/tools#39", Doing: "CI on #43", Leases: []string{"lab-1"}},
			{Name: "Token rotation", Issue: "example/portal#5", Doing: "rotating the portal token"},
		},
		Notes:  []noteCount{{Owner: "Supervisor run 3", N: 2}, {Owner: "Secret sweep", Issue: "example/portal#5", N: 1}, {Owner: "Key audit", N: 1}, {Owner: "Vault check", N: 1}},
		Asking: []string{"Plan tools#44"},
		Drafts: []github.PR{{Repo: "example/plans", N: 9}},
		Lanes:  lanes,
		Timers: []state.Timer{{ID: 7, Due: to.Add(30 * time.Minute), What: "check the rollout of portal#8, see note #12"}},
		Machine: &snapshot{
			OOMSince: from, Load: [3]float64{3.2, 2.5, 2}, Cores: 24, LoadLimit: 36, CPUPSI10: 0.4,
			Mem:    machine.Mem{TotalMiB: 88064, AvailableMiB: 50176, SwapTotalMiB: 16384, SwapUsedMiB: 2048, ZswappedMiB: 1536},
			Root:   machine.Disk{UsedMiB: 1767424, FreeMiB: 71680, TotalMiB: 1921024},
			OOM:    []oomKill{{OOMKill: machine.OOMKill{Task: "go", Memcg: "/memcap.slice/run-1.scope"}}},
			Budget: &github.Budget{Limit: 5000, Remaining: 4800, Reset: to.Add(20 * time.Minute)},
		},
		Alerts: []alertCount{{Installation: "staging", Active: 3, Paging: 1}, {Installation: "lab", Active: 0}, {Installation: "far", Unreachable: "timeout"}},
	}
	for i := range f.Merged {
		f.Merged[i].Title = map[int]string{41: "feat(gate): queue merges | promotions", 7: "fix: the portal's login", 8: "chore(deps): bump x", 12: "docs: tools#41 in the notebook"}[f.Merged[i].N]
	}
	f.Owners = post.Owners("example/tools", "example/portal", "other/notebook")
	return f
}

func TestRenderReport(t *testing.T) {
	f := reportFixture(t)
	got := renderReport(f)
	want, err := os.ReadFile("testdata/report.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("renderReport =\n%s\nwant\n%s", got, want)
	}
	if p := post.Report(got, f.Zone, f.To); len(p) > 0 {
		t.Errorf("the report fails the check: %v", p)
	}
	for _, leak := range []string{"Token rotation", "Secret sweep", "Key audit", "Vault check", "rotating"} {
		if strings.Contains(got, leak) {
			t.Errorf("the report names %q", leak)
		}
	}
}

func TestRenderReportEmpty(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Berlin")
	from := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	got := renderReport(&reportFacts{From: from, To: from.Add(time.Hour), Zone: zone})
	for _, want := range []string{"**10:00–11:00 CET**\n", "**Merged & shipped**\n\n- none\n", "**Waiting on the person**\n\n- none\n", "**Queue**\n\n- none\n", "**Machine**\n\n- none\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the empty report has no %q:\n%s", want, got)
		}
	}
	if p := post.Report(got, zone, from.Add(time.Hour)); len(p) > 0 {
		t.Errorf("the empty report fails the check: %v", p)
	}
}

func TestReportTime(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Athens")
	ref := time.Date(2026, 9, 27, 22, 30, 0, 0, time.UTC) // 01:30 EEST
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"1h", ref.Add(-time.Hour)},
		{"01:00", time.Date(2026, 9, 27, 22, 0, 0, 0, time.UTC)},
		{"02:00", time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC)},
		{"2026-09-27T21:00:00Z", time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)},
	} {
		got, err := reportTime(ref, c.in, zone)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("reportTime(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	if _, err := reportTime(ref, "soon", zone); err == nil {
		t.Error("reportTime(soon) parses")
	}
}
