//go:build unix

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A wake of an omp agent goes to its inbox while its process reads it, and
// is refused once none does: nothing resumes an omp agent.
func TestWakeOmp(t *testing.T) {
	const id = "1234abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &out}
	ag := state.Agent{Party: state.Party{HostSession: omp.HostPrefix + id, Name: "test: omp"}}
	by := state.Party{Name: "test"}
	inbox := omp.InboxPath(dir, id)
	if err := omp.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := a.wakeOmp(by, ag, id, "hello"); err == nil || !strings.Contains(err.Error(), "no longer runs") {
		t.Fatalf("wake with no reader = %v, want the refusal", err)
	}
	r, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // the test's inbox
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := a.wakeOmp(by, ag, id, "hello"); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(r).ReadString('\n'); !strings.Contains(line, `"message":"hello"`) {
		t.Fatalf("inbox got %q", line)
	}
	if !strings.Contains(out.String(), "written to its inbox") {
		t.Fatalf("output %q", out.String())
	}
	// A busy state lock delays the agents.wake event, never drops it.
	a.store = busyStore{store}
	if err := a.wakeOmp(by, ag, id, "again"); err != nil {
		t.Fatal(err)
	}
	if evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == "agents.wake" }); err != nil || len(evs) != 2 {
		t.Errorf("agents.wake events under a busy lock = %+v, %v; want both wakes", evs, err)
	}
}

func TestRoleTextNamesTheHarness(t *testing.T) {
	sv := &sessionView{Session: newOmpSession()}
	if got := roleText(sv, "agent: fix it"); got != "omp busy agent: fix it" {
		t.Fatalf("roleText = %q", got)
	}
}

func newOmpSession() *claude.Session {
	return &claude.Session{Harness: omp.Harness, State: omp.StateBusy}
}

// An omp agent's model is named, from --model or omp.model, and is one
// omp lists by its exact selector: no default, no pattern.
func TestOmpModel(t *testing.T) {
	const local, remote = "ollama/qwen3.5:9b", "vllm/qwen3-8b"
	listed := func() ([]string, error) { return []string{local, remote}, nil }
	for _, tc := range []struct {
		flag, configured, want, err string
	}{
		{configured: local, want: local},
		{flag: remote, configured: local, want: remote},
		{err: "needs its model"},
		{configured: "qwen3.5", err: `lists no model "qwen3.5"`},
	} {
		got, err := ompModel(tc.flag, tc.configured, listed)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ompModel(%q, %q) = %q, %v; want the error %q", tc.flag, tc.configured, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ompModel(%q, %q) = %q, %v; want %q", tc.flag, tc.configured, got, err, tc.want)
		}
	}
	if _, err := ompModel("", local, func() ([]string, error) { return nil, errors.New("omp failed") }); err == nil {
		t.Error("an unreadable model list passed")
	}
	if argv := ompArgv("omp", local); !slices.Equal(argv[len(argv)-2:], []string{modelFlag, local}) {
		t.Errorf("ompArgv = %v", argv)
	}
}

// An omp agent on a provider of omp.providers gets the provider's key from
// the reference, under the variable the models file names; a start on a
// key the file carries while a reference names it, on a variable no
// reference fills, or with no variable named is refused; a provider the
// config does not know starts on omp's own key as before.
func TestOmpCredentials(t *testing.T) {
	const planted = "planted-key-7b2e" //nolint:gosec // a planted test value
	const ref = sparkRef
	write := func(t *testing.T, raw string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "models.yml")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, tc := range map[string]struct {
		models, model, ref, wantName, err string
	}{
		"from the vault":    {models: "providers: {spark: {apiKey: SPARK_API_KEY}}", model: sparkModel, ref: ref, wantName: "SPARK_API_KEY"},
		"keyless provider":  {models: "providers: {local: {auth: none}}", model: "local/m"},
		"omp's own key":     {models: "providers: {spark: {apiKey: " + planted + "}}", model: sparkModel},
		"built-in provider": {models: "providers: {}", model: "amazon-bedrock/m"},
		"value and ref":     {models: "providers: {spark: {apiKey: " + planted + "}}", model: sparkModel, ref: ref, err: "replace the value"},
		"ref without name":  {models: "providers: {spark: {baseUrl: x}}", model: sparkModel, ref: ref, err: "names no variable"},
		"name without ref":  {models: "providers: {spark: {apiKey: SPARK_API_KEY}}", model: sparkModel, err: "fills from no reference"},
	} {
		cfg := &config.Config{Omp: config.Omp{ModelsFile: write(t, tc.models)}}
		if tc.ref != "" {
			cfg.Omp.Providers = map[string]config.OmpProvider{sparkProvider: {APIKey: tc.ref}}
		}
		a := &app{cfg: cfg}
		creds, err := a.ompCredentials(tc.model)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: %v, want the error %q", name, err, tc.err)
			}
			if err != nil && strings.Contains(err.Error(), planted) {
				t.Errorf("%s: the error carries the value: %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		switch {
		case tc.wantName == "" && len(creds) != 0:
			t.Errorf("%s: credentials %+v, want none", name, creds)
		case tc.wantName != "" && (len(creds) != 1 || creds[0].Name != tc.wantName || creds[0].Ref.Op != tc.ref):
			t.Errorf("%s: credentials %+v", name, creds)
		}
	}
}

