package cmd

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

const agyTestModel = "gemini-3.1-pro-high"

func sshAgyTestApp(t *testing.T, offered map[string]string) (*app, string, *[]execution.StartRequest) {
	t.Helper()
	a, brief := sshStartTestApp(t)
	localCeiling := 1
	a.cfg.Capacity = config.Capacity{
		Ceiling: 10,
		Local:   config.LocalCapacity{Ceiling: &localCeiling},
		Providers: map[string]config.Provider{
			claudeComm: {Ceiling: 1},
			harnessAgy: {Ceiling: 2, Usage: config.Usage{Command: config.DefaultAgyUsageCommand}},
		},
		Choices: []config.Choice{
			{Harness: claudeComm, Model: bgModel},
			{Harness: harnessAgy, Model: agyTestModel},
		},
	}
	stubSubscriptionUsage(a)
	h := a.cfg.Agents.Hosts[sshTestSelector]
	h.Harnesses = []string{claudeComm, harnessAgy}
	a.cfg.Agents.Hosts[sshTestSelector] = h
	a.executionProbe = func(_ context.Context, h execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{CIWait: true, SourcePreparation: true, Publication: true, Protocol: execution.Protocol, Identity: h.Identity, Version: project.Version(), PolicyDigest: h.PolicyDigest, Harnesses: offered, Unavailable: map[string]string{}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
	}
	var requests []execution.StartRequest
	var handle execution.Handle
	fake := &testExecutionTransport{}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		requests = append(requests, r)
		handle = execution.Handle{Ref: r.Ref, Repository: r.Repository, Commit: strings.Repeat("a", 40), Workspace: path.Join(a.cfg.Agents.Hosts[sshTestSelector].WorkspaceRoot, string(r.Ref.Execution), "workspace"), Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}
		return handle, nil
	}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		if len(requests) < 2 {
			return execution.Observation{Handle: handle, ProcessState: execution.Prepared}, nil
		}
		return execution.Observation{Handle: handle, ProcessState: execution.Running, Ready: true}, nil
	}
	a.executionTransport = func(execution.Host) execution.Transport { return fake }
	return a, brief, &requests
}

