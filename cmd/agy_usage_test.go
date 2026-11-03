package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const agyUsageSample = "Gemini Models\tWeekly Limit Remaining\t51%\t2026-11-03T09:23:51Z\n" +
	"Gemini Models\tFive Hour Limit Remaining\t100%\t2026-11-01T11:02:31Z\n" +
	"Claude and GPT models\tWeekly Limit Remaining\t23%\t2026-11-03T09:58:13Z\n" +
	"Claude and GPT models\tFive Hour Limit Remaining\t96%\t2026-11-01T12:02:03Z\n"

func TestParseAgyBuiltinUsage(t *testing.T) {
	r, err := parseAgyBuiltinUsage([]byte(agyUsageSample))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.BucketOrder, []string{config.DefaultAgyGeminiBucket, config.DefaultAgyClaudeBucket}) {
		t.Fatalf("bucket order = %v", r.BucketOrder)
	}
	gem, cl := r.Buckets[config.DefaultAgyGeminiBucket], r.Buckets[config.DefaultAgyClaudeBucket]
	if gem.FiveHour.UsedPercent != 0 || gem.SevenDay.UsedPercent != 49 ||
		!gem.SevenDay.ResetsAt.Equal(time.Date(2026, 11, 3, 9, 23, 51, 0, time.UTC)) {
		t.Errorf("Gemini bucket = %+v / %+v", gem.FiveHour, gem.SevenDay)
	}
	if cl.FiveHour.UsedPercent != 4 || cl.SevenDay.UsedPercent != 77 ||
		!cl.FiveHour.ResetsAt.Equal(time.Date(2026, 11, 1, 12, 2, 3, 0, time.UTC)) {
		t.Errorf("Claude bucket = %+v / %+v", cl.FiveHour, cl.SevenDay)
	}
	if r.FiveHour != nil || r.SevenDay != nil {
		t.Errorf("agy has no aggregate windows: %+v", r)
	}

	// A bucket printing one window only is left out; the complete one counts.
	partial := strings.Replace(agyUsageSample, "Gemini Models\tFive Hour Limit Remaining\t100%\t2026-11-01T11:02:31Z\n", "", 1)
	r, err = parseAgyBuiltinUsage([]byte(partial))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Buckets[config.DefaultAgyGeminiBucket]; ok || !slices.Equal(r.BucketOrder, []string{config.DefaultAgyClaudeBucket}) {
		t.Errorf("partial bucket kept: %v %+v", r.BucketOrder, r.Buckets)
	}

	for name, raw := range map[string]string{
		"empty":            "  \n",
		"no records":       "You are not signed in\n",
		"only one window":  "Gemini Models\tWeekly Limit Remaining\t51%\t2026-11-03T09:23:51Z\n",
		"bad percentage":   "Gemini Models\tWeekly Limit Remaining\tmany%\t2026-11-03T09:23:51Z\n",
		"bad reset":        "Gemini Models\tWeekly Limit Remaining\t51%\tsoon\n",
		"percentage > 100": "Gemini Models\tWeekly Limit Remaining\t151%\t2026-11-03T09:23:51Z\n",
	} {
		if r, err := parseAgyBuiltinUsage([]byte(raw)); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, r)
		}
	}
}

// TestAgyProviderDefaultsToBuiltinReader: a command-less agy provider, even
// agy: {} or one with only a ceiling, reads agy's own usage report; an
// explicit command still wins.
func TestAgyProviderDefaultsToBuiltinReader(t *testing.T) {
	_, cfgPath := isolateUsageTestEnv(t)
	for raw, want := range map[string][]string{
		"capacity: {providers: {agy: {}}}\n":                                  config.DefaultAgyUsageCommand,
		"capacity: {providers: {agy: {floor: 0, ceiling: 3}}}\n":              config.DefaultAgyUsageCommand,
		"capacity: {providers: {agy: {ceiling: 2, fiveHourMax: 90}}}\n":       config.DefaultAgyUsageCommand,
		"capacity: {providers: {agy: {command: [/opt/read-agy]}}}\n":          {"/opt/read-agy"},
		"capacity: {providers: {claude: {}, agy: {every: 1m, ceiling: 1}}}\n": config.DefaultAgyUsageCommand,
	} {
		if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := cfg.Capacity.Providers[config.AgyHarness].Command; !slices.Equal(got, want) {
			t.Errorf("%s: command = %q, want %q", strings.TrimSpace(raw), got, want)
		}
	}
}

// TestAgyUsageProbeCwdEnvAndCache runs the built-in reader against a fake agy:
// under the state directory, without the requesting worker's agy identity,
// cached for the provider's every and read again once it passes.
func TestAgyUsageProbeCwdEnvAndCache(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	if err := os.WriteFile(cfgPath, []byte("capacity: {providers: {agy: {every: 2m, staleAfter: 10m}}}\n"), 0o600); err != nil {
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
	fixture := filepath.Join(dir, "agy-usage.tsv")
	if err := os.WriteFile(fixture, []byte(agyUsageSample), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv(agy.EnvAgent, "test-agent-id")
	t.Setenv(agy.EnvName, "Worker One")
	t.Setenv("AGY_KEEP_ME", "1")
	script := "#!/bin/sh\ntest -z \"${" + agy.EnvAgent + "+set}${" + agy.EnvName + "+set}\" || exit 1\ntest \"$AGY_KEEP_ME\" = 1 || exit 1\ntest \"$*\" = \"-p /usage\" || exit 1\npwd >> '" + calls + "'\ncat '" + fixture + "'\n"
	if err := os.WriteFile(filepath.Join(dir, config.AgyHarness), []byte(script), 0o700); err != nil { //nolint:gosec // executable test CLI in the test-owned temporary directory
		t.Fatal(err)
	}
	// Before every reset of the sample, so no passed reset forces a re-read.
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	a := &app{cfg: cfg, store: store, now: t0}
	u := cfg.Capacity.Providers[config.AgyHarness].Usage
	read := func() usageReading {
		t.Helper()
		r, err := a.resolveReading(context.Background(), usageProviderFile(config.AgyHarness), u)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		return r
	}
	for range 2 {
		r := read()
		if b := r.Buckets[config.DefaultAgyClaudeBucket]; b.FiveHour == nil || b.SevenDay == nil || b.SevenDay.UsedPercent != 77 {
			t.Fatalf("probe reading: %+v", r)
		}
	}
	probeDir, err := filepath.EvalSymlinks(filepath.Join(cfg.StateDir, "usage", config.AgyHarness))
	if err != nil {
		t.Fatal(err)
	}
	once := probeDir + "\n"
	if got, _ := os.ReadFile(calls); string(got) != once { //nolint:gosec // test-owned calls file
		t.Fatalf("probe cwd/calls within every: %q want %q", got, once)
	}
	a.now = t0.Add(3 * time.Minute)
	read()
	if got, _ := os.ReadFile(calls); string(got) != once+once { //nolint:gosec // test-owned calls file
		t.Fatalf("probe calls after every: %q want %q", got, once+once)
	}
}