// The tests' vault provider: a model of it and its key's reference.
const (
	sparkProvider   = "spark"
	sparkModel      = "spark/m"
	sparkRef        = "op://Shared/spark/credential" //nolint:gosec // a reference, no value
	autoHarnessFlag = "--harness=auto"
)

// A brokered omp start on a provider of omp.providers reads the vault,
// by its model, omp.model or the actual resolved --harness=auto choice; an
// eligible non-vault choice ahead of a vault-backed omp fallback does not.
func TestOmpStartNeedsVault(t *testing.T) {
	const ompHarness = "--harness=omp"
	const autoHarness = autoHarnessFlag
	const claudeStartedOut = "claude started"
	a := vaultApp(t, 20*time.Millisecond)
	a.cfg.Omp = config.Omp{Model: sparkModel, Providers: map[string]config.OmpProvider{sparkProvider: {APIKey: sparkRef}}}
	a.cfg.Capacity = config.Capacity{
		Floor:   1,
		Ceiling: 2,
		Providers: map[string]config.Provider{
			claudeComm:  {Floor: 1, Ceiling: 1},
			omp.Harness: {Floor: 1, Ceiling: 1},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: testModelOpus47},
			{Harness: omp.Harness, Model: sparkModel, Provider: omp.Harness},
		},
	}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{agentStartName, ompHarness, "--", "w", briefMD}, true},
		{[]string{agentStartName, ompHarness, "--model=spark/other", "--", "w", briefMD}, true},
		{[]string{agentStartName, ompHarness, "--model=ollama/m", "--", "w", briefMD}, false},
		// First choice is eligible claude/claude-opus-4-7, so auto does not need the vault.
		{[]string{agentStartName, autoHarness, "--", "w", briefMD}, false},
		// Excluding claude selects the vault-backed omp fallback, which needs the vault.
		{[]string{agentStartName, autoHarness, "--exclude=[claude]", "--", "w", briefMD}, true},
		{[]string{agentStartName, autoHarness, "--model=" + sparkModel, "--", "w", briefMD}, true},
		{[]string{agentStartName, autoHarness, "--model=" + sparkModel, "--exclude=[omp]", "--", "w", briefMD}, false},
		{[]string{agentStartName, autoHarness, "--model=ollama/m", "--", "w", briefMD}, false},
		{[]string{agentStartName, "--", "w", briefMD}, false},
		{[]string{"wake", "--", "w", "hi"}, false},
		{[]string{agentStartName, "--", ompHarness, briefMD}, false},
	} {
		if got := a.ompStartNeedsVault(tc.args); got != tc.want {
			t.Errorf("ompStartNeedsVault(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
	// With a locked vault and an eligible first claude choice ahead of an unused
	// vault-backed omp fallback, brokeredVault runs the start handler without
	// asking the vault; excluding claude selects omp and waits on the vault.
	k := secret.NewKeeper(time.Hour, nil)
	v := &vaultBroker{k: k, wait: 20 * time.Millisecond, signin: func(context.Context) (string, string, error) {
		return "", "", errors.New("vault locked")
	}}
	called := false
	h := a.brokeredVault(v, func(env []string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			called = true
			if env != nil {
				t.Errorf("expected nil vault env for claude choice, got %q", env)
			}
			return sandbox.Reply{Out: claudeStartedOut}, nil
		}
	})
	r, err := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpAgents, Args: []string{agentStartName, autoHarness, "--", "w", briefMD}})
	if err != nil || r.Code != 0 || !called || r.Out != claudeStartedOut {
		t.Fatalf("brokeredVault with eligible claude choice and locked vault: r=%+v, err=%v, called=%v", r, err, called)
	}
	called = false
	rOmp, err := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpAgents, Args: []string{agentStartName, autoHarness, "--exclude=claude", "--", "w", briefMD}})
	if err != nil || rOmp.Code != ExitVault || called {
		t.Fatalf("brokeredVault when excluded claude falls back to omp on locked vault: r=%+v, err=%v, called=%v", rOmp, err, called)
	}

	// Forwarding a resolved --harness=auto choice rewrites the command's
	// flags to --harness=omp, --model=<targetModel>, and --choice=<line> for scratchArgv.
	c := a.agentStartCmd()
	if err := c.ParseFlags([]string{autoHarness}); err != nil {
		t.Fatal(err)
	}
	if err := forwardBrokeredChoice(c, omp.Harness, sparkModel, "omp / spark/m (provider omp)"); err != nil {
		t.Fatal(err)
	}
	argv, err := a.scratchArgv(c, []string{"w", briefMD})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, ompHarness) || !slices.Contains(argv, "--model="+sparkModel) ||
		!slices.Contains(argv, "--choice=omp / spark/m (provider omp)") || !a.ompStartNeedsVault(argv) {
		t.Fatalf("forwarded scratchArgv = %q, ompStartNeedsVault = %v", argv, a.ompStartNeedsVault(argv))
	}
	if !a.ompStartBrokered(sparkModel) {
		t.Error("expected brokered with secret.session")
	}
	if !a.ompStartBrokered("") || a.ompStartBrokered("ollama/m") {
		t.Error("the brokering does not follow the provider")
	}
	a.cfg.Secret.Session = false
	if a.ompStartBrokered(sparkModel) {
		t.Error("brokered without secret.session")
	}
	a.cfg.Secret.Session = true
	t.Setenv(sandbox.Brokered, "1")
	if a.ompStartBrokered(sparkModel) {
		t.Error("the broker's own call brokered again")
	}
}

