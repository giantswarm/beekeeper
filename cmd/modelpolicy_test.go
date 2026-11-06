package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const testTaskAudit = "audit"

func TestModelPolicyPickAndStart(t *testing.T) {
	r := newPickRig(t, `    - {harness: claude, model: opus-x, family: claude, class: 3, cost: 5}
    - {harness: codex, model: gpt-x, family: gpt, class: 2, cost: 1}
`)
	r.want("codex " + pickGPT) // same budget: cheaper model
	r.want("codex "+pickGPT, "--review", "--author-model", pickOpus)
	r.configure("  taskPolicies:\n    review: {differentFamily: true}\n")
	r.want(noPick, "--review")
	r.want("codex "+pickGPT, "--review", "--author-model", pickOpus)
	r.want(noPick, "--review", taskKindArg, testTaskAudit)
	r.want("codex "+pickGPT, "--review", taskKindArg, testTaskAudit, "--author-model", pickOpus)
	// Model filtering must preserve the author's family.
	k := r.cfg.Capacity
	k.TaskKind, k.AuthorModel = taskKindReview, pickOpus
	k = filterChoiceModel(k, pickGPT)
	if why := modelPolicyReason(k, k.Choices[0], nil, "", forReview); why != "" {
		t.Fatal(why)
	}
	r.configure("  taskPolicies:\n    audit: {minClass: 3}\n")
	r.want("claude "+pickOpus, taskKindArg, testTaskAudit)
	// A default rule resolves before the budget gate, and --model overrides it.
	for _, explicit := range []bool{false, true} {
		a, out := r.app(false)
		a.cfg.Agents.Models = []config.AgentModelRule{{Match: "^scan", Model: pickGPT}}
		a.cfg.Agents.RequireModel = true
		brief := filepath.Join(r.dir, "brief.md")
		if err := os.WriteFile(brief, []byte("Inspect the change."), 0o600); err != nil {
			t.Fatal(err)
		}
		c := a.agentStartCmd()
		c.SilenceErrors, c.SilenceUsage = true, true
		args := testCheckoutArgs(t, r.dir, []string{"scan", brief, usageDirFlag, r.dir, testDryRunFlag, usageForceFlag})
		want := pickGPT
		if explicit {
			args = append(args, "--model", pickOpus)
			want = pickOpus
		}
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), modelFlag+" "+want) && !strings.Contains(out.String(), " -m "+want) {
			t.Fatalf("start did not receive model %q: %s", want, out)
		}
	}
}

func TestRequiredModelRefusesBeforeAStart(t *testing.T) {
	a, _ := stubApp(t)
	a.cfg.Agents.RequireModel = true
	c := a.agentStartCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs([]string{"unmatched", "missing-brief", testDryRunFlag})
	err := c.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "agents.requireModel") {
		t.Fatalf("error: %v, code %d", err, Code(err))
	}
}

func TestGuideConfiguredModelForSuccessor(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.now = relayNow
	a.cfg.Agents.Mode = config.ModeBackground
	a.cfg.Agents.RequireModel = true
	a.cfg.Guide.Model = bgModel
	a.cfg.Agents.Model = "worker-fallback"
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		*st = *handOverState()
		st.Starts = append(st.Starts, state.Start{Party: supA, Mode: state.ModeBypass, Model: "old-guide-model"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · Guide run 12\n", bgSession{ID: bgShortID, SessionID: bgID})
	if _, _, err := a.startSuccessor(context.Background(), guideRole, supA, supA, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.Contains(strings.Join((*calls)[0].argv, " "), "--model "+bgModel) {
		t.Fatalf("calls: %+v", *calls)
	}
	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == verbStart })
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e.Detail, "model "+bgModel) {
			found = true
		}
	}
	if !found {
		t.Fatalf("start log did not name model: %+v", events)
	}
}

