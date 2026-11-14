package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const sessionOne = "one"

// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestWatchSaysARunawayOnce(t *testing.T) {
	dir := t.TempDir()
	w, _, out := notifyingWatch(t, dir, false)
	w.Notifier = nil
	watchApp(w.Host).cfg.LeaseDir = filepath.Join(dir, "leases")
	watchApp(w.Host).now = time.Now()
	ts := watchApp(w.Host).now.Add(-time.Minute).UTC().Format(time.RFC3339)
	tr := filepath.Join(dir, "s1.jsonl")
	line := `{"type":"assistant","timestamp":"` + ts + `","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":950000},"content":[]}}` + "\n"
	if err := os.WriteFile(tr, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	one := state.Party{Session: "s1", Name: sessionOne}
	if err := watchApp(w.Host).store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(one, "merge.queued", "o/r#1 in lane l"), event(one, testMergedWord, "o/r#1 exit 0, release v1")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	live := []*claude.Session{{ID: "s1", Name: sessionOne, Transcript: tr, LastActive: watchApp(w.Host).now}}

	_, ms := watchApp(w.Host).sessionMetrics(live, nil, nil)
	if m := ms[0]; m.Merges != (merges{Queued: 1, Merged: 1}) || m.Context != 950010 || m.LastHour.CostUSD == nil {
		t.Errorf("metrics %+v", m)
	}
	w.Runaways(live, nil)
	w.Runaways(live, nil)
	got := strings.TrimSpace(out.String())
	if strings.Count(got, "\n") != 0 || !strings.Contains(got, `RUNAWAY: "one"'s context is at 95% of its 1000000 tokens (threshold 90%)`) {
		t.Errorf("want one context line, got:\n%s", got)
	}
}

func TestRunawayThresholds(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	s := &claude.Session{Name: sessionOne}
	m := &metrics{}
	m.LastHour.GitHubCalls, m.LastHour.SameErrors, m.LastHour.SameError = 1001, 11, "Bash: go test ./..."
	m.ContextFill = 0.5
	lines := a.runaway(s, m)
	if len(lines) != 2 || !strings.Contains(lines["github"], "1001 GitHub calls") || !strings.Contains(lines["errors"], "11 times") {
		t.Errorf("lines %v", lines)
	}
	a.cfg.Metrics.Runaway.GitHubCallsPerHour, a.cfg.Metrics.Runaway.SameErrorRepeats = -1, -1
	if lines := a.runaway(s, m); len(lines) != 0 {
		t.Errorf("a negative threshold is off: %v", lines)
	}
}

func TestTotalsRankUnknownCostsLast(t *testing.T) {
	one, two := 1.0, 2.0
	sessions := []*claude.Session{{Name: "unpriced"}, {Name: "cheap"}, {Name: "dear"}, {Name: "idle"}}
	ms := []*metrics{{}, {}, {}, {}}
	ms[0].LastHour = claude.Counts{Tokens: claude.Tokens{Input: 9}, CostUnknown: []string{"claude-x"}}
	ms[1].LastHour = claude.Counts{Tokens: claude.Tokens{Input: 1}, CostUSD: &one}
	ms[2].LastHour = claude.Counts{Tokens: claude.Tokens{Input: 1}, CostUSD: &two}
	tot := totals(sessions, ms)
	var names []string
	for _, t := range tot.Top {
		names = append(names, t.Name)
	}
	if strings.Join(names, ",") != "dear,cheap,unpriced" || tot.LastHour.CostUSD != nil || costText(tot.LastHour) != "cost unknown" {
		t.Errorf("top %v cost %v", names, tot.LastHour.CostUSD)
	}
}
