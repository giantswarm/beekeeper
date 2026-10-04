// Package central is the local beekeeper's client of the central instance:
// beekeeper serve behind muster, called as the person through muster's
// call_tool. The person signs in once per installation (muster auth login
// --context); each call takes the endpoint from the muster context and the
// bearer from muster auth token, and keeps neither.
//
// There is no local copy of what the central instance holds: a call that
// does not reach it fails with Unreachable, and the caller refuses the verb.
package central

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Unreachable is a call that did not reach the central instance: muster's
// context or sign-in, the network, muster, or beekeeper serve behind it.
type Unreachable struct {
	Context string
	Reason  string
}

func (e *Unreachable) Error() string {
	return fmt.Sprintf("the central instance (muster context %s) is unreachable: %s", e.Context, e.Reason)
}

// Refused is the central instance's refusal: a lease held, a name it does
// not know, an action that is another person's.
type Refused struct {
	Message string
	// Usage marks a call the central instance found malformed: a name it
	// does not know, an argument missing.
	Usage bool
}

func (e *Refused) Error() string { return e.Message }

// usageCode is beekeeper's exit code of a malformed call, which beekeeper
// serve's failed results carry.
const usageCode = 2

// Runner runs a muster command and returns its standard output.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Client calls the central instance's tools.
type Client struct {
	cfg config.Central
	run Runner
}

// New returns the client of cfg; run nil runs cfg.Muster.
func New(cfg config.Central, run Runner) *Client {
	c := &Client{cfg: cfg, run: run}
	if c.run == nil {
		c.run = c.muster
	}
	return c
}

// Context is the muster context the client calls through.
func (c *Client) Context() string { return c.cfg.Context }

func (c *Client) muster(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.cfg.Muster, args...) //nolint:gosec // the configured muster binary, with beekeeper's own arguments
	cmd.Env = append(os.Environ(), "MUSTER_NO_UPDATE_CHECK=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			line, _, _ := strings.Cut(msg, "\n")
			return nil, fmt.Errorf("%s %s: %s", c.cfg.Muster, args[0], line)
		}
		return nil, fmt.Errorf("%s %s: %w", c.cfg.Muster, args[0], err)
	}
	return out, nil
}

func (c *Client) unreachable(format string, args ...any) error {
	return &Unreachable{Context: c.cfg.Context, Reason: fmt.Sprintf(format, args...)}
}

// endpoint is the muster context's endpoint URL.
func (c *Client) endpoint(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "context", "show", c.cfg.Context, "-o", "json")
	if err != nil {
		return "", c.unreachable("%v", err)
	}
	var v struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Endpoint == "" {
		return "", c.unreachable("muster context show %s names no endpoint", c.cfg.Context)
	}
	return v.Endpoint, nil
}

// token is the person's muster access token, renewed by muster when it
// expired.
func (c *Client) token(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "auth", "token", "--context", c.cfg.Context)
	if err != nil {
		return "", c.unreachable("%v; sign in with `muster auth login --context %s`", err, c.cfg.Context)
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", c.unreachable("muster auth token --context %s printed no token; sign in with `muster auth login --context %s`", c.cfg.Context, c.cfg.Context)
	}
	return t, nil
}

// Tool is the muster name of one of the central instance's tools.
func (c *Client) Tool(name string) string { return "x_" + c.cfg.Server + "_" + name }

// Call calls the central instance's tool with args and decodes its
// structured result into v (nil: none wanted). It returns the tool's text.
// A refusal is *Refused; a call that did not reach the instance is
// *Unreachable.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any, v any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout.Duration)
	defer cancel()
	url, err := c.endpoint(ctx)
	if err != nil {
		return "", err
	}
	token, err := c.token(ctx)
	if err != nil {
		return "", err
	}
	mc, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		return "", c.unreachable("%v", err)
	}
	defer func() { _ = mc.Close() }()
	if err := mc.Start(ctx); err != nil {
		return "", c.unreachable("%v", err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: project.Name, Version: project.Version()}
	if _, err := mc.Initialize(ctx, init); err != nil {
		return "", c.unreachable("muster at %s: %v", url, err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "call_tool"
	req.Params.Arguments = map[string]any{"name": c.Tool(tool), "arguments": args}
	res, err := mc.CallTool(ctx, req)
	if err != nil {
		return "", c.unreachable("muster: %s: %v", c.Tool(tool), err)
	}
	return c.result(tool, res, v)
}

// result unwraps muster's call_tool result: its text is the envelope of
// the wrapped tool's result, its structured content the tool's own. An
// error without beekeeper's outcome never came from beekeeper serve.
func (c *Client) result(tool string, res *mcp.CallToolResult, v any) (string, error) {
	text := envelopeText(res)
	outcome, code := "", 0.0
	if m, ok := res.StructuredContent.(map[string]any); ok {
		outcome, _ = m["outcome"].(string)
		code, _ = m["code"].(float64)
	}
	if res.IsError {
		switch outcome {
		case "refused":
			return text, &Refused{Message: text, Usage: code == usageCode}
		case "error":
			return text, errors.New(text)
		}
		return text, c.unreachable("%s: %s", c.Tool(tool), firstLine(text))
	}
	if v != nil && res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return text, err
		}
		if err := json.Unmarshal(raw, v); err != nil {
			return text, fmt.Errorf("%s: the result: %w", c.Tool(tool), err)
		}
	}
	return text, nil
}

// envelopeText is the wrapped tool's text: muster's call_tool returns the
// tool's result as a JSON envelope in its first text content.
func envelopeText(res *mcp.CallToolResult) string {
	var texts []string
	for _, ct := range res.Content {
		if t, ok := ct.(mcp.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	raw := strings.Join(texts, "\n")
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if len(texts) == 0 || json.Unmarshal([]byte(texts[0]), &env) != nil || env.Content == nil {
		return raw
	}
	var inner []string
	for _, ct := range env.Content {
		if ct.Type == "text" {
			inner = append(inner, ct.Text)
		}
	}
	return strings.Join(inner, "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