func TestModelPolicyConstraints(t *testing.T) {
	r := newPickRig(t, `    - {harness: claude, model: opus-x, family: claude, class: 3, cost: 5, reviewReserve: 80}
    - {harness: codex, model: gpt-x, family: gpt, class: 2, cost: 1, kinds: [worker, review]}
`)
	r.want("codex " + pickGPT) // claude's last 80% is held for reviews
	r.want("codex "+pickGPT, "--review")
	r.configure("  taskPolicies:\n    audit: {minClass: 3, maxCost: 2}\n")
	r.want(noPick, taskKindArg, testTaskAudit)
	// A forced pick cannot override capability/cost/family policy.
	a, _ := r.app(false)
	a.selectionKind = testTaskAudit
	v, err := a.startCapacityForAuto(t.Context(), false, nil, forWorker, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pickAutoChoice(v, true); Code(err) != ExitRefused {
		t.Fatalf("force bypassed policy: %v", err)
	}
	brief := filepath.Join(r.dir, "brief.md")
	if err := os.WriteFile(brief, []byte("Audit the change."), 0o600); err != nil {
		t.Fatal(err)
	}
	c := a.agentStartCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs(testCheckoutArgs(t, r.dir, []string{"audit", brief, usageDirFlag, r.dir, testDryRunFlag, usageForceFlag, testHarnessFlag, claudeComm, modelFlag, pickOpus, taskKindArg, testTaskAudit}))
	err = c.Execute()
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), modelPolicyPrefix) {
		t.Fatalf("explicit forced target bypassed policy: %v", err)
	}
	c = a.agentStartCmd()
	c.SilenceErrors, c.SilenceUsage = true, true
	c.SetArgs(testCheckoutArgs(t, r.dir, []string{"audit", brief, usageDirFlag, r.dir, testDryRunFlag, usageForceFlag, testHarnessFlag, claudeComm, modelFlag, "unclassified-model", taskKindArg, testTaskAudit}))
	if err := c.Execute(); Code(err) != ExitRefused || !strings.Contains(err.Error(), "no configured") {
		t.Fatalf("unclassified model bypassed policy: %v", err)
	}
}

func TestModelPolicyLoopSuppliesAuthorModel(t *testing.T) {
	r := newLoopRig(t)
	r.a.cfg.Capacity.TaskPolicies = map[string]config.TaskPolicy{taskKindReview: {DifferentFamily: true}}
	r.a.cfg.Capacity.Choices = []config.Choice{
		{Harness: config.OmpHarness, Model: loopQwen, Family: "writer"},
		{Harness: claudeComm, Model: pickOpus, Family: "independent-family"},
	}
	r.park(t, loopPR, r.a.now)
	if err := r.a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents[0].Model = loopQwen
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.pass(t)
	args := strings.Join(r.called(agentsName, agentStartName), " ")
	if !strings.Contains(args, "--"+authorModelFlag+" "+loopQwen) {
		t.Fatalf("loop omitted author model: %s", args)
	}
}

func TestModelPolicyVaultFlags(t *testing.T) {
	f := parseStartArgsForVault([]string{agentStartName, workerRole, "brief.md", taskKindArg, taskKindReview, "--" + authorModelFlag, pickOpus})
	if f.taskKind != taskKindReview || f.authorModel != pickOpus {
		t.Fatalf("broker selection flags: %+v", f)
	}
}

func TestNameRuleStartsAndRecordsItsModel(t *testing.T) {
	a, _ := stubApp(t)
	plat.Machine = tableMachine{plat.Machine}
	a.cfg.Agents.Mode = config.ModeBackground
	a.cfg.Agents.Models = []config.AgentModelRule{{Match: "^" + bgName + "$", Model: bgModel}}
	a.cfg.Agents.RequireModel = true
	calls := stubBackground(t, "backgrounded · "+bgShortID+" · "+bgName+"\n", bgSession{ID: bgShortID, SessionID: bgID})
	by := state.Party{Session: "model-policy-caller", Name: "model policy"}
	sa, err := a.startAgent(t.Context(), agentStart{name: bgName, role: workerRole, brief: "Return done.", dir: t.TempDir(), by: &by})
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.Contains(strings.Join((*calls)[0].argv, " "), "--model "+bgModel) {
		t.Fatalf("calls: %+v", *calls)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if i := st.StartIndex(state.Party{Session: sa.id}); i < 0 || st.Starts[i].Model != bgModel {
		t.Fatalf("starts: %+v", st.Starts)
	}
}
