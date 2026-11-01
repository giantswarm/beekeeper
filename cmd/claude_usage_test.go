package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const claudeUsageSessionReset = "2026-11-01T02:29:00Z"

const claudeUsageSample = `You are currently using your subscription to power your Claude Code usage

Current session: 18% used · resets Nov 1, 3:29am (Europe/Berlin)
Current week (all models): 19% used · resets Nov 1, 3pm (Europe/Berlin)
Current week (Fable): 1% used · resets Nov 1, 2:59pm (Europe/Berlin)

What's contributing to your limits usage?
Last 24h · 1598 requests · 46 sessions
  82% Start fresh when you switch tasks`

func claudeUsageJSON(t *testing.T, result string) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}{Result: result})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseClaudeUsage(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 25, 0, 0, time.UTC)
	r, err := parseClaudeUsage(claudeUsageJSON(t, claudeUsageSample), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.FiveHour.UsedPercent != 18 || r.SevenDay.UsedPercent != 19 || r.Buckets["Fable"].SevenDay.UsedPercent != 1 {
		t.Fatalf("windows: %+v", r)
	}
	if got := r.FiveHour.ResetsAt.Format(time.RFC3339); got != claudeUsageSessionReset {
		t.Fatal(got)
	}
	if got := r.SevenDay.ResetsAt.Format(time.RFC3339); got != "2026-11-01T14:00:00Z" {
		t.Fatal(got)
	}
	if got := r.Buckets["Fable"].SevenDay.ResetsAt.Format(time.RFC3339); got != "2026-11-01T13:59:00Z" {
		t.Fatal(got)
	}

}

func TestClaudeWindowResets(t *testing.T) {
	now := time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC)
	for _, tc := range []struct{ name, value, want string }{
		{"unused", "0% used", ""},
		{"hour", "18% used · resets 3pm (Europe/Berlin)", "2027-01-01T14:00:00Z"},
		{"minute", "18% used · resets 3:29am (Europe/Berlin)", "2027-01-01T02:29:00Z"},
		{"next-day", "18% used · resets 12:15am (Europe/Berlin)", "2027-01-01T23:15:00Z"},
		{"other-zone", "18% used · resets Dec 31, 5pm (America/Los_Angeles)", "2027-01-01T01:00:00Z"},
		{"next-year", "18% used · resets Jan 1, 3pm (Europe/Berlin)", "2027-01-01T14:00:00Z"},
		{"summer", "18% used · resets Jul 1, 3pm (Europe/Berlin)", "2027-07-01T13:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := parseClaudeWindow(tc.value, now)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if !w.ResetsAt.IsZero() {
					t.Fatal(w)
				}
				return
			}
			if got := w.ResetsAt.Format(time.RFC3339); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestClaudeWindowRecentlyPassedReset(t *testing.T) {
	for _, tc := range []struct{ name, now, stamp, want string }{
		{"dated-seconds", "2026-11-01T02:29:30Z", "Nov 1, 3:29am", claudeUsageSessionReset},
		{"time-seconds", "2026-11-01T02:29:30Z", "3:29am", claudeUsageSessionReset},
		{"dated-lag", "2026-11-01T02:31:00Z", "Nov 1, 3:29am", claudeUsageSessionReset},
		{"time-lag", "2026-11-01T02:31:00Z", "3:29am", claudeUsageSessionReset},
		{"previous-year", "2026-12-31T23:00:30Z", "Dec 31, 11:59pm", "2026-12-31T22:59:00Z"},
		{"previous-day", "2026-11-01T23:00:30Z", "11:59pm", "2026-11-01T22:59:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			w, err := parseClaudeWindow("18% used · resets "+tc.stamp+" (Europe/Berlin)", now)
			if err != nil {
				t.Fatal(err)
			}
			if got := w.ResetsAt.Format(time.RFC3339); got != tc.want {
				t.Fatalf("reset = %s, want %s", got, tc.want)
			}
			if usageFresh(usageReading{At: now.Add(-5 * time.Minute), FiveHour: w}, config.Usage{}, now) {
				t.Fatal("an elapsed reset must trigger a fresh probe")
			}
		})
	}
}