// An omp agent's beekeeper commands act as its roster entry, even where its
// tool shell carries another session's variables: agents idle --done
// finishes it.
func TestOmpAgentIsTheCaller(t *testing.T) {
	const id = "1234abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "some-other-session")
	t.Setenv(omp.EnvAgent, id)
	t.Setenv(omp.EnvName, "omp worker")
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &bytes.Buffer{}}
	me, err := a.caller()
	if err != nil {
		t.Fatal(err)
	}
	ag := state.Agent{Party: state.Party{HostSession: omp.HostPrefix + id, Name: "omp worker"}}
	if !ag.Is(me) || me.Session != "" {
		t.Fatalf("caller = %+v, want the omp agent's roster entry", me)
	}
}

func TestAutoStartNeedsVaultScratchConfigIsolation(t *testing.T) {
	a := vaultApp(t, 20*time.Millisecond)
	a.cfg.StateDir = a.store.Dir()
	marker := filepath.Join(t.TempDir(), "pwned")
	missingState := filepath.Join(t.TempDir(), "missing-state")
	scratchCfg := filepath.Join(t.TempDir(), "scratch.yaml")
	rawScratch := `stateDir: ` + missingState + `
omp:
  model: ` + sparkModel + `
  providers:
    spark:
      apiKey: ` + sparkRef + `
capacity:
  floor: 1
  ceiling: 2
  providers:
    spark:
      floor: 1
      ceiling: 1
      command: ["sh", "-c", "touch ` + marker + ` && echo '{\"fiveHour\":{\"usedPercent\":10,\"resetsAt\":\"2099-01-01T00:00:00Z\"}}'"]
      fiveHour:
        - {upTo: 90, floor: 1, ceiling: 1}
  choices:
    - {harness: omp, model: ` + sparkModel + `, provider: spark}
`
	if err := os.WriteFile(scratchCfg, []byte(rawScratch), 0o600); err != nil {
		t.Fatal(err)
	}

	const autoHarness = autoHarnessFlag
	// Host config has no capacity.choices or omp.providers: autoStartNeedsVault
	// must never copy Capacity/Omp from scratchCfg, never run scratch usage
	// commands on the host, and never create missingState.
	if a.ompStartNeedsVault([]string{agentStartName, autoHarness, "--config=" + scratchCfg, "--", "w", briefMD}) {
		t.Fatal("ompStartNeedsVault copied Capacity/Omp from scratch config")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch usage command ran on host: marker stat err = %v", err)
	}
	if _, err := os.Stat(missingState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing scratch stateDir was created on host: stat err = %v", err)
	}

	// Denied paths under the state's scanDir (or outside sandbox allow lists) are rejected.
	deniedCfg := filepath.Join(a.scanDir(), "denied.yaml")
	if err := os.MkdirAll(a.scanDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deniedCfg, []byte(rawScratch), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Omp = config.Omp{Model: sparkModel, Providers: map[string]config.OmpProvider{sparkProvider: {APIKey: sparkRef}}}
	a.cfg.Capacity = config.Capacity{
		Floor:   1,
		Ceiling: 2,
		Providers: map[string]config.Provider{
			claudeComm:  {Floor: 1, Ceiling: 1},
			omp.Harness: {Floor: 1, Ceiling: 1},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: testModelOpus47},
			{Harness: omp.Harness, Model: sparkModel, Provider: omp.Harness},
		},
	}
	if a.ompStartNeedsVault([]string{agentStartName, autoHarness, "--config=" + deniedCfg, "--", "w", briefMD}) {
		t.Fatal("ompStartNeedsVault accepted a scratch config inside denied scanDir")
	}
}

