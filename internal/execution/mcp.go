package execution

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const MCPList = "mcp-list"
const MCPCall = "mcp-call"
const configFlag = "--config"
const httpsScheme = "https"
const agentsCommand = "agents"
const mcpProxyCommand = "mcp-proxy"

// MCPServer is coordinator-only configuration. Credentials and local commands
// never travel to a worker. Empty Tools exposes nothing; wildcards are not used.
type MCPServer struct {
	URL     string   `yaml:"url,omitempty"`
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	Tools   []string `yaml:"tools"`
}

type MCPCallRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func ValidateMCP(servers map[string]MCPServer) error {
	for name, s := range servers {
		if !ValidIdentity(name) || strings.Contains(name, "__") || (s.URL == "") == (s.Command == "") {
			return errors.New("each MCP server needs a distinct name and exactly one URL or command")
		}
		if s.Command != "" && !HostPath(s.Command) {
			return errors.New("MCP command must be an absolute path")
		}
		if s.URL != "" {
			u, err := url.Parse(s.URL)
			if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != httpsScheme) {
				return errors.New("MCP URL must be HTTP(S) without user credentials")
			}
		}
		for i, tool := range s.Tools {
			if tool == "" || len(tool) > 128 || strings.ContainsAny(tool, "*\x00\r\n") || slices.Contains(s.Tools[:i], tool) {
				return errors.New("MCP tools must be distinct explicit names")
			}
		}
	}
	return nil
}

func connectMCP(ctx context.Context, s MCPServer) (*client.Client, error) {
	var c *client.Client
	var err error
	if s.Command != "" {
		c, err = client.NewStdioMCPClient(s.Command, nil, s.Args...)
	} else {
		c, err = client.NewStreamableHttpClient(s.URL)
		if err == nil {
			err = c.Start(ctx)
		}
	}
	if err != nil {
		if c != nil {
			_ = c.Close()
		}
		return nil, errors.New("coordinator MCP connection failed")
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "beekeeper-remote", Version: "1"}
	if _, err = c.Initialize(ctx, init); err != nil {
		_ = c.Close()
		return nil, errors.New("coordinator MCP initialization failed")
	}
	return c, nil
}

// GatewayMCP enforces the current coordinator allowlist for discovery AND calls.
// An unexposed call is refused before any upstream connection or side effect.
func GatewayMCP(ctx context.Context, servers map[string]MCPServer, op string, request MCPCallRequest) (any, error) {
	if err := ValidateMCP(servers); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if op == MCPCall {
		name, tool, ok := strings.Cut(request.Name, "__")
		s, exists := servers[name]
		if !ok || !exists || !slices.Contains(s.Tools, tool) {
			return nil, errors.New("MCP tool is not exposed by the coordinator")
		}
		c, err := connectMCP(ctx, s)
		if err != nil {
			return nil, err
		}
		defer func() { _ = c.Close() }()
		req := mcp.CallToolRequest{}
		req.Params.Name = tool
		req.Params.Arguments = request.Arguments
		result, err := c.CallTool(ctx, req)
		if err != nil {
			return nil, errors.New("coordinator MCP tool call failed; outcome may be unknown, do not retry automatically")
		}
		return result, nil
	}
	if op != MCPList {
		return nil, errors.New("unsupported MCP operation")
	}
	result := mcp.ListToolsResult{Tools: []mcp.Tool{}}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		s := servers[name]
		if len(s.Tools) == 0 {
			continue
		}
		c, err := connectMCP(ctx, s)
		if err != nil {
			return nil, err
		}
		listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		_ = c.Close()
		if err != nil {
			return nil, fmt.Errorf("coordinator MCP discovery failed for %s", name)
		}
		for _, tool := range listed.Tools {
			if slices.Contains(s.Tools, tool.Name) {
				tool.Name = name + "__" + tool.Name
				result.Tools = append(result.Tools, tool)
			}
		}
	}
	return result, nil
}

type MCPCallback func(context.Context, string, any, any) error

// MCPProxy is the worker's stdio gateway. Every tool use returns to the
// coordinator so cached discovery never grants authority after revocation.
func MCPProxy(ctx context.Context, call MCPCallback) (*mcpserver.MCPServer, error) {
	var tools mcp.ListToolsResult
	if err := call(ctx, MCPList, nil, &tools); err != nil {
		return nil, err
	}
	server := mcpserver.NewMCPServer("beekeeper-coordinator", "1")
	for _, tool := range tools.Tools {
		server.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var result mcp.CallToolResult
			err := call(ctx, MCPCall, MCPCallRequest{Name: req.Params.Name, Arguments: req.GetArguments()}, &result)
			return &result, err
		})
	}
	return server, nil
}
