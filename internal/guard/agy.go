package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

const decisionAsk = "ask"

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
		for _, k := range []string{"Cwd", "DirectoryPath", "SearchPath", "SearchDirectory", "Path", "TargetFile", "AbsolutePath"} {
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
	ev, origCmd, errReason := parseAgyEvent(input)
	_ = origCmd
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
	raw := h.decide(ev)
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
	if ev.ToolName == bashTool && out.UpdatedInput != nil {
		if newCmd, ok := out.UpdatedInput[commandKey].(string); ok && newCmd != "" && newCmd != origCmd {
			return answerAgy(agyOutput{
				Decision:  decisionAllow,
				Overwrite: map[string]any{"CommandLine": newCmd},
			})
		}
	}
	return PassAgy()
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
	session := p.ConversationID
	if session != "" && !strings.HasPrefix(session, "agy-") {
		session = "agy-" + session
	}
	ev = event{
		CWD:     cwd,
		Session: session,
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

	case name == "write_file" || name == "write_to_file":
		ev.ToolName = writeTool
		ev.ToolInput = map[string]any{
			filePathKey: firstString(args, "Path", "TargetFile", filePathKey),
			"content":   firstString(args, "Content", "CodeContent", "content"),
		}
		return ev, "", ""

	case name == "edit_file" || name == "replace_file_content" || name == "multi_replace_file_content":
		const newStringKey = "new_string"
		ev.ToolName = editTool
		ti := map[string]any{
			filePathKey:  firstString(args, "Path", "TargetFile", filePathKey),
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

	case name == "read_file" || name == "view_file" || name == "view_file_outline" || name == "view_code_item":
		ev.ToolName = readTool
		ev.ToolInput = map[string]any{
			filePathKey: firstString(args, "Path", "AbsolutePath", "File", filePathKey),
		}
		return ev, "", ""

	case name == "grep_search":
		ev.ToolName = grepTool
		mode := contentKey
		if mpl, ok := args["MatchPerLine"].(bool); ok && !mpl {
			mode = "files_with_matches"
		}
		ev.ToolInput = map[string]any{
			pathKey:       firstString(args, "SearchPath", "Path", "DirectoryPath", pathKey),
			patternKey:    firstString(args, "Query", "Pattern", patternKey),
			outputModeKey: mode,
		}
		return ev, "", ""

	case name == "find_by_name" || name == "list_dir":
		ev.ToolName = globTool
		ev.ToolInput = map[string]any{
			pathKey:    firstString(args, "SearchDirectory", "DirectoryPath", "Path", pathKey),
			patternKey: firstString(args, "Pattern", patternKey),
		}
		return ev, "", ""

	case name == "ask_question":
		ev.ToolName = AskTool
		ev.ToolInput = args
		return ev, "", ""

	case name == "send_message":
		ev.ToolName = SendMessageTool
		ev.ToolInput = map[string]any{
			"to":      firstString(args, "Recipient", "to"),
			"message": firstString(args, "Message", "message"),
		}
		return ev, "", ""

	case name == "call_mcp_tool":
		server := firstString(args, "ServerName", "server")
		tool := firstString(args, "ToolName", "tool")
		mcpInput := args
		if sub, ok := args["Arguments"].(map[string]any); ok {
			mcpInput = sub
		}
		ev.ToolName = mcpPrefix + server + "__" + tool
		ev.ToolInput = mcpInput
		return ev, "", ""

	case strings.HasPrefix(name, "mcp_"):
		if strings.HasPrefix(name, mcpPrefix) {
			ev.ToolName = name
		} else {
			rest := strings.TrimPrefix(name, "mcp_")
			server, tool, ok := strings.Cut(rest, "_")
			if ok {
				ev.ToolName = mcpPrefix + server + "__" + tool
			} else {
				ev.ToolName = mcpPrefix + rest
			}
		}
		ev.ToolInput = args
		return ev, "", ""
	}

	return event{}, "", ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
