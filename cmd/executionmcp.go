package cmd

import (
	"context"
	"errors"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// Authentication is read under the store's lock; network I/O never holds it.
func (a *app) executionMCP(ctx context.Context, host string, req execution.CallbackRequest) (any, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	r, _ := executionRecord(st, req.Ref)
	if r == nil || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
		return nil, errors.New("unauthenticated execution callback")
	}
	h, ok := a.cfg.Agents.Hosts[r.Selector]
	if !ok || h.Identity != req.Ref.Host || !r.Request.MCP || r.Request.Coordinator != a.cfg.Identity.Host || st.Stop != nil || r.StopRequest != nil || r.Terminal() || r.Completed {
		return nil, errors.New("coordinator MCP access revoked")
	}
	for _, hold := range st.Holds {
		if hold.Target == holdGitHub && hold.Active(a.now) {
			return nil, errors.New("coordinator MCP access held")
		}
	}
	var request execution.MCPCallRequest
	if req.Operation == execution.MCPCall && execution.Decode(req.Payload, &request) != nil {
		return nil, errors.New("invalid MCP call")
	}
	return execution.GatewayMCP(ctx, a.cfg.Agents.RemoteMCP, req.Operation, request)
}

func (a *app) executionMCPCmd() *cobra.Command {
	return &cobra.Command{Use: "mcp-proxy", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, j, err := a.executionCaller()
		if err != nil {
			return err
		}
		if !j.Request.MCP {
			return refused("coordinator MCP is not enabled for this execution")
		}
		policy, err := s.InstalledPolicy()
		if err != nil || policy.Digest() != j.Request.PolicyDigest {
			return refused("execution policy changed before MCP proxy startup")
		}
		co, ok := s.Config.Coordinators[j.Request.Coordinator]
		if !ok {
			return refused("execution coordinator is no longer trusted")
		}
		proxy, err := execution.MCPProxy(cmd.Context(), func(ctx context.Context, op string, payload, result any) error {
			return execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), op, j.Handle.Ref, payload, j.Request.ControlKey, result, s.Callback)
		})
		if err != nil {
			return err
		}
		return mcpserver.NewStdioServer(proxy).Listen(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
	}}
}
