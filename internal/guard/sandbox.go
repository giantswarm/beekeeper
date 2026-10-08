package guard

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
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
		path, _ = ev.ToolInput[pathKey].(string)
		if pat, _ := ev.ToolInput[patternKey].(string); ev.ToolName == globTool && filepath.IsAbs(pat) {
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

// labProxy puts the refresh of the session's lab kubeconfigs in front of a
// sandboxed session's command, after the hook's own rewrites: the sandbox
// reaches a lab's API server only through its SOCKS proxy, whose
// credentials change with every Claude Code process, so the kubeconfig in
// the lease is pointed at this process's proxy before the command runs.
// out is the hook's answer so far; a refused call stays refused.
func (h Hook) labProxy(ev event, out []byte) []byte {
	if ev.ToolName != bashTool || h.Sandbox == nil || h.Labs == nil {
		return out
	}
	var labs []string
	for _, l := range h.Labs() {
		if heldBy(l, ev.Session) {
			labs = append(labs, ShellQuote(l.Env))
		}
	}
	if len(labs) == 0 {
		return out
	}
	slices.Sort(labs)
	input := ev.ToolInput
	var d hookOutput
	if out != nil {
		var o map[string]hookOutput
		if json.Unmarshal(out, &o) != nil {
			return out
		}
		if d = o["hookSpecificOutput"]; d.PermissionDecision == decisionDeny {
			return out
		}
		if d.UpdatedInput != nil {
			input = d.UpdatedInput
		}
	}
	cmd, _ := input["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return out
	}
	d.UpdatedInput = maps.Clone(input)
	d.UpdatedInput["command"] = ShellQuote(h.Self) + " lease kubeconfig --refresh " + strings.Join(labs, " ") + " >/dev/null 2>&1; " + cmd
	d.PermissionDecision = decisionAllow
	return answer(d)
}