func TestAutoStartNeedsVaultZeroNowPace(t *testing.T) {
	const autoHarness = autoHarnessFlag
	a := vaultApp(t, 20*time.Millisecond)
	// Leave a.now at zero (time.Time{}), as sandboxCmd's PersistentPreRunE
	// (loadConfig) leaves it on the host broker.
	a.now = time.Time{}
	now := time.Now().UTC()
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(84 * time.Hour) // 50% of 7d elapsed -> pace is 50% at time.Now(), 0% at 0001-01-01.
	a.readUsage = func(context.Context, []string, time.Duration) (usageReading, error) {
		return usageReading{
			FiveHour: &usageWindow{UsedPercent: 10, ResetsAt: reset5h},
			SevenDay: &usageWindow{UsedPercent: 25, ResetsAt: reset7d},
		}, nil
	}
	a.cfg.Omp = config.Omp{Model: sparkModel, Providers: map[string]config.OmpProvider{sparkProvider: {APIKey: sparkRef}}}
	a.cfg.Capacity = config.Capacity{
		Floor:   1,
		Ceiling: 2,
		Providers: map[string]config.Provider{
			claudeComm: {
				Floor:   1,
				Ceiling: 1,
				Usage: config.Usage{
					Command:  []string{"read-claude"},
					FiveHour: []config.UsageBand{{UpTo: 90, Floor: 1, Ceiling: 1}},
					SevenDay: []config.UsageBand{{UpTo: 90, Floor: 1, Ceiling: 1}},
				},
			},
			omp.Harness: {Floor: 1, Ceiling: 1},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: testModelOpus47},
			{Harness: omp.Harness, Model: sparkModel, Provider: omp.Harness},
		},
	}
	if a.ompStartNeedsVault([]string{agentStartName, autoHarness, "--", "w", briefMD}) {
		t.Fatal("ompStartNeedsVault rejected eligible 7d pace choice when a.now was zero")
	}
	k := secret.NewKeeper(time.Hour, nil)
	v := &vaultBroker{k: k, wait: 20 * time.Millisecond, signin: func(context.Context) (string, string, error) {
		return "", "", errors.New("vault locked")
	}}
	called := false
	h := a.brokeredVault(v, func(env []string) sandbox.Handler {
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			called = true
			return sandbox.Reply{Out: "ok"}, nil
		}
	})
	r, err := h(context.Background(), os.Getpid(), sandbox.Request{Op: sandbox.OpAgents, Args: []string{agentStartName, autoHarness, "--", "w", briefMD}})
	if err != nil || r.Code != 0 || !called {
		t.Fatalf("brokeredVault with zero a.now and locked vault: r=%+v, err=%v, called=%v", r, err, called)
	}
}

