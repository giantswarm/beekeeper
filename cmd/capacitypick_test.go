package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The pick tests' models: one per subscription and the self-hosted one.
const (
	pickOpus = "opus-x"
	pickGPT  = "gpt-x"
	pickQwen = "local/qwen-x"
	// noPick is capacity pick's line when no start fits.
	noPick = "none"
	// ofSelfHostedArg asks for the reviewer of a self-hosted model's work.
	ofSelfHostedArg = "--review-of-self-hosted"
)

// pickRig is a desk with two subscriptions, claude and codex, each with its
// 5-hour and 7-day stops and a ceiling of 2, and a self-hosted provider with
// a ceiling of 1. At t0 the 7-day windows' even pace is exactly 50%.
type pickRig struct {
	t                *testing.T
	dir, cfgPath     string
	cfg              *config.Config
	store            *state.FileStore
	t0               time.Time
	claude, codex    [2]float64
	reset5h, reset7d time.Time
	choices          string
	// claudeExtra is more of the claude provider's configuration.
	claudeExtra string
}

func newPickRig(t *testing.T, choices string) *pickRig {
	t.Helper()
	dir, cfgPath := isolateUsageTestEnv(t)
	t0 := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	r := &pickRig{t: t, dir: dir, cfgPath: cfgPath, t0: t0, choices: choices,
		claude: [2]float64{10, 40}, codex: [2]float64{10, 40},
		reset5h: t0.Add(3 * time.Hour), reset7d: t0.Add(84 * time.Hour)}
	r.configure("")
	return r
}

// configure writes the desk's configuration, extra under capacity.
func (r *pickRig) configure(extra string) {
	r.t.Helper()
	raw := `capacity:
  floor: 1
  ceiling: 10
` + extra + `  providers:
    claude:
      ceiling: 2
      command: ["read-claude"]
` + r.claudeExtra + `      fiveHourMax: 90
      sevenDayMax: 95
    codex:
      ceiling: 2
      command: ["read-codex"]
      fiveHourMax: 90
      sevenDayMax: 95
    local:
      kind: self-hosted
      ceiling: 1
  choices:
` + r.choices
	if err := os.WriteFile(r.cfgPath, []byte(raw), 0o600); err != nil {
		r.t.Fatal(err)
	}
	cfg, err := config.Load(r.cfgPath)
	if err != nil {
		r.t.Fatal(err)
	}
	r.cfg = cfg
	if r.store, err = state.OpenVersion(cfg.StateDir, "dev"); err != nil {
		r.t.Fatal(err)
	}
}

func (r *pickRig) app(jsonOut bool) (*app, *bytes.Buffer) {
	var out bytes.Buffer
	for _, name := range []string{claudeComm, codex.Harness} {
		_ = os.Remove(filepath.Join(r.cfg.StateDir, usageProviderFile(name)))
	}
	read := func(_ context.Context, cmd []string, _ time.Duration) (usageReading, error) {
		w := r.codex
		if cmd[0] == testReadClaude {
			w = r.claude
		}
		return usageReading{
			FiveHour: &usageWindow{UsedPercent: w[0], ResetsAt: r.reset5h},
			SevenDay: &usageWindow{UsedPercent: w[1], ResetsAt: r.reset7d},
		}, nil
	}
	return &app{cfgPath: r.cfgPath, cfg: r.cfg, store: r.store, now: r.t0, out: &out, as: agentOne, json: jsonOut, readUsage: read}, &out
}

// pick runs capacity pick with args and returns its output.
func (r *pickRig) pick(jsonOut bool, args ...string) string {
	r.t.Helper()
	a, out := r.app(jsonOut)
	c := a.capacityCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs(append([]string{"pick"}, args...))
	if err := c.Execute(); err != nil {
		r.t.Fatalf("capacity pick %v: %v", args, err)
	}
	return out.String()
}

// want asserts capacity pick's one line.
func (r *pickRig) want(line string, args ...string) {
	r.t.Helper()
	if got := r.pick(false, args...); got != line+"\n" {
		r.t.Errorf("capacity pick %v = %q, want %q", args, got, line)
	}
}

// subscriptionChoices are the two subscriptions, claude first.
const subscriptionChoices = `    - {harness: claude, model: opus-x}
    - {harness: codex, model: gpt-x}
`

