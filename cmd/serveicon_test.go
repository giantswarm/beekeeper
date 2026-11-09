package cmd

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/giantswarm/mcp-toolkit/brand"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestServeAdvertisesGiantSwarmIcon pins the logo muster stores as the
// connector's icon: serve's initialize result carries the Giant Swarm logo.
func TestServeAdvertisesGiantSwarmIcon(t *testing.T) {
	initReq := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	raw, err := json.Marshal((&server{}).mcpServer().HandleMessage(t.Context(), initReq))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result mcp.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if want := []mcp.Icon{brand.GiantSwarmIcon()}; !reflect.DeepEqual(resp.Result.ServerInfo.Icons, want) {
		t.Errorf("serverInfo.icons = %+v, want %+v", resp.Result.ServerInfo.Icons, want)
	}
}
