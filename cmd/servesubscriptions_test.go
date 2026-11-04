package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Exercise serve's MCP server over its stateless HTTP transport without
// requiring the backing stores: initialize and subscribe never read them.
func TestServeRefusesLegacyResourceSubscriptions(t *testing.T) {
	s := &server{}
	h := httptest.NewServer(s.mcpHandler())
	t.Cleanup(h.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	version := "2025-06-18"
	request := func(body string) map[string]json.RawMessage {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", version)
		var message struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(body), &message); err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Mcp-Method", message.Method)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var got map[string]json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
			t.Fatalf("HTTP %d: %v", res.StatusCode, err)
		}
		return got
	}

	for _, method := range []string{"resources/subscribe", "resources/unsubscribe"} {
		t.Run(method, func(t *testing.T) {
			got := request(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{"uri":"beekeeper://roster"}}`)
			var rpcError struct {
				Code int `json:"code"`
			}
			if err := json.Unmarshal(got["error"], &rpcError); err != nil || rpcError.Code != mcp.METHOD_NOT_FOUND {
				t.Fatalf("want method-not-supported, got %s", got)
			}
		})
	}
	got := request(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	var result struct {
		Capabilities struct {
			Resources *struct {
				Subscribe bool `json:"subscribe"`
			} `json:"resources"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(got["result"], &result); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if result.Capabilities.Resources == nil || result.Capabilities.Resources.Subscribe {
		t.Fatalf("want readable resources without legacy subscriptions, got %s", got["result"])
	}
	// The modern revision still advertises the updates delivered through
	// subscriptions/listen, while refusing the removed legacy method.
	version = "2026-07-28"
	got = request(`{"jsonrpc":"2.0","id":3,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if err := json.Unmarshal(got["result"], &result); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if result.Capabilities.Resources == nil || !result.Capabilities.Resources.Subscribe {
		t.Fatalf("want modern resource subscriptions, got %s", got["result"])
	}
	got = request(`{"jsonrpc":"2.0","id":4,"method":"resources/subscribe","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"uri":"beekeeper://roster"}}`)
	var rpcError struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(got["error"], &rpcError); err != nil || rpcError.Code != mcp.METHOD_NOT_FOUND {
		t.Fatalf("want modern method-not-supported, got %s", got)
	}
}
