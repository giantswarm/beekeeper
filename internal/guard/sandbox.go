package guard

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// DecideSandbox is Decide for a session out of the hooks' scope: only the
// sandbox's hold on the file tools applies.
func (h Hook) DecideSandbox(input []byte) []byte {
	var ev event
	if json.Unmarshal(input, &ev) != nil {
		return nil
	}
	if r := h.sandboxRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	return nil
}

// sandboxRefusal holds the harness's own file tools, which run outside the
// agent sandbox, to the sandbox's allow lists: a path outside them is
// refused. "" when the call passes or no sandbox holds the session.
func (h Hook) sandboxRefusal(ev event) string {
	if h.Sandbox == nil {
		return ""
	}
	cwd := ev.CWD
	path, write := "", false
	switch ev.ToolName {
	case readTool:
		path, _ = ev.ToolInput[filePathKey].(string)
	case editTool, writeTool, multiEditTool:
		path, _ = ev.ToolInput[filePathKey].(string)
		write = true
	case notebookTool:
		path, _ = ev.ToolInput[notebookPathKey].(string)
		write = true
	case grepTool, globTool:
		path, _ = ev.ToolInput["path"].(string)
		if pat, _ := ev.ToolInput["pattern"].(string); ev.ToolName == globTool && filepath.IsAbs(pat) {
			// an absolute pattern searches from its literal prefix
			path = globRoot(pat)
		}
		if path == "" {
			path = cwd
		}
	default:
		return ""
	}
	if path == "" {
		return ""
	}
	if write && !h.Sandbox.Writable(path, cwd) {
		return "Refused: the agent sandbox does not let this session write " + path +
			". Sessions write their checkouts and worktrees, beekeeper's state and the temporary directory; " +
			"a path the desk needs goes into sandbox.allowWrite in beekeeper's config."
	}
	if !write && !h.Sandbox.Readable(path, cwd) {
		return "Refused: the agent sandbox does not let this session read " + path +
			". The home directory is denied apart from the paths the sandbox re-allows (beekeeper sandbox render lists them); " +
			"credentials stay unreadable by design. A path the desk needs goes into sandbox.allowRead in beekeeper's config."
	}
	return ""
}

// globRoot is the directory an absolute glob pattern searches from: its
// components before the first one holding a meta character.
func globRoot(pat string) string {
	parts := strings.Split(pat, string(filepath.Separator))
	for i, p := range parts {
		if strings.ContainsAny(p, `*?[{\`) {
			return filepath.Join(append([]string{string(filepath.Separator)}, parts[:i]...)...)
		}
	}
	return pat
}
