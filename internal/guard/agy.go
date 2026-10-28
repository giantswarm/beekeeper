package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

const (
	decisionAsk       = "ask"
	manageTaskTool    = "manage_task"
	commandStatusTool = "command_status"
)

// agyPayload is the PreToolUse JSON agy passes on stdin.
type agyPayload struct {
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
	TranscriptPath string   `json:"transcriptPath"`
	ModelName      string   `json:"modelName"`
	StepIdx        int      `json:"stepIdx"`
	ToolCall       *struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

// agyOutput is the PreToolUse JSON agy reads on stdout.
// agy 1.4.7 denies a call whose hook answers no decision (empty stdout or {}
// included), so a pass answers {"decision":"ask"}.
type agyOutput struct {
	Decision  string         `json:"decision"`
	Reason    string         `json:"reason,omitempty"`
	Overwrite map[string]any `json:"overwrite,omitempty"`
}

func answerAgy(o agyOutput) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(o)
	return b.Bytes()
}

// PassAgy is the agy PreToolUse response that lets agy proceed with its normal
// permission handling.
func PassAgy() []byte {
	return answerAgy(agyOutput{Decision: decisionAsk})
}

// DenyAgy is the agy PreToolUse response that denies a tool call with reason.
func DenyAgy(reason string) []byte {
	return answerAgy(agyOutput{Decision: decisionDeny, Reason: reason})
}

