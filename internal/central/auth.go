package central

import (
	"context"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// authenticationRequired recognizes muster's pre-dispatch refusal, not a
// backend error mentioning authentication after a write may have happened.
func (c *Client) authenticationRequired(res *mcp.CallToolResult) bool {
	if res == nil || !res.IsError {
		return false
	}
	if fields, ok := res.StructuredContent.(map[string]any); ok {
		if fields["outcome"] != nil {
			return false
		}
		if fields["status"] == "auth_required" && fields["server"] == c.cfg.Server {
			return true
		}
	}
	// Older aggregators supply only the text marker.
	return strings.HasPrefix(envelopeText(res), "auth_required: server '"+c.cfg.Server+"' requires authentication before its tools can be called")
}

func (c *Client) signInRequired() error {
	return c.unreachable("authentication required for server %s; sign in with `muster auth login --context %s --server %s` (SSO servers may require `muster auth login --context %s --force`); the requested operation was not run", c.cfg.Server, c.cfg.Context, c.cfg.Server, c.cfg.Context)
}

// authenticate asks muster to reuse the caller's grant. A browser challenge
// stays a refusal: no browser, token, or sign-in URL is exposed by this client.
func (c *Client) authenticate(ctx context.Context, mc *client.Client, deadline time.Duration) error {
	req := mcp.CallToolRequest{}
	req.Params.Name = "call_tool"
	req.Params.Arguments = map[string]any{"name": "core_auth_login", "arguments": map[string]any{"server": c.cfg.Server}}
	res, err := mc.CallTool(ctx, req)
	if err != nil {
		return c.timedOut(ctx, stageAuth, deadline, c.signInRequired())
	}
	if res == nil || res.IsError {
		return c.signInRequired()
	}
	if fields, ok := res.StructuredContent.(map[string]any); ok {
		if fields["authUrl"] != nil || fields["status"] == "auth_required" {
			return c.signInRequired()
		}
	}
	// These are muster's documented connection markers, also used by its CLI.
	text := envelopeText(res)
	for _, marker := range []string{"Already Connected", "Successfully connected", "already authenticated"} {
		if strings.Contains(text, marker) {
			return nil
		}
	}
	return c.signInRequired()
}