// The one line with the full reasoning under --json: with claude ahead of
// its even pace, the other subscription is the pick.
func TestCapacityPickTakesTheSubscriptionNotAheadOfItsPace(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.want("claude " + pickOpus)
	r.claude[1] = 51
	r.want(codex.Harness + " " + pickGPT)
	var p struct {
		Pick, Harness, Model, Provider string
		Capacity                       struct {
			Choice struct{ Candidates []choiceCandidate }
		}
	}
	if err := json.Unmarshal([]byte(r.pick(true)), &p); err != nil {
		t.Fatal(err)
	}
	if p.Pick != codex.Harness+" "+pickGPT || p.Provider != codex.Harness || len(p.Capacity.Choice.Candidates) != 2 ||
		!strings.Contains(p.Capacity.Choice.Candidates[0].Reason, "ahead of pace") {
		t.Errorf("capacity pick --json = %+v", p)
	}
	r.codex[1] = 50.5
	r.want(noPick)
}

// A paceMargin and a choice's skipPace no longer let a start run ahead of
// the pace.
func TestCapacityPickHasNoPaceException(t *testing.T) {
	r := newPickRig(t, `    - {harness: claude, model: opus-x, paceMargin: 5}
    - {harness: codex, model: gpt-x, skipPace: true}
`)
	r.claude[1], r.codex[1] = 54, 70
	r.want(noPick)
}

// The 5-hour window's stop percent holds a provider.
func TestCapacityPickHoldsAProviderAtItsFiveHourStop(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.claude[0] = 90
	r.want(codex.Harness + " " + pickGPT)
}

// The 7-day window's stop percent holds a provider even under its pace.
func TestCapacityPickHoldsAProviderAtItsSevenDayStop(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.reset7d = r.t0.Add(time.Hour)
	r.claude[1] = 95
	r.want(codex.Harness + " " + pickGPT)
}

