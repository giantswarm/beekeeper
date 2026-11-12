package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const policyOmpSelector = "fixture-provider/qwen-x"

// Placement runs the real capacity command and policy evaluator, with only
// provider usage supplied by the fixture. Missing author flags leave no room.
func TestModelPolicyReviewPlacementUsesAuthorCapacity(t *testing.T) {
	p := newPickRig(t, `    - {harness: claude, model: opus-x, family: claude}
    - {harness: codex, model: gpt-x, family: gpt}
`)
	p.configure("  taskPolicies:\n    review: {differentFamily: true}\n")
	r := newLoopRig(t)
	r.a.cfg.Capacity = p.cfg.Capacity
	r.park(t, loopPR, r.a.now)
	r.started(t, loopWorker, "worker-session-1")
	work := ""
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Park.PostMerge = &work
		st.Starts[0].Model = pickOpus
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	checked := false
	r.answer = func(args []string) (string, int) {
		if len(args) == 0 || args[0] != loop.LoopCapacity {
			return "", 0
		}
		checked = true
		a, out := p.app(true)
		c := a.capacityCmd()
		c.SilenceErrors, c.SilenceUsage = true, true
		var flags []string
		for _, arg := range args[1:] {
			if arg != jsonFlag {
				flags = append(flags, arg)
			}
		}
		c.SetArgs(flags)
		if err := c.Execute(); err != nil {
			return out.String(), Code(err)
		}
		return out.String(), 0
	}
	r.pass(t)
	if !checked {
		t.Fatal("review placement did not check capacity")
	}
	if args := r.called(agentsName, agentStartName); len(args) == 0 {
		t.Fatalf("independent reviewer was not started despite eligible capacity: %s", r.out)
	}
}

func TestModelPolicyQualifiedOmpTargetCannotBypass(t *testing.T) {
	for _, tc := range []struct{ name, extra, choice, kind string }{
		{"capability", "  taskPolicies:\n    audit: {minClass: 3}\n", "class: 1", testTaskAudit},
		{"cost", "  taskPolicies:\n    audit: {maxCost: 2}\n", "cost: 5", testTaskAudit},
		{paramKind, "", "kinds: [worker]", testTaskAudit},
		{"reserve", "", "reviewReserve: 80", workerRole},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPickRig(t, "    - {harness: omp, model: qwen-x, provider: claude, "+tc.choice+"}\n"+subscriptionChoices)
			p.configure(tc.extra)
			a, _ := p.app(false)
			a.selectionKind = tc.kind
			_, _, err := a.startGateWithOptions(t.Context(), workerRole, config.OmpHarness, policyOmpSelector, false, true, nil, capacity.ForWorker)
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), capacity.ModelPolicyPrefix) {
				t.Fatalf("qualified selector bypassed %s: %v", tc.name, err)
			}
		})
	}
}

func TestModelPolicyQualifiedOmpAuthorAndFilteredFamily(t *testing.T) {
	p := newPickRig(t, `    - {harness: omp, model: qwen-x, provider: local, family: qwen}
    - {harness: claude, model: opus-x, family: claude}
    - {harness: codex, model: gpt-x, family: qwen}
`)
	p.configure("  taskPolicies:\n    review: {differentFamily: true}\n")
	p.want("claude "+pickOpus, "--review", "--"+authorModelFlag, policyOmpSelector)
	for _, model := range []string{pickOpus, pickGPT} {
		k := p.cfg.Capacity
		k.AuthorModel, k.TaskKind = policyOmpSelector, capacity.TaskKindReview
		k = capacity.FilterChoiceModel(k, model)
		why := capacity.ModelPolicyReason(k, k.Choices[0], nil, "", capacity.ForReview)
		if model == pickOpus && why != "" {
			t.Fatalf("independent filtered choice rejected: %s", why)
		}
		if model == pickGPT && !strings.Contains(why, "different known model family") {
			t.Fatalf("same-family filtered choice: %q", why)
		}
	}
}