func TestExecutionStartsAgyOnAHostThatOffersItAndCountsProviderNotLocalCeiling(t *testing.T) {
	a, brief, requests := sshAgyTestApp(t, map[string]string{claudeComm: codexTestClaudeVer, harnessAgy: testAgyVersion})
	if _, err := runAgents(a, agentStartName, "--name", "remote-agy", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAgy, "--model", agyTestModel); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != harnessAgy || (*requests)[1].Model != agyTestModel || !(*requests)[1].Messaging || !(*requests)[1].LeaseRequired {
		t.Fatalf("remote agy request: %+v", *requests)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Starts[0].Harness != harnessAgy || st.Starts[0].Execution.Provider != harnessAgy {
		t.Fatalf("remote agy is recorded against %q/%q", st.Starts[0].Harness, st.Starts[0].Execution.Provider)
	}
	// Remote agy counts against the agy provider ceiling, never the coordinator's local ceiling.
	counts := countAgents(st, nil, a.now, a.cfg)
	pcounts := countProviderAgents(st, nil, a.cfg.Capacity, a.now, a.cfg)
	if len(pcounts[harnessAgy].Busy) != 1 || len(pcounts[claudeComm].Busy) != 0 || len(localBusy(st, counts.Busy)) != 0 {
		t.Fatalf("remote agy counted against local=%v provider=%+v", localBusy(st, counts.Busy), pcounts)
	}
	v := buildProvidersCapacity(counts, pcounts, a.cfg.Capacity, a.executionReadings(context.Background(), a.cfg.Capacity), &headroom{AvailableMiB: 1 << 16, Slots: 1, SlotsFree: 1}, true, nil, forWorker, a.now).withLocal(st, a.cfg.Capacity)
	if v.Local == nil || len(v.Local.Busy) != 0 || v.Local.Room != 1 || len(v.Providers[harnessAgy].Busy) != 1 || v.Providers[harnessAgy].Room != 1 {
		t.Fatalf("capacity view: local=%+v agy=%+v", v.Local, v.Providers[harnessAgy])
	}
	if _, err := runAgents(a, agentStartName, "--name", "remote-agy-2", brief, "--host", sshTestOtherSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAgy, "--model", agyTestModel); err == nil {
		t.Fatal("a host that does not list agy started it")
	}
}

func TestExecutionRefusesAgyTheHostDoesNotOffer(t *testing.T) {
	a, brief, requests := sshAgyTestApp(t, map[string]string{claudeComm: codexTestClaudeVer})
	_, err := runAgents(a, agentStartName, "--name", "remote-agy", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAgy, "--model", agyTestModel)
	if err == nil || !strings.Contains(err.Error(), "does not offer harness agy") || len(*requests) != 0 {
		t.Fatalf("a host whose agy check failed started it: %v", err)
	}
	if _, err := runAgents(a, agentStartName, "--name", "remote-auto", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAuto); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != claudeComm {
		t.Fatalf("auto picked %+v", *requests)
	}
}

func TestExecutionAutoPicksRemoteAgy(t *testing.T) {
	a, brief, requests := sshAgyTestApp(t, map[string]string{harnessAgy: testAgyVersion})
	if _, err := runAgents(a, agentStartName, "--name", "remote-auto", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAuto); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != harnessAgy || (*requests)[1].Model != agyTestModel {
		t.Fatalf("auto on an agy-only host picked %+v", *requests)
	}
}

// An effort agy would ignore must never reach a request, a start record or an
// event, however it was selected and whichever way agy was chosen.
func TestExecutionRefusesEffortOnRemoteAgy(t *testing.T) {
	remote := []string{LoopHostFlag, sshTestSelector, LoopRepoFlag, sshTestRepository, LoopRefFlag, mainBranch}
	explicit := []string{testHarnessFlag, harnessAgy, modelFlag, agyTestModel}
	auto := []string{testHarnessFlag, harnessAuto}
	flag := []string{effortFlag, testEffortHigh}
	for _, tc := range []struct {
		name   string
		args   []string
		models []config.AgentModelRule
	}{
		{name: "explicit flag", args: slices.Concat(explicit, flag)},
		{name: "explicit rule", args: explicit, models: []config.AgentModelRule{{Match: "^remote", Effort: testEffortHigh}}},
		{name: "explicit dry run", args: slices.Concat(explicit, flag, []string{testDryRunFlag})},
		{name: "auto flag", args: slices.Concat(auto, flag)},
		{name: "auto rule", args: auto, models: []config.AgentModelRule{{Match: "^remote", Effort: testEffortHigh}}},
		{name: "auto dry run", args: slices.Concat(auto, flag, []string{testDryRunFlag})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, brief, requests := sshAgyTestApp(t, map[string]string{harnessAgy: testAgyVersion})
			a.cfg.Agents.Models = tc.models
			args := slices.Concat([]string{agentStartName, nameFlag, "remote-agy", brief}, remote, tc.args)
			_, err := runAgents(a, args...)
			if err == nil || !strings.Contains(err.Error(), "agy does not expose a session effort option") {
				t.Fatalf("remote agy accepted an effort: %v", err)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(*requests) != 0 || len(st.Starts) != 0 || len(st.Agents) != 0 {
				t.Fatalf("refused start left requests=%+v starts=%+v agents=%+v", *requests, st.Starts, st.Agents)
			}
		})
	}
}

// The refusal follows the harness auto resolved, not the choices it could have made.
func TestExecutionAutoKeepsEffortOnAHarnessThatTakesIt(t *testing.T) {
	a, brief, requests := sshAgyTestApp(t, map[string]string{claudeComm: codexTestClaudeVer})
	if _, err := runAgents(a, agentStartName, "--name", "remote-auto", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, testHarnessFlag, harnessAuto, effortFlag, testEffortHigh); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != claudeComm || (*requests)[1].Effort != testEffortHigh {
		t.Fatalf("auto on a Claude host: %+v", *requests)
	}
}

func TestExecutionPublicationAndMergeAcceptRemoteAgy(t *testing.T) {
	a, _ := sshStartTestApp(t)
	owner, _, _ := strings.Cut(sshTestRepository, "/")
	a.cfg.GitHub.AllowedOrgs = []string{owner}
	ref := execution.Ref{Host: sshTestSelector, Execution: "3aa27721-fb1e-4f07-b0aa-bdf100e0fdd9", Generation: 1}
	rec := &state.ExecutionRecord{Handle: execution.Handle{Ref: ref, Commit: strings.Repeat("a", 40)}, Request: execution.StartRequest{Ref: ref, Repository: sshTestRepository, Harness: harnessAgy, LeaseRequired: true}, Lease: &execution.LeaseGrant{ExpiresAt: time.Now().Add(time.Hour)}}
	st := &state.State{Starts: []state.Start{{Party: state.Party{Name: "remote-agy-worker"}, Execution: rec}}}
	if _, err := a.executionPublicationLive(st, ref); err != nil {
		t.Fatalf("publication for agy: %v", err)
	}
	err := a.executionMergeLive(context.Background(), st, execution.MergeUse{Ref: ref, Repository: sshTestRepository, PR: 1, Head: strings.Repeat("b", 40)})
	if err != nil && strings.Contains(err.Error(), "authority stopped or mismatched") {
		t.Fatalf("merge refused remote agy by harness: %v", err)
	}
}

func TestExecutionGuardDecisionMatchesAgyHookFormat(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	thread := "019a0000-0000-7000-8000-000000000001"
	agyRaw := []byte(`{"conversationId":"` + thread + `","workspacePaths":["` + workspace + `"],"toolCall":{"name":"run_command","args":{"CommandLine":"git status"}}}`)
	deniedAgy := func(b []byte) bool { return strings.Contains(string(b), `"decision":"deny"`) }
	convID := "c0ffee01-0000-4000-8000-000000000000"
	agyJournal := execution.Journal{Request: execution.StartRequest{Harness: harnessAgy, ConversationID: convID}, Handle: execution.Handle{Workspace: workspace, ConversationID: convID}, Thread: thread}
	if out := executionGuardDecision(guard.Hook{}, harnessAgy, agyJournal, nil)(agyRaw); deniedAgy(out) {
		t.Fatalf("agy hook on its own thread was denied: %s", out)
	}
	unrecorded := agyJournal
	unrecorded.Thread = ""
	if out := executionGuardDecision(guard.Hook{}, harnessAgy, unrecorded, nil)(agyRaw); !deniedAgy(out) {
		t.Fatal("agy hook passed before its thread was recorded")
	}
	for _, otherFormat := range []string{"", codex.Harness} {
		if out := executionGuardDecision(guard.Hook{}, otherFormat, agyJournal, nil)(agyRaw); !strings.Contains(string(out), "deny") {
			t.Fatalf("format %q hook ran in an agy execution: %s", otherFormat, out)
		}
	}
	claudeJournal := agyJournal
	claudeJournal.Request.Harness = claudeComm
	if out := executionGuardDecision(guard.Hook{}, harnessAgy, claudeJournal, nil)(agyRaw); !deniedAgy(out) {
		t.Fatal("agy-format hook ran in a claude execution")
	}

	// Verify loadConfig + InstalledPolicy + executionHook succeed when child
	// processes inherit HOME=<s.Root>/agy-home and BEEKEEPER_EXECUTION_HOME.
	realHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", realHome)
	t.Setenv("XDG_STATE_HOME", "")
	rulesPath := filepath.Join(realHome, "host-rules.md")
	if err := os.WriteFile(rulesPath, []byte("fixture host rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := execution.SigningKey(filepath.Join(realHome, "coordinator"))
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(realHome, "config.yaml")
	cfgYAML := "identity:\n  person: fixture\nagents:\n  execution:\n    identity: " + sshTestSelector + "\n    rulesFile: " + rulesPath + "\n    workspaceRoot: " + filepath.Join(realHome, "workspaces") + "\n    repositories:\n      " + sshTestRepository + ": " + filepath.Join(realHome, "source") + "\n    harnesses:\n      agy:\n        executable: /usr/local/bin/agy\n        version: 1.4.7\n        configDir: " + filepath.Join(realHome, "agy-login") + "\n    coordinators:\n      " + sshTestCoordinator + ":\n        publicKey: " + execution.PublicKey(key) + "\n        callback:\n          ssh: guard-coordinator.example\n          executable: " + sshTestExecutable + "\n          config: " + sshTestConfig + "\n    maxBusy: 1\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	prepApp := &app{cfgPath: cfgPath, hook: true}
	if err := prepApp.load(); err != nil {
		t.Fatal(err)
	}
	prepSrv, err := prepApp.hostRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepSrv.Prepare(prepSrv.Policy()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(prepApp.cfg.StateDir, "execution", "agy-home"))
	t.Setenv(execution.EnvHome, realHome)
	guardApp := &app{cfgPath: cfgPath, hook: true}
	if err := guardApp.load(); err != nil {
		t.Fatalf("guard load under agy HOME override: %v", err)
	}
	guardSrv, err := guardApp.hostRuntime()
	if err != nil {
		t.Fatalf("guard hostRuntime under agy HOME override: %v", err)
	}
	if _, err := guardSrv.InstalledPolicy(); err != nil {
		t.Fatalf("guard InstalledPolicy under agy HOME override: %v", err)
	}
	if out := executionGuardDecision(guardApp.executionHook(), harnessAgy, agyJournal, guardApp.executionProtected(guardSrv))(agyRaw); deniedAgy(out) {
		t.Fatalf("agy guard under agy HOME override denied valid command: %s", out)
	}
}
