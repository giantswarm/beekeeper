package execution

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const proxyEchoTool = "fixture__echo"
const mcpFixtureName = "fixture"
const mcpEchoName = "echo"

func TestMCPProxyListsAndCallsCoordinator(t *testing.T) {
	var called bool
	proxy, err := MCPProxy(context.Background(), func(_ context.Context, op string, payload, result any) error {
		switch op {
		case MCPList:
			*result.(*mcp.ListToolsResult) = mcp.ListToolsResult{Tools: []mcp.Tool{mcp.NewTool(proxyEchoTool)}}
		case MCPCall:
			request := payload.(MCPCallRequest)
			if request.Name != proxyEchoTool || request.Arguments["text"] != "hello" {
				t.Fatalf("bad forwarding: %+v", request)
			}
			called = true
			*result.(*mcp.CallToolResult) = *mcp.NewToolResultText("from coordinator")
		default:
			t.Fatalf("unexpected operation %s", op)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.NewInProcessClient(proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: mcpFixtureName, Version: "1"}
	if _, err := c.Initialize(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil || len(tools.Tools) != 1 {
		t.Fatalf("tools %v, error %v", tools, err)
	}
	call := mcp.CallToolRequest{}
	call.Params.Name = proxyEchoTool
	call.Params.Arguments = map[string]any{"text": "hello"}
	result, err := c.CallTool(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if !called || !strings.Contains(string(raw), "from coordinator") {
		t.Fatal("result lost")
	}
	call.Params.Name = "fixture__private"
	result, err = c.CallTool(context.Background(), call)
	if err == nil && !result.IsError {
		t.Fatal("unlisted tool accepted")
	}
}

func TestMCPConfigRefusesBroadOrAmbiguousExposure(t *testing.T) {
	for _, s := range []MCPServer{{}, {URL: "file:///private", Tools: []string{mcpEchoName}}, {Command: "relative", Tools: []string{mcpEchoName}}, {URL: "http://example.test/mcp", Tools: []string{"*"}}, {URL: "http://example.test/mcp", Command: "/bin/false"}} {
		if ValidateMCP(map[string]MCPServer{mcpFixtureName: s}) == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}

// The helper is an actual MCP stdio subprocess, with synthetic tools only.
func TestMCPStdioFixture(t *testing.T) {
	if !slices.Contains(os.Args, "--mcp-fixture") {
		t.Skip("subprocess fixture")
	}
	s := mcpserver.NewMCPServer(mcpFixtureName, "1")
	s.AddTool(mcp.NewTool(mcpEchoName), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("stdio proof"), nil
	})
	if err := mcpserver.NewStdioServer(s).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestMCPGatewayStdioConnector(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	servers := map[string]MCPServer{mcpFixtureName: {Command: executable, Args: []string{"-test.run=^TestMCPStdioFixture$", "--", "--mcp-fixture"}, Tools: []string{mcpEchoName}}}
	listed, err := GatewayMCP(context.Background(), servers, MCPList, MCPCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if tools := listed.(mcp.ListToolsResult).Tools; len(tools) != 1 || tools[0].Name != proxyEchoTool {
		t.Fatalf("tools %+v", tools)
	}
	result, err := GatewayMCP(context.Background(), servers, MCPCall, MCPCallRequest{Name: proxyEchoTool})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "stdio proof") {
		t.Fatalf("result %s", raw)
	}
}
