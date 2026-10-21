package cmd

import (
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
)

// docsServer is serve's MCP server: every tool is registered in every mode.
var docsServer = sync.OnceValue(func() *mcpserver.MCPServer { return (&server{}).mcpServer() })

// TestToolsDocIsCurrent keeps docs/tools.md, every tool's description and
// input schema as a model reads them, equal to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (make tools-doc).
func TestToolsDocIsCurrent(t *testing.T) {
	tooldesctest.AssertServerDoc(t, docsServer(),
		"# The tools\n\nGenerated from the registered tools of `beekeeper serve` (`make tools-doc`); through muster every tool is `x_<central.server>_<name>`. "+
			"Every tool takes `agent` and `host`, which tell one person's agents apart.",
		"../docs/tools.md")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	if err := tooldesc.CheckServer(docsServer(), tooldesc.DefaultBudget); err != nil {
		t.Error(err)
	}
}

// TestToolAnnotations: every tool says what it does to the shared state,
// all four hints, as this table judged them from its handler.
func TestToolAnnotations(t *testing.T) {
	type hint struct{ readOnly, destructive, idempotent, openWorld bool }
	read, add, remove := hint{true, false, true, false}, hint{false, false, true, false}, hint{false, true, true, false}
	want := map[string]hint{
		"lease_list": read, "hold_list": read, "lanes": read, "lane_turn": read,
		"note_list": read, "list_agents": read, "snapshot": read, "receive_messages": read,
		"lease_claim": add, "lane_queue": add, "lane_settle": add, nameAgentsRegister: add,
		"lease_release": remove, "hold_set": remove, "hold_lift": remove, "lane_leave": remove,
		nameAgentsLeave: remove, "ack_messages": remove,
		"note_done": {false, true, true, true}, "note_answer": {false, true, true, true},
		"note_add": {false, false, false, true}, "send_message": {false, false, false, true}, "converse": {false, false, false, true},
	}
	tools := docsServer().ListTools()
	if len(tools) != len(want) {
		t.Errorf("%d tools registered, the table judges %d", len(tools), len(want))
	}
	for name, st := range tools {
		a := st.Tool.Annotations
		if got := (hint{*a.ReadOnlyHint, *a.DestructiveHint, *a.IdempotentHint, *a.OpenWorldHint}); got != want[name] {
			t.Errorf("%s reports %+v, want %+v", name, got, want[name])
		}
	}
}

// TestCallerRequired: a tool that acts as the calling agent itself requires
// agent and host, and only such a tool: its handler refuses without them.
func TestCallerRequired(t *testing.T) {
	identified := map[string]bool{nameAgentsRegister: true, nameAgentsLeave: true, "converse": true}
	for name, st := range docsServer().ListTools() {
		required := map[string]bool{}
		for _, r := range st.Tool.InputSchema.Required {
			required[r] = true
		}
		if got := required[paramAgent] && required[paramHost]; got != identified[name] {
			t.Errorf("%s requires agent and host: %v, its handler: %v", name, got, identified[name])
		}
	}
	// The roster tools refuse before they read the store.
	s := &server{cfg: &config.Config{}, now: time.Now}
	for _, tool := range append(s.tools(), s.messageTools()...) {
		if tool.tool.Name != nameAgentsRegister && tool.tool.Name != nameAgentsLeave {
			continue
		}
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{}
		c := s.newCall(t.Context(), identity.Caller{Email: "ada@example.com"}, req)
		if _, err := tool.run(c, req); Code(err) != ExitUsage {
			t.Errorf("%s without agent and host: %v, want a usage error", tool.tool.Name, err)
		}
	}
}
