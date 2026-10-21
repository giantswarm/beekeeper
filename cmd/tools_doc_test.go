package cmd

import (
	"sync"
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	mcpserver "github.com/mark3labs/mcp-go/server"
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

// TestToolAnnotations: every tool says what it does to the shared state.
// A read-only tool is not destructive and idempotent, or mcp-go's defaults
// report it destructive; a tool acting on the calling agent itself requires
// agent and host, as its handler does.
func TestToolAnnotations(t *testing.T) {
	identified := map[string]bool{"agents_register": true, "agents_leave": true}
	for name, st := range docsServer().ListTools() {
		a := st.Tool.Annotations
		if *a.ReadOnlyHint && (*a.DestructiveHint || !*a.IdempotentHint) {
			t.Errorf("%s is read-only but reported destructive %v, idempotent %v", name, *a.DestructiveHint, *a.IdempotentHint)
		}
		required := map[string]bool{}
		for _, r := range st.Tool.InputSchema.Required {
			required[r] = true
		}
		if got := required[paramAgent] && required[paramHost]; got != identified[name] {
			t.Errorf("%s requires agent and host: %v, its handler: %v", name, got, identified[name])
		}
	}
}