func TestModelPolicyForcedSSHAdmissionCannotBypass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy config.TaskPolicy
		review bool
	}{
		{name: "capability", policy: config.TaskPolicy{MinClass: 3}},
		{name: "cost", policy: config.TaskPolicy{MaxCost: 2}},
		{name: "family", policy: config.TaskPolicy{DifferentFamily: true}, review: true},
	} {
		for _, dry := range []bool{true, false} {
			name := tc.name
			if dry {
				name += "-dry-run"
			}
			t.Run(name, func(t *testing.T) {
				a, brief := sshStartTestApp(t)
				a.executionTransport = func(config.ExecutionHost) execution.Transport {
					t.Fatal("policy-rejected start reached SSH transport")
					return nil
				}
				kind := testTaskAudit
				if tc.review {
					kind = capacity.TaskKindReview
				}
				a.cfg.Capacity.Choices[0].Cost = 5
				a.cfg.Capacity.Choices[0].Family = "writer-family"
				a.cfg.Capacity.TaskPolicies = map[string]config.TaskPolicy{kind: tc.policy}
				args := []string{agentStartName, workerRole, brief, loop.LoopHostFlag, sshTestSelector, loop.LoopRepoFlag, sshTestRepository, loop.LoopRefFlag, mainBranch, testHarnessFlag, claudeComm, modelFlag, bgModel, taskKindArg, kind, usageForceFlag}
				if tc.review {
					args = append(args, "--review", "--"+authorModelFlag, bgModel)
				}
				if dry {
					args = append(args, testDryRunFlag)
				}
				_, err := runAgents(a, args...)
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), capacity.ModelPolicyPrefix) {
					t.Fatalf("SSH forced policy bypass: %v", err)
				}
				st, err := a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				if len(st.Starts) != 0 {
					t.Fatal("rejected start reserved an execution")
				}
			})
		}
	}
}

func TestModelPolicyCheckedBeforeReviewCapacityShortcut(t *testing.T) {
	p := newPickRig(t, subscriptionChoices)
	p.configure("  taskPolicies:\n    review: {minClass: 3}\n")
	a, _ := p.app(false)
	a.selectionKind = capacity.TaskKindReview
	v, err := a.startCapacityForAuto(t.Context(), false, nil, capacity.ForReview, "")
	if err != nil {
		t.Fatal(err)
	}
	// A competing start exhausts the review allowance before target admission.
	v.WithReviewHeadroom(0, 1, 1, capacity.ForReview)
	v = v.ForTarget(a.liveCapacity(), claudeComm, pickOpus)
	if _, err := gateStart(v, true); Code(err) != ExitRefused || !strings.Contains(err.Error(), capacity.ModelPolicyPrefix) {
		t.Fatalf("exhausted review room hid target policy: %v", err)
	}
}

func TestModelPolicySSHRequiresMetadataWithoutProviders(t *testing.T) {
	a, brief := sshStartTestApp(t)
	a.cfg.Capacity.Providers, a.cfg.Capacity.Choices = nil, nil
	a.cfg.Capacity.TaskPolicies = map[string]config.TaskPolicy{testTaskAudit: {MinClass: 3}}
	_, err := runAgents(a, agentStartName, workerRole, brief, loop.LoopHostFlag, sshTestSelector,
		loop.LoopRepoFlag, sshTestRepository, loop.LoopRefFlag, mainBranch, testHarnessFlag, claudeComm,
		modelFlag, bgModel, taskKindArg, testTaskAudit, usageForceFlag, testDryRunFlag)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), capacity.ModelPolicyPrefix) {
		t.Fatalf("missing provider metadata hid target policy: %v", err)
	}
}