// A provider whose running sessions fill its ceiling takes no start.
func TestCapacityPickHoldsAProviderAtItsSessionCeiling(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	if err := r.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, n := range []string{"C 1", "C 2"} {
			ag := tasked(n)
			ag.Harness, ag.Model = claudeComm, pickOpus
			st.Agents = append(st.Agents, ag)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.want(codex.Harness + " " + pickGPT)
}

// The machine's headroom is a hard gate: no pick while it blocks a start.
func TestCapacityPickHoldsEveryStartOnTheMachinesHeadroom(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	orig := plat.Machine
	plat.Machine = tightMachine{Machine: orig}
	t.Cleanup(func() { plat.Machine = orig })
	r.want(noPick)
}

// A review is decided exactly like a worker's start, even where the desk
// still configures capacity.reviewSkipPace.
func TestCapacityReviewIsDecidedLikeAWorkersStart(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.configure("  reviewSkipPace: true\n")
	r.claude[1] = 51
	a, out := r.app(true)
	c := a.capacityCmd()
	c.SetArgs([]string{"--" + reviewFlag})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	var v capacityView
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Choice == nil || v.Choice.Harness != codex.Harness {
		t.Errorf("capacity --review choice = %+v, want codex", v.Choice)
	}
}

// The one restriction: the review of a self-hosted model's work takes a
// provider that is not self-hosted, while a worker may start on it.
func TestCapacityPickReviewOfSelfHostedWorkTakesNoSelfHostedProvider(t *testing.T) {
	r := newPickRig(t, `    - {harness: omp, model: local/qwen-x, provider: local}
`+subscriptionChoices)
	r.want("claude " + pickOpus)
	r.claude[1], r.codex[1] = 60, 60
	r.want("omp " + pickQwen)
	r.claude[1], r.codex[1] = 40, 40
	r.want("claude "+pickOpus, ofSelfHostedArg)
	r.claude[1], r.codex[1] = 60, 60
	r.want(noPick, ofSelfHostedArg)
	var p struct{ Reason string }
	if err := json.Unmarshal([]byte(r.pick(true, ofSelfHostedArg)), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Reason, "a reviewer on a provider that is not self-hosted") {
		t.Errorf("capacity pick --review-of-self-hosted --json reason = %q", p.Reason)
	}
}

// --force overrides capacity, never the one restriction: a forced review of
// a self-hosted model's work with only the self-hosted choice free does not
// land on it.
func TestForcedReviewOfSelfHostedWorkNeverLandsOnASelfHostedProvider(t *testing.T) {
	r := newPickRig(t, `    - {harness: omp, model: local/qwen-x, provider: local}
`+subscriptionChoices)
	r.claude[1], r.codex[1] = 60, 60
	brief := filepath.Join(r.dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Review the work.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, out := r.app(false)
	c := a.agentStartCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs([]string{"Forced", brief, usageDirFlag, r.dir, testDryRunFlag, "--" + reviewFlag, ofSelfHostedArg, usageForceFlag})
	err := c.Execute()
	if strings.Contains(out.String(), "choice: omp") {
		t.Fatalf("a forced review of self-hosted work landed on the self-hosted provider (err %v):\n%s", err, out.String())
	}
	if err != nil {
		t.Fatalf("forced review start: %v", err)
	}
	if !strings.Contains(out.String(), "choice: claude / "+pickOpus) {
		t.Errorf("forced review start:\n%s", out.String())
	}
}

// A choice's paceMargin never loosens its provider's tighter one.
func TestCapacityPickKeepsTheProvidersTighterPaceMargin(t *testing.T) {
	r := newPickRig(t, `    - {harness: claude, model: opus-x, paceMargin: 5}
    - {harness: codex, model: gpt-x}
`)
	r.claudeExtra = "      paceMargin: -5\n"
	r.configure("")
	r.claude[1] = 47
	r.want(codex.Harness + " " + pickGPT)
	var p struct {
		Capacity struct {
			Choice struct{ Candidates []choiceCandidate }
		}
	}
	if err := json.Unmarshal([]byte(r.pick(true)), &p); err != nil {
		t.Fatal(err)
	}
	if got := p.Capacity.Choice.Candidates[0].Reason; got != "7d 47% within 5 points of pace 50%" {
		t.Errorf("reason under the pace within the margin = %q", got)
	}
}

// agents start naming no harness takes the pick on a desk with choices.
func TestAgentsStartWithoutAHarnessTakesThePick(t *testing.T) {
	r := newPickRig(t, `    - {harness: codex, model: gpt-x}
    - {harness: claude, model: opus-x}
`)
	r.codex[1] = 51
	brief := filepath.Join(r.dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Work the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, out := r.app(false)
	c := a.agentStartCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs([]string{"Picked", brief, usageDirFlag, r.dir, testDryRunFlag})
	if err := c.Execute(); err != nil {
		t.Fatalf("agents start --dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "choice: claude / "+pickOpus+" (provider claude)") {
		t.Errorf("agents start without --harness:\n%s", out.String())
	}
}

// The loop's reviewer of a self-hosted model's work asks for the restriction
// by name, not by excluding providers.
func TestLoopReviewOfASelfHostedWorkerAsksForTheRestriction(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Capacity = config.Capacity{
		Providers: map[string]config.Provider{loopLocal: {Kind: config.KindSelfHosted}, config.ClaudeHarness: {}},
		Choices:   []config.Choice{{Harness: config.OmpHarness, Model: loopQwen, Provider: loopLocal}, {Harness: config.ClaudeHarness, Model: "opus"}},
	}
	r.park(t, loopPR, time.Now())
	_ = r.a.store.Update(func(st *state.State) ([]state.Event, error) { st.Agents[0].Model = loopQwen; return nil, nil })
	r.pass(t)
	start := r.called(agentsName, agentStartName)
	if !slices.Contains(start, ofSelfHostedArg) || slices.Contains(start, "--exclude") {
		t.Errorf("the reviewer start = %v", start)
	}
}

func TestCapacityPickFurthestBehindPace(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.codex[1] = 20
	r.want("codex " + pickGPT)
	var p struct {
		Capacity struct {
			Choice struct {
				Candidates []struct{ BehindPace *float64 }
			}
		}
	}
	if err := json.Unmarshal([]byte(r.pick(true)), &p); err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{10, 30} {
		got := p.Capacity.Choice.Candidates[i].BehindPace
		if got == nil || *got != want {
			t.Errorf("candidate %d behind pace = %v, want %v", i, got, want)
		}
	}
}

func TestCapacityPickPaceTieKeepsConfiguredOrder(t *testing.T) {
	r := newPickRig(t, subscriptionChoices)
	r.codex[1] = 20
	r.want("codex " + pickGPT)
	r.claude[1] = 20
	r.want("claude " + pickOpus)
	r.choices = `    - {harness: codex, model: gpt-x}
    - {harness: claude, model: opus-x}
`
	r.configure("")
	r.want("codex " + pickGPT)
}

func TestCapacityPickUsesChoicesBucketPace(t *testing.T) {
	pace := 50.0
	k := config.Capacity{Providers: map[string]config.Provider{"claude": {}, "codex": {}}, Choices: []config.Choice{
		{Harness: "claude", Model: pickOpus},
		{Harness: "codex", Model: pickGPT, Bucket: "model"},
	}}
	providers := map[string]*providerView{
		"claude": {Room: 1, SevenDay: &usageWindow{UsedPercent: 40, Pace: &pace}},
		"codex": {Room: 1, SevenDay: &usageWindow{UsedPercent: 49, Pace: &pace}, Buckets: map[string]*bucketView{
			"model": {Room: 1, SevenDay: &usageWindow{UsedPercent: 20, Pace: &pace}},
			"other": {Room: 1, SevenDay: &usageWindow{UsedPercent: 50, Pace: &pace}},
		}},
	}
	got := evaluateChoices(k, providers, nil, "", nil, forWorker, time.Now())
	if got.Provider != "codex" {
		t.Fatalf("bucket pick = %+v; candidates = %+v", got, got.Candidates)
	}
}
