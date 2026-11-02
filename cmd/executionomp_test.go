package cmd

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
)

const (
	executionOmpFixture = "fixture"
	executionOmpStatus  = "git status"
)

func TestExecutionOmpCoordinatorAdmission(t *testing.T) {
	a, brief, requests := sshCodexTestApp(t, map[string]string{omp.Harness: "18.14.2"})
	a.cfg.Capacity = config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{executionOmpFixture: {Kind: config.KindSelfHosted, Ceiling: 1}}, Choices: []config.Choice{{Harness: omp.Harness, Model: "fixture/model", Provider: executionOmpFixture}}}
	h := a.cfg.Agents.Hosts[sshTestSelector]
	h.Harnesses = []string{omp.Harness}
	a.cfg.Agents.Hosts[sshTestSelector] = h
	if _, err := runAgents(a, agentStartName, "--name", "remote-omp", brief, "--host", sshTestSelector, "--repo", sshTestRepository, "--ref", mainBranch, "--harness", harnessAuto); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || (*requests)[1].Harness != omp.Harness || (*requests)[1].Model != "fixture/model" || !(*requests)[1].Messaging || !(*requests)[1].LeaseRequired {
		t.Fatalf("request %+v", *requests)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Starts[0].Execution.Provider != executionOmpFixture {
		t.Fatalf("provider = %q", st.Starts[0].Execution.Provider)
	}
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now)
	if len(counts[executionOmpFixture].Busy) != 1 {
		t.Fatal("remote worker escaped coordinator ceiling")
	}
}

func TestExecutionOmpRemoteStartDoesNotReadCoordinatorCredentials(t *testing.T) {
	a, _ := sshStartTestApp(t)
	a.cfg.Omp.Providers = map[string]config.OmpProvider{executionOmpFixture: {APIKey: "op://fixture/item/key"}} //nolint:gosec // fixture vault reference, never a credential value
	for _, host := range [][]string{{"--host", sshTestSelector}, {"--host=" + sshTestSelector}} {
		for _, harness := range []string{omp.Harness, harnessAuto} {
			args := append([]string{agentStartName, "--harness=" + harness, "--model=fixture/model"}, host...)
			if a.ompStartNeedsVault(args) {
				t.Fatalf("remote start asked for coordinator credentials: %q", args)
			}
		}
	}
}

func TestExecutionOmpGuardFormat(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	j := execution.Journal{Request: execution.StartRequest{Harness: omp.Harness}, Handle: execution.Handle{Workspace: workspace}, Thread: "fixture-thread"}
	for _, tc := range []struct {
		name, format, harness, tool, command string
		allow                                bool
	}{
		{"omp status", omp.Harness, omp.Harness, executionOmpBash, executionOmpStatus, true},
		{"omp merge", omp.Harness, omp.Harness, executionOmpBash, "gh pr merge 7 --repo giantswarm/beekeeper", false},
		{"foreign format", omp.Harness, execution.HarnessCodex, executionOmpBash, executionOmpStatus, false},
		{"unsupported", omp.Harness, omp.Harness, "python", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j.Request.Harness = tc.harness
			raw, _ := json.Marshal(map[string]any{executionOmpSession: j.Thread, executionOmpCwd: workspace, executionHookTool: tc.tool, executionHookInput: map[string]any{hookTypeCommand: tc.command}})
			b := executionGuardDecision(guard.Hook{}, tc.format, j, nil)(raw)
			var out struct {
				Allow bool `json:"allow"`
			}
			if json.Unmarshal(b, &out) != nil || out.Allow != tc.allow {
				t.Fatalf("answer %s", b)
			}
		})
	}
	j.Request.Harness = omp.Harness
	b := executionGuardDecision(guard.Hook{}, "", j, nil)(nil)
	if !strings.Contains(string(b), "does not match") {
		t.Fatalf("Claude format accepted omp: %s", b)
	}
}
