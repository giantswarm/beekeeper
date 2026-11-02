package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
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
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Model = pickOpus
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	checked := false
	r.answer = func(args []string) (string, int) {
		if len(args) == 0 || args[0] != loopCapacity {
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
			_, _, err := a.startGateWithOptions(t.Context(), workerRole, config.OmpHarness, policyOmpSelector, false, true, nil, forWorker)
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), modelPolicyPrefix) {
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
		k.AuthorModel, k.TaskKind = policyOmpSelector, taskKindReview
		k = filterChoiceModel(k, model)
		why := modelPolicyReason(k, k.Choices[0], nil, "", forReview)
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
					kind = taskKindReview
				}
				a.cfg.Capacity.Choices[0].Cost = 5
				a.cfg.Capacity.Choices[0].Family = "writer-family"
				a.cfg.Capacity.TaskPolicies = map[string]config.TaskPolicy{kind: tc.policy}
				args := []string{agentStartName, workerRole, brief, loopHostFlag, sshTestSelector, loopRepoFlag, sshTestRepository, loopRefFlag, mainBranch, testHarnessFlag, claudeComm, modelFlag, bgModel, taskKindArg, kind, usageForceFlag}
				if tc.review {
					args = append(args, "--review", "--"+authorModelFlag, bgModel)
				}
				if dry {
					args = append(args, testDryRunFlag)
				}
				_, err := runAgents(a, args...)
				if Code(err) != ExitRefused || !strings.Contains(err.Error(), modelPolicyPrefix) {
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
