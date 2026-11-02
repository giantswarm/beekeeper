package guard

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// ExecutionDenyOmp is the extension's explicit, fail-closed wire decision.
func ExecutionDenyOmp(reason string) []byte {
	b, _ := json.Marshal(map[string]any{"allow": false, "reason": reason})
	return b
}

// DecideExecutionOmp translates only the small tool surface the remote runner
// enables. Unknown tools and malformed inputs fail closed before the shared
// execution guard checks workspace containment and coordinator-only operations.
func (h Hook) DecideExecutionOmp(raw []byte, session, workspace string, protected []string) []byte {
	var ev event
	if json.Unmarshal(raw, &ev) != nil || ev.Session != session || session == "" || ev.ToolInput == nil {
		return ExecutionDeny("invalid managed omp execution tool event")
	}
	input := ev.ToolInput
	switch ev.ToolName {
	case "bash":
		ev.ToolName = bashTool
		for _, key := range []string{"async", "pty", "name", "ready"} {
			if value, ok := input[key]; ok && value != nil && value != false && value != "" {
				return ExecutionDeny("managed omp requires a foreground command without services or PTYs")
			}
		}
		if dir, ok := input["cwd"]; ok {
			target, valid := dir.(string)
			if !valid {
				return ExecutionDeny("invalid omp command working directory")
			}
			if target != "" {
				if !filepath.IsAbs(target) {
					target = filepath.Join(ev.CWD, target)
				}
				ev.CWD = target
			}
		}
	case "read", "write":
		if ev.ToolName == "read" {
			ev.ToolName = readTool
		} else {
			ev.ToolName = writeTool
		}
		path, ok := input["path"].(string)
		if !ok || path == "" || strings.Contains(path, ":") {
			return ExecutionDeny("omp file tool requires a local path")
		}
		// Do not preserve provider-specific fields which the shared guard
		// might interpret differently from the harness.
		ev.ToolInput = map[string]any{"file_path": path}
		if ev.ToolName == writeTool {
			ev.ToolInput["content"] = input["content"]
		}
	default:
		return ExecutionDeny("tool is not supported by the managed omp adapter; use bash for edits and component commands")
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return ExecutionDeny("invalid managed omp tool input")
	}
	return h.DecideExecution(b, session, workspace, protected)
}
