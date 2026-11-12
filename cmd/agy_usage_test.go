package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
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

func TestAgyQuotaExhaustedBucketAndLaggingUsage(t *testing.T) {
	dir, cfgPath := isolateUsageTestEnv(t)
	rawCfg := `capacity:
  ceiling: 4
  availMinMiB: 1
  providers:
    agy:
      ceiling: 4
      every: 2m
      staleAfter: 10m
  choices:
    - harness: agy
      model: claude-opus-4-6-thinking
    - harness: agy
      model: gemini-3.1-pro-high
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
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
	t0 := time.Date(2026, 10, 28, 0, 30, 0, 0, time.UTC)
	a := &app{cfg: cfg, cfgPath: cfgPath, store: store, now: t0}

	// 1. Provider reports Claude and GPT models weekly limit remaining 0% (100% used):
	// the pick must not choose the exhausted bucket, even without explicit sevenDayMax.
	exhaustedTSV := "Gemini Models\tWeekly Limit Remaining\t95%\t2026-11-03T09:23:51Z\n" +
		"Gemini Models\tFive Hour Limit Remaining\t98%\t2026-10-28T05:00:00Z\n" +
		"Claude and GPT models\tWeekly Limit Remaining\t0%\t2026-11-03T09:58:13Z\n" +
		"Claude and GPT models\tFive Hour Limit Remaining\t95%\t2026-10-28T05:00:00Z\n"
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) {
		return parseAgyBuiltinUsage([]byte(exhaustedTSV))
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	v := a.resolveProvidersCapacity(context.Background(), st, nil, cfg.Capacity, false, nil, forWorker)
	if v.Choice == nil || v.Choice.Model != "gemini-3.1-pro-high" {
		t.Fatalf("choice with 0%% remaining Claude bucket = %+v, want gemini-3.1-pro-high", v.Choice)
	}
	if bv := v.Providers[config.AgyHarness].Buckets[config.DefaultAgyClaudeBucket]; bv == nil || bv.Ceiling != 0 || bv.Room != 0 {
		t.Fatalf("Claude bucket view = %+v, want ceiling 0 and room 0", bv)
	}

	// 2. Provider usage reading lags (still reporting 95% remaining / 5% used on Claude),
	// then the first 429 marks the Claude bucket full until its reset and stops starts on it.
	laggingTSV := "Gemini Models\tWeekly Limit Remaining\t95%\t2026-11-03T09:23:51Z\n" +
		"Gemini Models\tFive Hour Limit Remaining\t98%\t2026-10-28T05:00:00Z\n" +
		"Claude and GPT models\tWeekly Limit Remaining\t98%\t2026-11-03T09:58:13Z\n" +
		"Claude and GPT models\tFive Hour Limit Remaining\t98%\t2026-10-28T05:00:00Z\n"
	_ = os.Remove(filepath.Join(cfg.StateDir, usageProviderFile(config.AgyHarness)))
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) {
		return parseAgyBuiltinUsage([]byte(laggingTSV))
	}
	v = a.resolveProvidersCapacity(context.Background(), st, nil, cfg.Capacity, false, nil, forWorker)
	if v.Choice == nil || v.Choice.Model != testModelOpus46 {
		t.Fatalf("choice before 429 = %+v, want %s", v.Choice, testModelOpus46)
	}

	resetDur := 4*time.Hour + 46*time.Minute + 32*time.Second
	a.recordAgyQuotaExhausted(testModelOpus46, &agy.QuotaError{
		ShortError: "Individual quota reached for this model, resets in 4h46m32s",
		ErrorCode:  429,
		Status:     "RESOURCE_EXHAUSTED",
		ResetsIn:   resetDur,
		At:         t0,
		Model:      testModelOpus46,
	}, t0)

	v = a.resolveProvidersCapacity(context.Background(), st, nil, cfg.Capacity, false, nil, forWorker)
	if v.Choice == nil || v.Choice.Model != "gemini-3.1-pro-high" {
		t.Fatalf("choice after 429 = %+v, want gemini-3.1-pro-high", v.Choice)
	}
	if bv := v.Providers[config.AgyHarness].Buckets[config.DefaultAgyClaudeBucket]; bv == nil || bv.Room != 0 || !bv.High {
		t.Fatalf("Claude bucket after 429 = %+v, want room 0 and High", bv)
	}

	// After the reset time passes, the quota lock lifts.
	_ = dir
	a.now = t0.Add(resetDur + time.Minute)
	v = a.resolveProvidersCapacity(context.Background(), st, nil, cfg.Capacity, false, nil, forWorker)
	if v.Choice == nil || v.Choice.Model != testModelOpus46 {
		t.Fatalf("choice after reset = %+v, want %s", v.Choice, testModelOpus46)
	}
}

// The quota-stopped worker of the quota tests and its task.
const (
	quotaTestWorker = "beekeeper 2150"
	quotaTestTask   = "Work https://github.com/giantswarm/beekeeper/issues/2150"
)

// quotaFixture is a desk whose agy session name hit a quota 429 and still
// holds task.
type quotaFixture struct {
	cfg     *config.Config
	store   *state.FileStore
	t0      time.Time
	startID string
	party   state.Party
}

func newQuotaFixture(t *testing.T, name, task, issue string) quotaFixture {
	t.Helper()
	dir, cfgPath := isolateUsageTestEnv(t)
	briefTpl := filepath.Join(dir, "worker-brief.md")
	if err := os.WriteFile(briefTpl, []byte("Work {{.URL}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawCfg := `capacity:
  ceiling: 2
  availMinMiB: 1
  providers:
    agy:
      ceiling: 2
      every: 2m
      staleAfter: 10m
  choices:
    - harness: agy
      model: claude-opus-4-6-thinking
    - harness: agy
      model: gemini-3.1-pro-high
supervisor:
  workerBrief: ` + briefTpl + `
`
	if err := os.WriteFile(cfgPath, []byte(rawCfg), 0o600); err != nil {
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
	f := quotaFixture{cfg: cfg, store: store, t0: time.Date(2026, 10, 28, 0, 30, 0, 0, time.UTC), startID: "quota-worker-1"}
	f.party = state.Party{HostSession: agy.HostPrefix + f.startID, Name: name}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts = append(st.Starts, state.Start{Party: f.party, Harness: agy.Harness, Model: testModelOpus46, At: f.t0})
		st.Agents = append(st.Agents, state.Agent{Party: f.party, Harness: agy.Harness, Model: testModelOpus46, Task: task, Registered: f.t0, AssignedAt: f.t0, ActivityAt: f.t0})
		st.Records = append(st.Records, state.Record{Session: f.party, Issue: issue, By: f.party, At: f.t0})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := agy.WriteQuotaError(agy.InboxPath(cfg.StateDir, f.startID), &agy.QuotaError{
		ShortError: "Individual quota reached for this model, resets in 4h46m32s",
		ErrorCode:  429,
		Status:     "RESOURCE_EXHAUSTED",
		ResetsIn:   4*time.Hour + 46*time.Minute + 32*time.Second,
		At:         f.t0,
		Model:      testModelOpus46,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// loop is a supervisor loop over f whose beekeeper runs answer with run.
func (f quotaFixture) loop(t *testing.T, out *bytes.Buffer, run func(args []string) (string, int, error)) *loop.Loop {
	t.Helper()
	a := &app{cfg: f.cfg, store: f.store, out: out, now: f.t0.Add(time.Minute)}
	origRun := loop.BeekeeperRun
	loop.BeekeeperRun = func(_ context.Context, args ...string) (string, int, error) { return run(slices.Clone(args)) }
	t.Cleanup(func() { loop.BeekeeperRun = origRun })
	origRefs := refStates
	refStates = func(_ context.Context, refs []github.PR) (map[github.PR]github.RefState, error) {
		states := map[github.PR]github.RefState{}
		for _, r := range refs {
			states[r] = github.RefState{Kind: github.RefPullRequest, State: github.Open}
		}
		return states, nil
	}
	t.Cleanup(func() { refStates = origRefs })
	return &loop.Loop{Host: a, Said: map[string]bool{}}
}

func TestAgyQuotaWorkerSlotFreedAndTaskRequeued(t *testing.T) {
	f := newQuotaFixture(t, quotaTestWorker, quotaTestTask, "giantswarm/beekeeper#2150")
	cfg, store, t0 := f.cfg, f.store, f.t0
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	// Slot is immediately freed in countAgents and countProviderAgents even before staleAfter (24h).
	c := countAgents(st, nil, t0.Add(time.Minute), cfg)
	if len(c.Busy) != 0 || len(c.Gone) != 1 || !strings.HasPrefix(c.Gone[0], quotaTestWorker) {
		t.Fatalf("countAgents = %+v, want 0 busy and 1 gone", c)
	}
	pa := countProviderAgents(st, nil, cfg.Capacity, t0.Add(time.Minute), cfg)
	if len(pa[config.AgyHarness].Busy) != 0 {
		t.Fatalf("countProviderAgents busy = %v, want empty", pa[config.AgyHarness].Busy)
	}
	sc := pickScope{
		me:     state.Party{Name: "caller"},
		alive:  func(state.Party) bool { return false },
		gone:   func(ag state.Agent) bool { return agyQuotaError(cfg, ag) != nil },
		listed: t0.Add(time.Minute),
		owner:  defaultOrg,
	}
	if owners := boardOwners(st, sc); len(owners) != 0 {
		t.Fatalf("boardOwners = %v, want empty for quota-stopped worker", owners)
	}

	var out bytes.Buffer
	var runCalls [][]string
	l := f.loop(t, &out, func(args []string) (string, int, error) {
		runCalls = append(runCalls, args)
		return "started replacement\n", 0, nil
	})
	if err := l.RecoverQuota(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "SUPERVISOR QUOTA beekeeper 2150:") || !strings.Contains(out.String(), "re-queuing giantswarm/beekeeper#2150 on another choice") {
		t.Fatalf("recoverQuota output = %q", out.String())
	}
	if len(runCalls) != 1 || !slices.Contains(runCalls[0], "start") || !slices.Contains(runCalls[0], quotaTestWorker) || !slices.Contains(runCalls[0], "auto") || slices.Contains(runCalls[0], "--review") {
		t.Fatalf("recoverQuota runCalls = %v", runCalls)
	}
	brief, err := os.ReadFile(runCalls[0][slices.Index(runCalls[0], quotaTestWorker)+1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), quotaTestTask) || !strings.Contains(string(brief), "Continue your existing task:") {
		t.Fatalf("replacement brief = %q", brief)
	}
	after, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Agents) != 0 {
		t.Fatalf("agents after recoverQuota = %+v, want empty", after.Agents)
	}
	if len(after.Records) != 1 || after.Records[0].Ended.IsZero() {
		t.Fatalf("records after recoverQuota = %+v, want ended", after.Records)
	}
	if _, err := os.Stat(agy.QuotaPath(cfg.StateDir, f.startID)); !os.IsNotExist(err) {
		t.Fatalf("quota marker after replacement: %v, want removed", err)
	}
}

// A held or refused replacement start keeps the entry, its records and its
// quota marker, so the next pass re-queues the task.
func TestAgyQuotaRequeueKeepsTaskUntilStarted(t *testing.T) {
	for _, tc := range []struct {
		name string
		held bool
		code int
	}{
		{name: "held", held: true},
		{name: "refused by the gate", code: ExitRefused},
		{name: "start failed", code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQuotaFixture(t, quotaTestWorker, quotaTestTask, "giantswarm/beekeeper#2150")
			st, err := f.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			var runCalls [][]string
			l := f.loop(t, &out, func(args []string) (string, int, error) {
				runCalls = append(runCalls, args)
				return "no room\n", tc.code, nil
			})
			l.StartsHeld = tc.held
			if err := l.RecoverQuota(context.Background(), st); err != nil {
				t.Fatal(err)
			}
			if tc.held != (len(runCalls) == 0) {
				t.Fatalf("runCalls = %v, held %v", runCalls, tc.held)
			}
			after, err := f.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Agents) != 1 || !after.Agents[0].Is(f.party) || after.Agents[0].Task == "" {
				t.Fatalf("agents = %+v, want the quota-stopped entry kept", after.Agents)
			}
			if len(after.Records) != 1 || !after.Records[0].Ended.IsZero() {
				t.Fatalf("records = %+v, want open", after.Records)
			}
			if agyQuotaError(f.cfg, after.Agents[0]) == nil {
				t.Fatal("quota marker removed, want kept for the next pass")
			}
			if strings.Contains(out.String(), "re-queuing") {
				t.Fatalf("output = %q, want no re-queue reported", out.String())
			}

			// The next pass with room starts the replacement and frees it.
			runCalls = nil
			l.StartsHeld = false
			tc.code = 0
			if err := l.RecoverQuota(context.Background(), after); err != nil {
				t.Fatal(err)
			}
			if len(runCalls) != 1 {
				t.Fatalf("second pass runCalls = %v", runCalls)
			}
			if after, err = f.store.Read(); err != nil {
				t.Fatal(err)
			}
			if len(after.Agents) != 0 {
				t.Fatalf("agents after second pass = %+v, want empty", after.Agents)
			}
		})
	}
}

// A reviewer the loop started that hits a quota 429 restarts as the
// reviewer of its round, with its round's brief and the review flags.
func TestAgyQuotaReviewerRestartsAsReviewer(t *testing.T) {
	const ref = "giantswarm/beekeeper#2153"
	name := superloop.ReviewerName(ref, 2)
	f := newQuotaFixture(t, name, "Review https://github.com/giantswarm/beekeeper/pull/2153 at head abc", ref)
	key := "review " + ref + "@abc"
	if err := f.store.Record(about(event(loop.LoopParty, loop.LoopClaimVerb, "[%s] %s", key, name), ref)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var runCalls [][]string
	l := f.loop(t, &out, func(args []string) (string, int, error) {
		runCalls = append(runCalls, args)
		return `{"room":1}` + "\n", 0, nil
	})
	brief, err := l.Brief("", "Review {{.URL}} at {{.Head}}\n", key, loop.BriefData{URL: "https://github.com/giantswarm/beekeeper/pull/2153", Head: "c0ffee"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RecoverQuota(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(runCalls, func(args []string) bool { return slices.Contains(args, "start") })
	if i < 0 {
		t.Fatalf("runCalls = %v, want a start", runCalls)
	}
	start := runCalls[i]
	if !slices.Contains(start, name) || !slices.Contains(start, brief) || !slices.Contains(start, "--review") || !slices.Contains(start, "--"+reviewOfSelfHostedFlag) {
		t.Fatalf("reviewer restart = %v, want %q with brief %s, --review and --%s", start, name, brief, reviewOfSelfHostedFlag)
	}
}

func TestAgyQuotaDelayedRecoveryKeepsOriginalReset(t *testing.T) {
	f := newQuotaFixture(t, quotaTestWorker, quotaTestTask, "giantswarm/beekeeper#2150")
	qerr := agy.ReadQuotaError(f.cfg.StateDir, f.startID)
	for _, delay := range []time.Duration{time.Hour, 6 * time.Hour} {
		a := &app{cfg: f.cfg, store: f.store, now: f.t0.Add(delay)}
		a.recordAgyQuotaExhaustedForID("", testModelOpus46, qerr, a.now)
		var ledger usageQuotaLedger
		if found, err := f.store.ReadFile(usageQuotaFile(config.AgyHarness), &ledger); err != nil || !found {
			t.Fatalf("quota ledger: found %v, error %v", found, err)
		}
		got := ledger.Buckets[config.DefaultAgyClaudeBucket].ResetsAt
		if want := f.t0.Add(qerr.ResetsIn); !got.Equal(want) {
			t.Fatalf("recovered after %s: reset %s, want %s", delay, got, want)
		}
	}
}
