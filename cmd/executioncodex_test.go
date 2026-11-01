package cmd

import (
	"context"
	"path"
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

const codexTestModel = "gpt-fixture"

// sshCodexTestApp is a coordinator with a codex provider whose first host
// lists claude and codex; offered is what the host's check advertises.
func sshCodexTestApp(t *testing.T, offered map[string]string) (*app, string, *[]execution.StartRequest) {
	t.Helper()
	a, brief := sshStartTestApp(t)
	a.cfg.Capacity = config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{claudeComm: {Ceiling: 1}, codex.Harness: {Ceiling: 1}}, Choices: []config.Choice{{Harness: claudeComm, Model: bgModel}, {Harness: codex.Harness, Model: codexTestModel}}}
	h := a.cfg.Agents.Hosts[sshTestSelector]
	h.Harnesses = []string{claudeComm, codex.Harness}
	a.cfg.Agents.Hosts[sshTestSelector] = h
	a.executionProbe = func(_ context.Context, h execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{Publication: true, Protocol: execution.Protocol, Identity: h.Identity, Version: project.Version(), PolicyDigest: h.PolicyDigest, Harnesses: offered, Unavailable: map[string]string{}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
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

func TestExecutionStartsCodexOnAHostThatOffersIt(t *testing.T) {
	a, brief, requests := sshCodexTestApp(t, map[string]string{claudeComm: "2.1.290", codex.Harness: "0.166.0"})
	if _, err := runAgents(a, agentStartName, "--name", "remote-codex", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", codex.Harness, "--model", codexTestModel); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != codex.Harness || (*requests)[1].Model != codexTestModel || !(*requests)[1].Messaging || !(*requests)[1].LeaseRequired {
		t.Fatalf("remote codex request: %+v", *requests)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Starts[0].Harness != codex.Harness || st.Starts[0].Execution.Provider != codex.Harness {
		t.Fatalf("remote codex is recorded against %q/%q", st.Starts[0].Harness, st.Starts[0].Execution.Provider)
	}
	// The one codex subscription now counts the remote worker: its single
	// slot is busy, while claude's own slot stays free.
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now)
	if len(counts[codex.Harness].Busy) != 1 || len(counts[claudeComm].Busy) != 0 {
		t.Fatalf("remote codex is not counted against its subscription: %+v", counts)
	}
	if _, err := runAgents(a, agentStartName, "--name", "remote-codex-2", brief, "--host", sshTestOtherSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", codex.Harness, "--model", codexTestModel); err == nil {
		t.Fatal("a host that does not list codex started it")
	}
}

func TestExecutionRefusesCodexTheHostDoesNotOffer(t *testing.T) {
	a, brief, requests := sshCodexTestApp(t, map[string]string{claudeComm: "2.1.290"})
	_, err := runAgents(a, agentStartName, "--name", "remote-codex", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", codex.Harness, "--model", codexTestModel)
	if err == nil || !strings.Contains(err.Error(), "does not offer harness codex") || len(*requests) != 0 {
		t.Fatalf("a host whose codex check failed started it: %v", err)
	}
	// Auto never picks the harness the host does not offer.
	if _, err := runAgents(a, agentStartName, "--name", "remote-auto", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAuto); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != claudeComm {
		t.Fatalf("auto picked %+v", *requests)
	}
}

func TestExecutionAutoPicksRemoteCodex(t *testing.T) {
	a, brief, requests := sshCodexTestApp(t, map[string]string{codex.Harness: "0.166.0"})
	if _, err := runAgents(a, agentStartName, "--name", "remote-auto", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAuto); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != codex.Harness || (*requests)[1].Model != codexTestModel {
		t.Fatalf("auto on a codex-only host picked %+v", *requests)
	}
}

func TestExecutionHarnessAcceptsCodexOnly(t *testing.T) {
	o := executionStartOptions{Host: sshTestSelector}
	for _, base := range []Harness{claudeHarness{}, codexHarness{}} {
		if _, err := executionHarness(base, o, "brief.md"); err != nil {
			t.Fatalf("%q: %v", base.Name(), err)
		}
	}
	for _, base := range []Harness{ompHarness{}, agyHarness{}} {
		if _, err := executionHarness(base, o, "brief.md"); err == nil {
			t.Fatalf("%q has no remote adapter yet", base.Name())
		}
	}
}

func TestExecutionPublicationAndMergeAcceptRemoteCodex(t *testing.T) {
	a, _ := sshStartTestApp(t)
	owner, _, _ := strings.Cut(sshTestRepository, "/")
	a.cfg.GitHub.AllowedOrgs = []string{owner}
	ref := execution.Ref{Host: sshTestSelector, Execution: "3aa27721-fb1e-4f07-b0aa-bdf100e0fdd9", Generation: 1}
	for _, harness := range []string{codex.Harness, "omp"} {
		rec := &state.ExecutionRecord{Handle: execution.Handle{Ref: ref, Commit: strings.Repeat("a", 40)}, Request: execution.StartRequest{Ref: ref, Repository: sshTestRepository, Harness: harness, LeaseRequired: true}, Lease: &execution.LeaseGrant{ExpiresAt: time.Now().Add(time.Hour)}}
		st := &state.State{Starts: []state.Start{{Party: state.Party{Name: "remote-worker"}, Execution: rec}}}
		_, err := a.executionPublicationLive(st, ref)
		if (err == nil) != (harness == codex.Harness) {
			t.Fatalf("publication for %s: %v", harness, err)
		}
		err = a.executionMergeLive(context.Background(), st, execution.MergeUse{Ref: ref, Repository: sshTestRepository, PR: 1, Head: strings.Repeat("b", 40)})
		if harness == codex.Harness && err != nil && strings.Contains(err.Error(), "authority stopped or mismatched") {
			t.Fatalf("merge refused remote codex by harness: %v", err)
		}
		if harness != codex.Harness && (err == nil || !strings.Contains(err.Error(), "authority stopped or mismatched")) {
			t.Fatalf("merge accepted %s: %v", harness, err)
		}
	}
}

func TestExecutionGuardDecisionMatchesHookFormatToHarness(t *testing.T) {
	workspace := t.TempDir()
	thread := "019a0000-0000-7000-8000-000000000001"
	codexRaw := []byte(`{"hook_event_name":"PreToolUse","tool_name":"exec_command","tool_input":{"cmd":"git status"},"session_id":"` + thread + `","cwd":"` + workspace + `"}`)
	deniedCodex := func(b []byte) bool { return strings.Contains(string(b), `"permissionDecision":"deny"`) }
	codexJournal := execution.Journal{Request: execution.StartRequest{Harness: codex.Harness, ConversationID: "c0ffee00-0000-4000-8000-000000000000"}, Handle: execution.Handle{Workspace: workspace, ConversationID: "c0ffee00-0000-4000-8000-000000000000"}, Thread: thread}
	if out := executionGuardDecision(guard.Hook{}, codex.Harness, codexJournal, nil)(codexRaw); deniedCodex(out) {
		t.Fatalf("codex hook on its own thread was denied: %s", out)
	}
	// A codex call is bound to the recorded thread, not the request identity.
	unrecorded := codexJournal
	unrecorded.Thread = ""
	if out := executionGuardDecision(guard.Hook{}, codex.Harness, unrecorded, nil)(codexRaw); !deniedCodex(out) {
		t.Fatal("codex hook passed before its thread was recorded")
	}
	// A Claude-format hook in a codex execution, and the reverse, are denied.
	if out := executionGuardDecision(guard.Hook{}, "", codexJournal, nil)(codexRaw); !deniedCodex(out) {
		t.Fatal("claude-format hook ran in a codex execution")
	}
	claudeJournal := codexJournal
	claudeJournal.Request.Harness = claudeComm
	if out := executionGuardDecision(guard.Hook{}, codex.Harness, claudeJournal, nil)(codexRaw); !deniedCodex(out) {
		t.Fatal("codex-format hook ran in a claude execution")
	}
}