// capacity pick returns an omp choice by its bare model id, as
// capacity.choices names it: the start takes it as the one provider/id omp
// lists for that id, so the start's model check and its vault and broker
// decision follow that provider; an id two providers list is refused with
// both, and an id omp does not list at all still is.
func TestOmpBareModelFromPick(t *testing.T) {
	const bare = "m"
	listed := []string{sparkModel, "ollama/qwen3.5:9b"}
	for _, tc := range []struct {
		model  string
		models []string
		want   string
		err    string
	}{
		{model: bare, models: listed, want: sparkModel},
		{model: sparkModel, models: listed, want: sparkModel},
		{model: "qwen3.5:9b", models: listed, want: "ollama/qwen3.5:9b"},
		{model: bare, models: append(listed, "ollama/m"), err: "under more than one provider (spark/m, ollama/m)"},
		{model: "unlisted", models: listed, err: `lists no model "unlisted"`},
		{model: "ollama/m", models: listed, err: `lists no model "ollama/m"`},
	} {
		got, err := ompModel(tc.model, "", func() ([]string, error) { return tc.models, nil })
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ompModel(%q) = %q, %v; want the error %q", tc.model, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ompModel(%q) = %q, %v; want %q", tc.model, got, err, tc.want)
		}
	}

	a := vaultApp(t, 20*time.Millisecond)
	a.cfg.Omp = config.Omp{Providers: map[string]config.OmpProvider{sparkProvider: {APIKey: sparkRef}}}
	a.ompModelList = func(context.Context) ([]string, error) { return listed, nil }
	if !a.ompNeedsVault(bare) || !a.ompStartBrokered(bare) {
		t.Errorf("a bare id omp lists under %s does not read the vault through the broker", sparkProvider)
	}
	if !a.ompStartNeedsVault([]string{agentStartName, "--harness=omp", "--model=" + bare, "--", "w", briefMD}) {
		t.Error("the broker does not open the vault for a start on the pick's bare id")
	}
	if a.ompNeedsVault("qwen3.5:9b") {
		t.Error("a bare id on a provider outside omp.providers reads the vault")
	}
	a.ompModelList = func(context.Context) ([]string, error) { return nil, errors.New("omp failed") }
	if a.ompNeedsVault(bare) {
		t.Error("an unlisted bare id reads the vault")
	}
}

// A worker started on the pick's bare id records the provider/id omp lists
// for it: the loop still takes it for a self-hosted model's worker (so its
// review carries --review-of-self-hosted), capacity counts it under the
// choice's own provider even with two self-hosted providers, and the start
// gate keeps the choice for the qualified model.
func TestOmpQualifiedModelMatchesItsBareChoice(t *testing.T) {
	const id, recorded, rackB = "flash-id", "vllm/flash-id", "rack-b"
	k := config.Capacity{
		Providers: map[string]config.Provider{
			claudeComm: {Floor: 1, Ceiling: 1},
			"rack-a":   {Kind: config.KindSelfHosted, Ceiling: 2},
			rackB:      {Kind: config.KindSelfHosted, Ceiling: 2},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: testModelOpus47},
			{Harness: omp.Harness, Model: "other-id", Provider: "rack-a"},
			{Harness: omp.Harness, Model: id, Provider: rackB},
		},
	}
	if !selfHostedModel(k, recorded) {
		t.Errorf("selfHostedModel(%q) with the self-hosted choice %q = false", recorded, id)
	}
	if selfHostedModel(k, "vllm/unlisted-id") || selfHostedModel(k, "vllm/x"+id) {
		t.Error("selfHostedModel matched a model no choice names")
	}
	if p, _ := resolveAgentProviderBucket(k, omp.Harness, recorded); p != rackB {
		t.Errorf("resolveAgentProviderBucket(omp, %q) = %q, want %s", recorded, p, rackB)
	}
	if !choicesHaveModel(k.Choices, recorded) {
		t.Errorf("choicesHaveModel(%q) = false", recorded)
	}
	if f := filterChoiceModel(k, recorded); len(f.Choices) != 1 || f.Choices[0].Model != id {
		t.Errorf("filterChoiceModel(%q) = %+v, want the %q choice", recorded, f.Choices, id)
	}
	// a subscription choice is never matched by an id after a slash
	if ompChoiceModel(config.Choice{Harness: claudeComm, Model: bgModel}, "x/"+bgModel) {
		t.Error("a claude choice matched its id after a slash")
	}
}