func TestClaudeUsageInvalidLines(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 25, 0, 0, time.UTC)
	for _, line := range []string{
		"Current session: unknown",
		"Current session: 101% used",
		"Current session: NaN% used",
		"Current session: 3% used",
		"Current session: 3% used · resets banana (Europe/Berlin)",
		"Current session: 3% used · resets 3pm (Unknown/Place)",
		"Current session: 3% used · resets 3pm",
		"Current fortnight: 0% used",
		"Current session without delimiter",
	} {
		t.Run(line, func(t *testing.T) {
			_, err := parseClaudeUsage(claudeUsageJSON(t, line), now)
			if err == nil || !strings.Contains(err.Error(), line) {
				t.Fatalf("error must name raw line: %v", err)
			}
		})
	}
	for _, result := range []string{"", "unexpected output", "Current session: 0% used", claudeUsageSample + "\nCurrent session: 0% used"} {
		if _, err := parseClaudeUsage(claudeUsageJSON(t, result), now); err == nil {
			t.Fatalf("accepted %q", result)
		}
	}
	if _, err := parseClaudeUsage([]byte(`{"is_error":true,"result":"failed"}`), now); err == nil {
		t.Fatal("accepted command error")
	}
}

func TestClaudeUsageAggregateAndNoPlan(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 25, 0, 0, time.UTC)
	cfg, err := config.Load(writeUsageConfig(t, "capacity: {providers: {claude: {}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Capacity.Providers[claudeComm]
	r, err := parseClaudeUsage(claudeUsageJSON(t, claudeUsageSample), now)
	if err != nil {
		t.Fatal(err)
	}
	r.At = now
	if err := validateReadingAt(r, p.Usage, now); err != nil {
		t.Fatal(err)
	}
	pv := buildSingleProviderView(claudeComm, p, providerAgents{}, providerReadingResult{reading: r, ok: true}, now)
	if pv.FiveHour == nil || pv.FiveHour.UsedPercent != 18 || pv.SevenDay == nil || pv.SevenDay.UsedPercent != 19 || pv.Buckets["Fable"].SevenDay.UsedPercent != 1 || pv.Room == 0 || pv.Error != "" {
		t.Fatalf("aggregate: %+v", pv)
	}
	for _, result := range []string{"You are currently using an API key to power your Claude Code usage", "API Usage Billing"} {
		r, err := parseClaudeUsage(claudeUsageJSON(t, result), now)
		if err != nil || !r.NoPlanWindows {
			t.Fatalf("no plan: %+v %v", r, err)
		}
		r.At = now
		if !usageFresh(r, p.Usage, now.Add(time.Minute)) || usageFresh(r, p.Usage, now.Add(6*time.Minute)) {
			t.Fatal("no-plan result did not use provider every")
		}
		pv := buildSingleProviderView(claudeComm, p, providerAgents{}, providerReadingResult{reading: r, ok: true}, now)
		if pv.Error != "" || pv.Room != 0 || !pv.NoPlanWindows || !strings.Contains(pv.windowsSummary(now), claudeNoPlanWindows) {
			t.Fatalf("no plan view: %+v", pv)
		}
	}
}

func writeUsageConfig(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeUsageProbeCwdAndCache(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte("capacity: {providers: {claude: {}}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenVersion(cfg.StateDir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	// A fake CLI records the cwd each execution and returns a real-shaped local
	// command result. Two reads within every must invoke it only once.
	raw := claudeUsageJSON(t, claudeUsageSample)
	fixture := filepath.Join(dir, "usage-result.json")
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "test-parent")
	t.Setenv(claudeCodeEnv, "1")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-only-placeholder")
	script := "#!/bin/sh\ntest -z \"$CLAUDE_CODE_SESSION_ID$CLAUDECODE\" || exit 1\ntest \"$CLAUDE_CODE_USE_VERTEX\" = 1 || exit 1\ntest \"${CLAUDE_CODE_OAUTH_TOKEN+present}\" = present || exit 1\ntest \"$*\" = \"-p /usage --output-format json\" || exit 1\npwd >> '" + calls + "'\ncat '" + fixture + "'\n"
	if err := os.WriteFile(filepath.Join(dir, claudeComm), []byte(script), 0o700); err != nil { //nolint:gosec // executable test CLI in the test-owned temporary directory
		t.Fatal(err)
	}
	a := &app{cfg: cfg, store: store, now: time.Now()}
	u := cfg.Capacity.Providers[claudeComm].Usage
	for range 2 {
		r, err := a.resolveReading(context.Background(), usageProviderFile(claudeComm), u)
		if err != nil || r.FiveHour == nil || r.SevenDay == nil {
			t.Fatalf("probe: %+v %v", r, err)
		}
	}
	got, err := os.ReadFile(calls) //nolint:gosec // test-owned calls file
	if err != nil {
		t.Fatal(err)
	}
	probeDir, err := filepath.EvalSymlinks(filepath.Join(cfg.StateDir, "usage", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	want := probeDir + "\n"
	if string(got) != want {
		t.Fatalf("probe cwd/calls: %q want %q", got, want)
	}
	// A login without plan windows is a successful cached reading, not a
	// failed probe retried at every capacity or watch tick.
	if err := os.WriteFile(fixture, claudeUsageJSON(t, "API Usage Billing"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.now = a.now.Add(6 * time.Minute)
	for range 2 {
		r, err := a.resolveReading(context.Background(), usageProviderFile(claudeComm), u)
		if err != nil || !r.NoPlanWindows {
			t.Fatalf("no-plan probe: %+v %v", r, err)
		}
	}
	got, err = os.ReadFile(calls) //nolint:gosec // test-owned calls file
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want+want {
		t.Fatalf("no-plan cache retried: %q", got)
	}

}

func TestClaudeUsageCapacityPickKeepsAggregateWindows(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	a, out := r.app(true)
	reading, err := parseClaudeUsage(claudeUsageJSON(t, claudeUsageSample), r.t0)
	if err != nil {
		t.Fatal(err)
	}
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) { return reading, nil }
	c := a.capacityPickCmd()
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Pick     string
		Capacity struct{ Providers map[string]*providerView }
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	pv := result.Capacity.Providers[claudeComm]
	if result.Pick != claudeComm+" "+pickOpus || pv == nil || pv.FiveHour == nil || pv.SevenDay == nil || pv.Room == 0 || pv.Buckets["Fable"] == nil {
		t.Fatalf("aggregate plan lost in capacity pick: %s", out.String())
	}
}

func TestClaudeUsageTargetKeepsAggregateRoom(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cfg, err := config.Load(writeUsageConfig(t, `capacity:
  ceiling: 2
  providers:
    claude: {ceiling: 2}
  choices:
    - {harness: claude, model: claude-fable, bucket: Fable}
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseClaudeUsage(claudeUsageJSON(t, claudeUsageSample), now)
	if err != nil {
		t.Fatal(err)
	}
	pv := buildSingleProviderView(claudeComm, cfg.Capacity.Providers[claudeComm], providerAgents{}, providerReadingResult{reading: r, ok: true}, now)
	v := &capacityView{Room: pv.Room, Providers: map[string]*providerView{claudeComm: pv}}
	got := v.forTarget(cfg.Capacity, claudeComm, "claude-fable-5-1")
	if got.Room != 2 || len(got.Blocks) != 0 {
		t.Fatalf("supplemental model week blocks aggregate room: room=%d blocks=%v", got.Room, got.Blocks)
	}
	// Ordinary independent usage buckets must still bound their own starts.
	pv.supplementalBuckets = false
	got = v.forTarget(cfg.Capacity, claudeComm, "claude-fable-5-1")
	if got.Room != 0 || len(got.Blocks) == 0 {
		t.Fatalf("independent bucket failed to block: room=%d blocks=%v", got.Room, got.Blocks)
	}
}

// An idle 5-hour window prints no Current session line; the weekly lines alone
// read as a 0 % five-hour window without a reset, as a custom reader's
// explicit idle window does, and leave room for a start.
func TestClaudeUsageIdleSessionWindow(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 25, 0, 0, time.UTC)
	const idle = `Current week (all models): 74% used · resets Nov 1, 3pm (Europe/Berlin)
Current week (Fable): 6% used · resets Nov 1, 3pm (Europe/Berlin)`
	r, err := parseClaudeUsage(claudeUsageJSON(t, idle), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.FiveHour == nil || r.FiveHour.UsedPercent != 0 || !r.FiveHour.ResetsAt.IsZero() {
		t.Fatalf("five-hour window: %+v", r.FiveHour)
	}
	if r.SevenDay == nil || r.SevenDay.UsedPercent != 74 || r.Buckets["Fable"].SevenDay.UsedPercent != 6 {
		t.Fatalf("weekly windows: %+v", r)
	}
	cfg, err := config.Load(writeUsageConfig(t, "capacity: {providers: {claude: {}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Capacity.Providers[claudeComm]
	r.At = now
	if err := validateReadingAt(r, p.Usage, now); err != nil {
		t.Fatal(err)
	}
	pv := buildSingleProviderView(claudeComm, p, providerAgents{}, providerReadingResult{reading: r, ok: true}, now)
	if pv.Error != "" || pv.Room == 0 || len(pv.Blocks) != 0 || pv.FiveHour == nil || pv.FiveHour.UsedPercent != 0 {
		t.Fatalf("idle session view: %+v", pv)
	}
	// Without the aggregate week the reading stays unknown.
	for _, result := range []string{"Current week (Fable): 6% used · resets Nov 1, 3pm (Europe/Berlin)", "Current session: 3% used · resets 3pm (Europe/Berlin)"} {
		if _, err := parseClaudeUsage(claudeUsageJSON(t, result), now); err == nil {
			t.Fatalf("accepted %q", result)
		}
	}
}