func TestModelPolicyPlacementHonorsAgentsModelsRule(t *testing.T) {
	// Reviewer placement passes --model when agents.models resolves a model for kind: review.
	r := newLoopRig(t)
	r.a.cfg.Agents.Models = []config.AgentModelRule{{Kind: capacity.TaskKindReview, Model: pickOpus}}
	r.a.cfg.Capacity.Choices = []config.Choice{
		{Harness: claudeComm, Model: pickOpus},
		{Harness: config.OmpHarness, Model: loopQwen},
	}
	r.park(t, loopPR, r.a.now)
	var capArgs []string
	r.answer = func(args []string) (string, int) {
		if len(args) > 0 && args[0] == loop.LoopCapacity {
			capArgs = append([]string(nil), args...)
			return loopRoomJSON, 0
		}
		return "", 0
	}
	r.pass(t)
	if !slices.Contains(capArgs, modelFlag) || !slices.Contains(capArgs, pickOpus) {
		t.Fatalf("reviewer placement did not pass --model %s: %v", pickOpus, capArgs)
	}

	// Worker and fixer placement with supervisor.hosts passes --model and waits when the rule-selected model has no room.
	r2 := newLoopRig(t)
	nextBrief(t, r2)
	r2.sshLoopHost(t, true)
	r2.a.cfg.Agents.Models = []config.AgentModelRule{{Kind: workerRole, Model: bgModel}}
	r2.a.cfg.Capacity.Choices = []config.Choice{
		{Harness: claudeComm, Model: bgModel},
		{Harness: config.OmpHarness, Model: loopQwen},
	}
	r2.park(t, loopPR, time.Now())
	r2.pull.State = superloop.Merged
	r2.pass(t)
	loopNextDone(t, r2)
	branch := loop.GhDefaultBranch
	t.Cleanup(func() { loop.GhDefaultBranch = branch })
	loop.GhDefaultBranch = func(context.Context, string) (string, error) { return mainBranch, nil }
	var workerCapCalls [][]string
	r2.answer = func(args []string) (string, int) {
		if len(args) > 0 && args[0] == loop.LoopCapacity {
			workerCapCalls = append(workerCapCalls, append([]string(nil), args...))
			// Local has no room for bgModel, SSH host has room.
			if slices.Contains(args, loop.LoopHostFlag) {
				if !slices.Contains(args, modelFlag) || !slices.Contains(args, bgModel) {
					t.Fatalf("worker host placement omitted --model %s: %v", bgModel, args)
				}
				return loopRoomJSON, 0
			}
			return loopNoRoomJSON, 0
		}
		if len(args) > 0 && args[0] == loop.LoopBoard {
			return loopNextPickJSON, 0
		}
		return "", 0
	}
	r2.pass(t)
	if !slices.ContainsFunc(workerCapCalls, func(c []string) bool {
		return !slices.Contains(c, loop.LoopHostFlag) && slices.Contains(c, modelFlag) && slices.Contains(c, bgModel)
	}) {
		t.Fatalf("worker local placement omitted --model %s: %v", bgModel, workerCapCalls)
	}
	start := r2.called(agentsName, agentStartName, loop.RepoName(sshTestRepository)+" 9")
	if start == nil || !slices.Contains(start, loop.LoopHostFlag) {
		t.Fatalf("rule-selected Claude worker did not place onto SSH host: %v", start)
	}
}

func TestModelPolicyOmpVaultAndScratchArgvHonorAgentsModels(t *testing.T) {
	a := vaultApp(t, 20*time.Millisecond)
	const qualified = sparkProvider + "/" + loopQwen
	a.cfg.Omp = config.Omp{Providers: map[string]config.OmpProvider{sparkProvider: {APIKey: sparkRef}}}
	a.cfg.Agents.Models = []config.AgentModelRule{{Match: "^" + workerRole, Model: qualified}}
	a.cfg.Capacity = config.Capacity{
		Providers: map[string]config.Provider{
			claudeComm:    {Ceiling: 2},
			sparkProvider: {Kind: config.KindSelfHosted, Ceiling: 2},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: pickOpus},
			{Harness: config.OmpHarness, Model: loopQwen, Provider: sparkProvider, Kinds: []string{workerRole}},
		},
	}
	if !a.ompStartNeedsVault([]string{agentStartName, workerRole, briefMD, testHarnessFlag, config.OmpHarness}) {
		t.Fatal("ompStartNeedsVault ignored agents.models for --harness omp")
	}
	if !a.ompStartNeedsVault([]string{agentStartName, testHarnessFlag + "=" + config.OmpHarness, "--", workerRole, briefMD}) {
		t.Fatal("ompStartNeedsVault ignored agents.models after --")
	}
	if !a.ompStartNeedsVault([]string{agentStartName, workerRole, briefMD, testHarnessFlag, capacity.HarnessAuto}) {
		t.Fatal("ompStartNeedsVault ignored agents.models for --harness auto when non-vault choice ranked first")
	}
	c := a.agentStartCmd()
	if err := c.ParseFlags([]string{testHarnessFlag, config.OmpHarness}); err != nil {
		t.Fatal(err)
	}
	argv, err := a.scratchArgv(c, []string{workerRole, briefMD})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(argv, "--model="+qualified) {
		t.Fatalf("scratchArgv omitted qualified rule model: %v", argv)
	}
}

func TestModelPolicyStartAgentDoesNotApplyWorkerRuleToOtherKinds(t *testing.T) {
	a, out := stubApp(t)
	a.cfg.Agents.Models = []config.AgentModelRule{{Kind: workerRole, Model: "worker-only-sonnet"}}
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("Review https://github.com/example/repo/pull/1.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{
		{"--review"},
		{taskKindArg, testTaskAudit},
	} {
		out.Reset()
		c := a.agentStartCmd()
		c.SilenceErrors, c.SilenceUsage = true, true
		args := testCheckoutArgs(t, filepath.Dir(brief), append([]string{"rev", brief, usageDirFlag, filepath.Dir(brief), testHarnessFlag, claudeComm, testDryRunFlag, usageForceFlag}, extra...))
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "worker-only-sonnet") {
			t.Fatalf("start %v inherited worker-only rule model: %s", extra, out.String())
		}
	}
}
