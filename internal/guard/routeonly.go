package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// RouteBuilds is the PreToolUse answer while hooks.guards is off: no check
// refuses anything, but a Bash call's heavy commands still run under
// `beekeeper run`, so run.remote builds them off the coordinator. Where a
// build runs is placement, not a guard. Nil leaves the call as it is.
func (h Hook) RouteBuilds(input []byte) []byte {
	if h.Self == "" {
		return nil
	}
	var ev event
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if dec.Decode(&ev) != nil || ev.ToolName != bashTool {
		return nil
	}
	cmd, _ := ev.ToolInput["command"].(string)
	if strings.TrimSpace(cmd) == "" || trivial.MatchString(cmd) || wrapped.MatchString(cmd) || !isHeavy(cmd) {
		return nil
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	bg, _ := ev.ToolInput[backgroundKey].(bool)
	if routed := h.routeBuilds(cmd); routed != "" {
		return h.rewrite(ev.ToolInput, cwd, routed, true, bg)
	}
	return h.rewrite(ev.ToolInput, cwd, ShellQuote(h.Self)+" run -- "+ShellQuote(h.Shell)+" -c "+ShellQuote(cmd), true, bg)
}
