package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
)

func TestExecutionOmpCoordinatorAdmission(t *testing.T) {
	a, brief, requests := sshCodexTestApp(t, map[string]string{omp.Harness: "18.14.2"})
	a.cfg.Capacity = config.Capacity{Ceiling: 10, Providers: map[string]config.Provider{"fixture": {Kind: config.KindSelfHosted, Ceiling: 1}}, Choices: []config.Choice{{Harness: omp.Harness, Model: "fixture/model", Provider: "fixture"}}}
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
	if st.Starts[0].Execution.Provider != "fixture" {
		t.Fatalf("provider = %q", st.Starts[0].Execution.Provider)
	}
	counts := countProviderAgents(st, nil, a.cfg.Capacity, a.now)
	if len(counts["fixture"].Busy) != 1 {
		t.Fatal("remote worker escaped coordinator ceiling")
	}
}

func TestExecutionOmpRemoteStartDoesNotReadCoordinatorCredentials(t *testing.T) {
	a, _ := sshStartTestApp(t)
	a.cfg.Omp.Providers = map[string]config.OmpProvider{"fixture": {APIKey: "op://fixture/item/key"}}
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
	workspace := t.TempDir()
	j := execution.Journal{Request: execution.StartRequest{Harness: omp.Harness}, Handle: execution.Handle{Workspace: workspace}, Thread: "fixture-thread"}
	for _, tc := range []struct {
		name, format, harness, tool, command string
		allow                                bool
	}{
		{"status", omp.Harness, omp.Harness, "bash", "git status", true},
		{"merge", omp.Harness, omp.Harness, "bash", "gh pr merge 7 --repo giantswarm/beekeeper", false},
		{"foreign format", omp.Harness, execution.HarnessCodex, "bash", "git status", false},
		{"unsupported", omp.Harness, omp.Harness, "python", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j.Request.Harness = tc.harness
			raw, _ := json.Marshal(map[string]any{"session_id": j.Thread, "cwd": workspace, "tool_name": tc.tool, "tool_input": map[string]any{"command": tc.command}})
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
