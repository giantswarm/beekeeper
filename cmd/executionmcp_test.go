package cmd

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Exercise the wire operations a worker's stdio gateway uses, through the
// authenticated callback and a real upstream MCP HTTP transport.
const remoteMCPEcho = "fixture__echo"
const remoteMCPNameKey = "name"
const remoteMCPFixture = "fixture"

func TestExecutionMCPRemoteDiscoveryAndCall(t *testing.T) {
	a, r := executionTestApp(t)
	var calls atomic.Int32
	upstream := mcpserver.NewMCPServer(remoteMCPFixture, "1")
	handler := func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return mcp.NewToolResultText(req.GetString(paramText, "")), nil
	}
	upstream.AddTool(mcp.NewTool("echo", mcp.WithString(paramText)), handler)
	upstream.AddTool(mcp.NewTool("private"), handler)
	httpServer := httptest.NewServer(mcpserver.NewStreamableHTTPServer(upstream, mcpserver.WithStateLess(true)))
	defer httpServer.Close()
	raw, _ := json.Marshal(map[string]any{"mcp": true})
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	a.cfg.Identity.Host = r.Coordinator
	config := "remoteMCP:\n  fixture:\n    url: " + httpServer.URL + "/mcp\n    tools: [echo]\n"
	if err := yaml.Unmarshal([]byte(config), &a.cfg.Agents); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts[0].Execution.Request = r; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	var listed mcp.ListToolsResult
	if err := coordinatorTestCall(t, a, r, "mcp-list", nil, &listed, false); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != remoteMCPEcho {
		t.Fatalf("unexpected tools: %+v", listed.Tools)
	}
	call := map[string]any{remoteMCPNameKey: remoteMCPEcho, "arguments": map[string]any{paramText: "remote proof"}}
	var result mcp.CallToolResult
	if err := coordinatorTestCall(t, a, r, "mcp-call", call, &result, false); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if result.IsError || !strings.Contains(string(encoded), "remote proof") || calls.Load() != 1 {
		t.Fatalf("call result %s, calls %d", encoded, calls.Load())
	}
	for _, name := range []string{"fixture__private", "missing__echo", "fixture__*"} {
		call[remoteMCPNameKey] = name
		if err := coordinatorTestCall(t, a, r, "mcp-call", call, &result, false); err == nil {
			t.Fatalf("unexposed tool %s accepted", name)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("refused calls reached upstream")
	}
	// Revocation takes effect even after a worker cached the discovered schema.
	if err := yaml.Unmarshal([]byte(strings.Replace(config, "tools: [echo]", "tools: []", 1)), &a.cfg.Agents); err != nil {
		t.Fatal(err)
	}
	call[remoteMCPNameKey] = remoteMCPEcho
	if err := coordinatorTestCall(t, a, r, "mcp-call", call, &result, false); err == nil {
		t.Fatal("revoked tool accepted")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts[0].Execution.Completed = true; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, r, "mcp-list", nil, &listed, false); err == nil {
		t.Fatal("completed execution retained access")
	}
}

func TestExecutionMCPAuthority(t *testing.T) {
	tests := []struct {
		name   string
		change func(*app, *execution.StartRequest, *state.State)
	}{
		{"wrong credential", func(_ *app, r *execution.StartRequest, _ *state.State) { r.ControlKey, _ = execution.NewControlKey() }},
		{"wrong generation", func(_ *app, r *execution.StartRequest, _ *state.State) { r.Ref.Generation++ }},
		{"desk stopped", func(_ *app, _ *execution.StartRequest, st *state.State) {
			st.Stop = &state.Stop{Reason: remoteMCPFixture}
		}},
		{"worker stopped", func(_ *app, _ *execution.StartRequest, st *state.State) {
			st.Starts[0].Execution.StopRequest = &execution.StopRequest{ID: remoteMCPFixture, Reason: remoteMCPFixture}
		}},
		{"worker completed", func(_ *app, _ *execution.StartRequest, st *state.State) { st.Starts[0].Execution.Completed = true }},
		{"worker terminal", func(_ *app, _ *execution.StartRequest, st *state.State) {
			st.Starts[0].Execution.Observation.ProcessState = execution.Exited
		}},
		{"MCP disabled", func(_ *app, _ *execution.StartRequest, st *state.State) { st.Starts[0].Execution.Request.MCP = false }},
		{"host removed", func(a *app, _ *execution.StartRequest, _ *state.State) { a.cfg.Agents.Hosts = nil }},
		{"GitHub held", func(_ *app, _ *execution.StartRequest, st *state.State) {
			st.Holds = []state.Hold{{Target: holdGitHub}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, r := executionTestApp(t)
			a.cfg.Identity.Host = r.Coordinator
			r.MCP = true
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts[0].Execution.Request = r; return nil, nil }); err != nil {
				t.Fatal(err)
			}
			var result mcp.ListToolsResult
			if err := coordinatorTestCall(t, a, r, "mcp-list", nil, &result, false); err != nil {
				t.Fatal("initial access refused", err)
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) { test.change(a, &r, st); return nil, nil }); err != nil {
				t.Fatal(err)
			}
			if err := coordinatorTestCall(t, a, r, "mcp-list", nil, &result, false); err == nil {
				t.Fatal("revoked or forged execution accepted")
			}
		})
	}
}
