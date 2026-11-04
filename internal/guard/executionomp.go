package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	ompWorkingDir = "cwd"
	ompReadTool   = "read"
	ompWriteTool  = "write"
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
	case bashCmd:
		ev.ToolName = bashTool
		for _, key := range []string{"async", "pty", nameKey, "ready"} {
			if value, ok := input[key]; ok && value != nil && value != false && value != "" {
				return ExecutionDeny("managed omp requires a foreground command without services or PTYs")
			}
		}
		if dir, ok := input[ompWorkingDir]; ok {
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
	case ompReadTool, ompWriteTool:
		if ev.ToolName == ompReadTool {
			ev.ToolName = readTool
		} else {
			ev.ToolName = writeTool
		}
		path, ok := input[pathKey].(string)
		if !ok || !ompLiteralPath(path) {
			return ExecutionDeny("managed omp file tools require one literal local path without shorthand, whitespace, lists or patterns; use guarded bash for other filenames")
		}
		if ev.ToolName == readTool {
			target := path
			if !filepath.IsAbs(target) {
				cwd := ev.CWD
				if cwd == "" {
					cwd = workspace
				}
				target = filepath.Join(cwd, target)
			}
			// Missing reads trigger omp spelling, suffix and approved-plan
			// recovery, whose eventual target the shared guard cannot check.
			if _, err := os.Stat(target); err != nil {
				return ExecutionDeny("managed omp reads require an existing literal target; use guarded bash for other reads")
			}
		}
		// Do not preserve provider-specific fields which the shared guard
		// might interpret differently from the harness.
		ev.ToolInput = map[string]any{filePathKey: path}
		if ev.ToolName == writeTool {
			ev.ToolInput[contentKey] = input[contentKey]
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

// omp expands home/@ shorthands, normalizes Unicode spaces and interprets
// read lists and patterns. The shared guard checks a single literal path;
// refusing these expressions keeps its target identical to the tool's target.
func ompLiteralPath(path string) bool {
	return path != "" && !strings.HasPrefix(strings.ToLower(path), "www.") && !strings.HasPrefix(path, "@") && !strings.HasPrefix(path, "~") &&
		!strings.ContainsAny(path, ":;,\\*?[]{}") &&
		!strings.ContainsFunc(path, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