// AgyPaths returns the workspace paths and tool working directory carried by an
// agy PreToolUse payload, for checking hooks.scope.dirs.
func AgyPaths(input []byte) []string {
	var p agyPayload
	if json.Unmarshal(input, &p) != nil {
		return nil
	}
	var out []string
	out = append(out, p.WorkspacePaths...)
	if p.ToolCall != nil && p.ToolCall.Args != nil {
		for _, k := range []string{"Cwd", "DirectoryPath", "SearchPath", "SearchDirectory", "Path", "TargetFile", "AbsolutePath", "NotebookPath"} {
			if v, ok := p.ToolCall.Args[k].(string); ok && v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// AgySession returns the conversation id from an agy PreToolUse payload.
func AgySession(input []byte) string {
	var p agyPayload
	if json.Unmarshal(input, &p) != nil || p.ConversationID == "" {
		return ""
	}
	return p.ConversationID
}

// DecideUnsandboxedAgy is DecideUnsandboxed in agy's PreToolUse format.
func DecideUnsandboxedAgy(input []byte) []byte {
	ev, _, errReason := parseAgyEvent(input)
	if errReason != "" {
		return DenyAgy(errReason)
	}
	if ev.ToolName == SendMessageTool {
		return PassAgy()
	}
	return DenyAgy(unsandboxed)
}

// DecideSandboxAgy is DecideSandbox in agy's PreToolUse format.
func (h Hook) DecideSandboxAgy(input []byte) []byte {
	ev, _, errReason := parseAgyEvent(input)
	if errReason != "" {
		return DenyAgy(errReason)
	}
	if ev.ToolName == "" {
		return PassAgy()
	}
	if r := h.sandboxRefusal(ev); r != "" {
		return DenyAgy(r)
	}
	return PassAgy()
}

// DecideAgy reads an agy PreToolUse JSON event from input, runs the Hook's
// checks on it, and returns the response in agy's PreToolUse JSON format.
func (h Hook) DecideAgy(input []byte) []byte {
	ev, origCmd, errReason := parseAgyEvent(input)
	if errReason != "" {
		return DenyAgy(errReason)
	}
	if ev.ToolName == "" {
		return PassAgy()
	}
	h.freshLabs(ev)
	raw := h.labProxy(ev, h.decide(ev))
	if len(raw) == 0 {
		return PassAgy()
	}
	var wrapper struct {
		Output hookOutput `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(raw, &wrapper) != nil {
		return PassAgy()
	}
	out := wrapper.Output
	if out.PermissionDecision == decisionDeny {
		return DenyAgy(out.Reason)
	}
	if ev.ToolName == bashTool && origCmd != "" && out.UpdatedInput != nil {
		if newCmd, ok := out.UpdatedInput[commandKey].(string); ok && newCmd != "" && newCmd != origCmd {
			return answerAgy(agyOutput{
				Decision:  decisionAllow,
				Overwrite: map[string]any{"CommandLine": newCmd},
			})
		}
	}
	if ev.ToolName == SendMessageTool && out.UpdatedInput != nil {
		if newTo, ok := out.UpdatedInput["to"].(string); ok && newTo != "" {
			return answerAgy(agyOutput{
				Decision:  decisionAllow,
				Overwrite: map[string]any{"Recipient": newTo},
			})
		}
	}
	return PassAgy()
}

// agyNonFileTools are known non-file agy tools whose string arguments are
// scanned by Outbound.toolRefusal.
var agyNonFileTools = map[string]bool{
	"search_web":         true,
	"schedule":           true,
	"invoke_subagent":    true,
	"define_subagent":    true,
	"manage_subagents":   true,
	"list_resources":     true,
	"read_resource":      true,
	"read_url_content":   true,
	"view_content_chunk": true,
	"browser_subagent":   true,
	manageTaskTool:       true,
	commandStatusTool:    true,
}

func parseAgyEvent(input []byte) (ev event, origCmd, errReason string) {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var p agyPayload
	if dec.Decode(&p) != nil || p.ToolCall == nil || p.ToolCall.Name == "" || p.ToolCall.Args == nil {
		return event{}, "", "Refused: malformed PreToolUse payload."
	}
	args := p.ToolCall.Args
	cwd := firstString(args, "Cwd", "DirectoryPath", "SearchPath", "SearchDirectory")
	if cwd == "" && len(p.WorkspacePaths) > 0 {
		cwd = p.WorkspacePaths[0]
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	ev = event{
		CWD:            cwd,
		Session:        p.ConversationID,
		TranscriptPath: p.TranscriptPath,
	}

	switch name := p.ToolCall.Name; {
	case name == "run_command":
		cmd, ok := args["CommandLine"].(string)
		if !ok {
			return event{}, "", "Refused: run_command without a CommandLine string."
		}
		bg, _ := args["IsDaemon"].(bool)
		ev.ToolName = bashTool
		ev.ToolInput = map[string]any{
			commandKey:    cmd,
			backgroundKey: bg,
		}
		return ev, cmd, ""

	case name == manageTaskTool:
		switch firstString(args, "Action", "action") {
		case "send_input":
			in, ok := args["Input"].(string)
			if !ok || in == "" {
				return event{}, "", "Refused: manage_task send_input without an Input string."
			}
			ev.ToolName = bashTool
			ev.ToolInput = map[string]any{
				commandKey: in,
			}
			return ev, "", ""
		case "kill":
			taskID := firstString(args, "TaskId", "task_id")
			if taskID == "" {
				return event{}, "", "Refused: manage_task kill without a TaskId string."
			}
			ev.ToolName = TaskStopTool
			ev.ToolInput = map[string]any{
				"task_id": taskID,
			}
			return ev, "", ""
		case "status":
			taskID := firstString(args, "TaskId", "task_id")
			if taskID == "" {
				return event{}, "", "Refused: manage_task status without a TaskId string."
			}
			ev.ToolName = manageTaskTool
			ev.ToolInput = args
			return ev, "", ""
		case "list":
			ev.ToolName = manageTaskTool
			ev.ToolInput = args
			return ev, "", ""
		default:
			return event{}, "", "Refused: manage_task without a valid Action."
		}

	case name == "send_command_input":
		in, ok := args["Input"].(string)
		if !ok || in == "" {
			return event{}, "", "Refused: send_command_input without an Input string."
		}
		ev.ToolName = bashTool
		ev.ToolInput = map[string]any{
			commandKey: in,
		}
		return ev, "", ""

	case name == "terminate_command":
		taskID := firstString(args, "CommandId", "TaskId", "task_id")
		if taskID == "" {
			return event{}, "", "Refused: terminate_command without a CommandId string."
		}
		ev.ToolName = TaskStopTool
		ev.ToolInput = map[string]any{
			"task_id": taskID,
		}
		return ev, "", ""

	case name == commandStatusTool:
		if firstString(args, "CommandId", "TaskId", "task_id") == "" {
			return event{}, "", "Refused: command_status without a CommandId string."
		}
		ev.ToolName = commandStatusTool
		ev.ToolInput = args
		return ev, "", ""

	case name == "write_file" || name == "write_to_file":
		fp := firstString(args, "Path", "TargetFile", filePathKey)
		if fp == "" {
			return event{}, "", "Refused: " + name + " without a Path string."
		}
		ev.ToolName = writeTool
		ev.ToolInput = map[string]any{
			filePathKey: fp,
			"content":   firstString(args, "Content", "CodeContent", "content"),
		}
		return ev, "", ""

	case name == "edit_file" || name == "replace_file_content" || name == "multi_replace_file_content" || name == "multi_edit_file":
		const newStringKey = "new_string"
		fp := firstString(args, "Path", "TargetFile", filePathKey)
		if fp == "" {
			return event{}, "", "Refused: " + name + " without a Path string."
		}
		ev.ToolName = editTool
		ti := map[string]any{
			filePathKey:  fp,
			newStringKey: firstString(args, "Replacement", "ReplacementContent", newStringKey),
		}
		if chunks, ok := args["ReplacementChunks"].([]any); ok {
			var edits []any
			for _, ch := range chunks {
				if m, ok := ch.(map[string]any); ok {
					edits = append(edits, map[string]any{
						newStringKey: firstString(m, "ReplacementContent", "Replacement", newStringKey),
					})
				}
			}
			ti["edits"] = edits
		}
		ev.ToolInput = ti
		return ev, "", ""

	case name == "notebook_edit":
		fp := firstString(args, "NotebookPath", "Path", "TargetFile", notebookPathKey, filePathKey)
		if fp == "" {
			return event{}, "", "Refused: notebook_edit without a NotebookPath string."
		}
		ev.ToolName = notebookTool
		ev.ToolInput = map[string]any{
			notebookPathKey: fp,
			"new_source":    firstString(args, "NewSource", "Content", "Replacement", "new_source"),
		}
		return ev, "", ""

	case name == "read_file" || name == "view_file" || name == "view_file_outline" || name == "view_code_item":
		fp := firstString(args, "Path", "AbsolutePath", "File", filePathKey)
		if fp == "" {
			return event{}, "", "Refused: " + name + " without a Path string."
		}
		ev.ToolName = readTool
		ev.ToolInput = map[string]any{
			filePathKey: fp,
		}
		return ev, "", ""

	case name == "grep_search":
		sp := firstString(args, "SearchPath", "Path", "DirectoryPath", pathKey)
		if sp == "" {
			return event{}, "", "Refused: grep_search without a SearchPath string."
		}
		ev.ToolName = grepTool
		mode := contentKey
		if mpl, ok := args["MatchPerLine"].(bool); ok && !mpl {
			mode = "files_with_matches"
		}
		ev.ToolInput = map[string]any{
			pathKey:       sp,
			patternKey:    firstString(args, "Query", "Pattern", patternKey),
			outputModeKey: mode,
		}
		return ev, "", ""

	case name == "find_by_name" || name == "list_dir" || name == "list_directory":
		sp := firstString(args, "SearchDirectory", "DirectoryPath", "Path", pathKey)
		if sp == "" {
			return event{}, "", "Refused: " + name + " without a DirectoryPath string."
		}
		ev.ToolName = globTool
		ev.ToolInput = map[string]any{
			pathKey:    sp,
			patternKey: firstString(args, "Pattern", patternKey),
		}
		return ev, "", ""

	case name == "ask_question":
		ev.ToolName = AskTool
		ev.ToolInput = args
		return ev, "", ""

	case name == "send_message":
		to := firstString(args, "Recipient", "to")
		if to == "" {
			return event{}, "", "Refused: send_message without a Recipient string."
		}
		ev.ToolName = SendMessageTool
		ev.ToolInput = map[string]any{
			"to":      to,
			"message": firstString(args, "Message", "message"),
		}
		return ev, "", ""

	case name == "call_mcp_tool":
		server := firstString(args, "ServerName", "server")
		tool := firstString(args, "ToolName", "tool")
		if server == "" || tool == "" {
			return event{}, "", "Refused: call_mcp_tool without ServerName and ToolName strings."
		}
		mcpInput := args
		if sub, ok := args["Arguments"].(map[string]any); ok {
			mcpInput = sub
		}
		ev.ToolName = mcpPrefix + server + "__" + tool
		ev.ToolInput = mcpInput
		return ev, "", ""

	case strings.HasPrefix(name, "mcp_"):
		if strings.HasPrefix(name, mcpPrefix) {
			if len(name) <= len(mcpPrefix) {
				return event{}, "", "Refused: malformed mcp tool name."
			}
			ev.ToolName = name
		} else {
			rest := strings.TrimPrefix(name, "mcp_")
			server, tool, ok := strings.Cut(rest, "_")
			if !ok || server == "" || tool == "" {
				return event{}, "", "Refused: malformed mcp tool name."
			}
			ev.ToolName = mcpPrefix + server + "__" + tool
		}
		ev.ToolInput = args
		return ev, "", ""

	case agyNonFileTools[name]:
		ev.ToolName = name
		ev.ToolInput = args
		return ev, "", ""
	}

	return event{}, "", "Refused: unknown agy tool `" + p.ToolCall.Name + "`."
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
