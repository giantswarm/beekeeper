package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	codexApplyPatchTool = "apply_patch"
	codexWriteStdinTool = "write_stdin"
	codexViewImageTool  = "view_image"
	codexUpdatePlanTool = "update_plan"
	codexWebTool        = "webrun"
	webSearchTool       = "WebSearch"
	codexCmdKey         = "cmd"
	codexCharsKey       = "chars"
	codexPatchKey       = "pa" + "tch"
	codexInputKey       = "input"
	hookEventKey        = "hook_event_" + "name"
	toolNameKey         = "tool_" + "name"
	toolInputKey        = "tool_" + "input"
	flagConfig          = "--con" + "fig"
	flagAs              = "--" + "as"
	gsedCmd             = "g" + "sed"
	perlCmd             = "pe" + "rl"
	gojqCmd             = "go" + "jq"
	xargsCmd            = "x" + "args"
	codexFailClosedMsg  = "Refused: the PreToolUse hook input is not valid Codex PreToolUse JSON; failing closed."
	codexHomeRefusalMsg = "Refused: the Codex harness home ($CODEX_HOME and ~/.codex) holds guard hooks, credentials, and shell configuration and cannot be modified by an agent."
)

// codexOutput is the PreToolUse JSON Codex reads on stdout.
type codexOutput struct {
	Specific codexSpecificOutput `json:"hookSpecificOutput"`
}

type codexSpecificOutput struct {
	Event              string         `json:"hookEventName"`
	PermissionDecision string         `json:"permissionDecision,omitempty"`
	Reason             string         `json:"permissionDecisionReason,omitempty"`
	Context            string         `json:"additionalContext,omitempty"`
	UpdatedInput       map[string]any `json:"updatedInput,omitempty"`
}

func answerCodex(o codexSpecificOutput) []byte {
	o.Event = PreToolUseEvent
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(codexOutput{Specific: o}); err != nil {
		return []byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Refused: failed to encode PreToolUse decision; failing closed."}}` + "\n")
	}
	return b.Bytes()
}

// PassCodex is the Codex PreToolUse response that lets the tool call proceed
// unchanged.
func PassCodex() []byte {
	return []byte("{}\n")
}

// DenyCodex is the Codex PreToolUse response that blocks a tool call with
// reason. Because Codex treats non-zero exits and non-JSON stdout as a pass,
// every denial and every internal guard error exits 0 with this JSON.
func DenyCodex(reason string) []byte {
	if strings.TrimSpace(reason) == "" {
		reason = "Refused: blocked by PreToolUse guard."
	}
	return answerCodex(codexSpecificOutput{
		PermissionDecision: decisionDeny,
		Reason:             reason,
	})
}

type codexPatchHeader struct {
	op     string
	target string
}

var patchHeaderLineRe = regexp.MustCompile(`^\*\*\* (Add File|Update File|Delete File|Move to): (.+)$`)

// parseCodexPatchHeaders trims each line of patch (matching Codex's own
// apply_patch line parser), validates every `***` header line, and returns the
// file headers or a non-empty refusal reason.
func parseCodexPatchHeaders(patch string) ([]codexPatchHeader, string) {
	var headers []codexPatchHeader
	for line := range strings.SplitSeq(patch, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "***") {
			continue
		}
		switch trimmed {
		case "*** Begin Patch", "*** End Patch", "*** End of File":
			continue
		}
		m := patchHeaderLineRe.FindStringSubmatch(trimmed)
		if m == nil {
			return nil, "Refused: apply_patch call contains an unrecognized or malformed `***` header line; failing closed."
		}
		target := strings.TrimSpace(m[2])
		if target == "" {
			return nil, "Refused: apply_patch call contains an empty file header; failing closed."
		}
		headers = append(headers, codexPatchHeader{
			op:     m[1],
			target: target,
		})
	}
	if len(headers) == 0 {
		return nil, "Refused: apply_patch call names no file header; failing closed."
	}
	return headers, ""
}

func isCodexShellTool(toolName string) bool {
	switch toolName {
	case bashTool, "bash", "exec_command", "shell_command", "shell", "local_shell":
		return true
	default:
		return false
	}
}

func extractCodexShellCommand(toolInput map[string]any) (cmd, key string, ok bool) {
	if s, ok := toolInput[commandKey].(string); ok {
		return s, commandKey, true
	}
	if s, ok := toolInput[codexCmdKey].(string); ok {
		return s, codexCmdKey, true
	}
	return "", "", false
}

// ValidateCodexInput checks that input is a well-formed Codex PreToolUse JSON
// object with hook_event_name == "PreToolUse", valid tool_name, tool_input
// object, and required tool arguments, and contains no NUL bytes. Returns a
// non-empty refusal reason when invalid.
func ValidateCodexInput(input []byte) string {
	if len(bytes.TrimSpace(input)) == 0 {
		return codexFailClosedMsg
	}
	var top map[string]any
	if err := json.Unmarshal(input, &top); err != nil || top == nil {
		return codexFailClosedMsg
	}
	if hasNULValue(top) {
		return "Refused: invalid NUL byte in PreToolUse hook input (ValueError: embedded null byte); failing closed."
	}
	if s, isStr := top[hookEventKey].(string); !isStr || s != PreToolUseEvent {
		return codexFailClosedMsg
	}
	toolName, ok := top[toolNameKey].(string)
	if !ok || strings.TrimSpace(toolName) == "" {
		return codexFailClosedMsg
	}
	toolInput, ok := top[toolInputKey].(map[string]any)
	if !ok || toolInput == nil {
		return codexFailClosedMsg
	}
	if rawCWD, exists := top["cwd"]; exists && rawCWD != nil {
		if _, isStr := rawCWD.(string); !isStr {
			return codexFailClosedMsg
		}
	}
	switch {
	case isCodexShellTool(toolName):
		if _, _, ok := extractCodexShellCommand(toolInput); !ok {
			return "Refused: " + toolName + " call is missing a string `command` in tool_input; failing closed."
		}
		for _, k := range []string{codexWorkdirKey, "cwd"} {
			if rawWD, exists := toolInput[k]; exists && rawWD != nil {
				if _, isStr := rawWD.(string); !isStr {
					return codexFailClosedMsg
				}
			}
		}
	case toolName == codexApplyPatchTool:
		patch, ok := extractCodexPatch(toolInput)
		if !ok || strings.TrimSpace(patch) == "" {
			return "Refused: apply_patch call is missing a patch string in tool_input; failing closed."
		}
		if _, reason := parseCodexPatchHeaders(patch); reason != "" {
			return reason
		}
	case toolName == codexWriteStdinTool:
		if _, ok := toolInput[codexCharsKey].(string); !ok {
			return "Refused: write_stdin call is missing a string `chars` in tool_input; failing closed."
		}
	case toolName == codexViewImageTool:
		p, ok := toolInput[pathKey].(string)
		if !ok || strings.TrimSpace(p) == "" {
			return "Refused: view_image call is missing a non-empty string `path` in tool_input; failing closed."
		}
	// Web tools are read-only and go through the shared guard, like Claude web calls.
	case toolName == codexUpdatePlanTool || toolName == codexWebTool || toolName == "WebFetch" || toolName == webSearchTool:
		return ""
	case toolName == readTool || toolName == writeTool || toolName == editTool || toolName == multiEditTool:
		p, ok := toolInput[filePathKey].(string)
		if !ok || strings.TrimSpace(p) == "" {
			p, ok = toolInput[pathKey].(string)
		}
		if !ok || strings.TrimSpace(p) == "" {
			return "Refused: " + toolName + " call is missing a non-empty file path in tool_input; failing closed."
		}
	case toolName == notebookTool:
		p, ok := toolInput[notebookPathKey].(string)
		if !ok || strings.TrimSpace(p) == "" {
			p, ok = toolInput[filePathKey].(string)
		}
		if !ok || strings.TrimSpace(p) == "" {
			return "Refused: NotebookEdit call is missing a non-empty notebook_path in tool_input; failing closed."
		}
	case toolName == globTool || toolName == grepTool || toolName == SendMessageTool || toolName == AskTool || toolName == TaskStopTool:
		return ""
	default:
		return "Refused: unrecognized PreToolUse tool_name `" + toolName + "` in Codex hook input; failing closed."
	}
	return ""
}

func hasNULValue(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.IndexByte(x, 0) >= 0
	case map[string]any:
		for k, val := range x {
			if strings.IndexByte(k, 0) >= 0 || hasNULValue(val) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if hasNULValue(item) {
				return true
			}
		}
	}
	return false
}

func extractCodexPatch(toolInput map[string]any) (string, bool) {
	for _, k := range []string{commandKey, codexPatchKey, codexInputKey} {
		if s, ok := toolInput[k].(string); ok {
			return s, true
		}
	}
	return "", false
}

// CodexPaths returns the working directory and target file paths carried by a
// Codex PreToolUse payload, for checking hooks.scope.dirs.
func CodexPaths(input []byte) []string {
	var ev event
	if json.Unmarshal(input, &ev) != nil {
		return nil
	}
	var out []string
	baseCWD := ""
	if ev.CWD != "" {
		baseCWD = expandHome(ev.CWD)
		out = append(out, baseCWD)
	}
	if ev.ToolInput == nil {
		return out
	}
	if isCodexShellTool(ev.ToolName) {
		if wd := codexShellWorkdir(baseCWD, ev.ToolInput); wd != "" && wd != baseCWD {
			out = append(out, wd)
		}
	}
	for _, k := range []string{filePathKey, pathKey, notebookPathKey} {
		if s, ok := ev.ToolInput[k].(string); ok && s != "" {
			p := expandHome(s)
			if !filepath.IsAbs(p) && ev.CWD != "" {
				p = filepath.Join(expandHome(ev.CWD), p)
			}
			out = append(out, p)
		}
	}
	if ev.ToolName == codexApplyPatchTool {
		if patch, ok := extractCodexPatch(ev.ToolInput); ok {
			headers, _ := parseCodexPatchHeaders(patch)
			for _, hdr := range headers {
				t := expandHome(hdr.target)
				if !filepath.IsAbs(t) && ev.CWD != "" {
					t = filepath.Join(expandHome(ev.CWD), t)
				}
				out = append(out, t)
			}
		}
	}
	return out
}

// CodexSession returns the session id from a Codex PreToolUse payload.
func CodexSession(input []byte) string {
	var ev event
	if json.Unmarshal(input, &ev) != nil {
		return ""
	}
	return ev.Session
}

// DecideUnsandboxedCodex is DecideUnsandboxed in Codex's PreToolUse format.
func DecideUnsandboxedCodex(input []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = DenyCodex(fmt.Sprintf("Refused: internal panic in PreToolUse guard (%v); failing closed.", r))
		}
	}()
	if reason := ValidateCodexInput(input); reason != "" {
		return DenyCodex(reason)
	}
	var ev event
	if json.Unmarshal(input, &ev) != nil {
		return DenyCodex(codexFailClosedMsg)
	}
	if ev.ToolName == SendMessageTool {
		return PassCodex()
	}
	return DenyCodex(unsandboxed)
}

// DecideSandboxCodex is DecideSandbox in Codex's PreToolUse format.
func (h Hook) DecideSandboxCodex(input []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = DenyCodex(fmt.Sprintf("Refused: internal panic in PreToolUse guard (%v); failing closed.", r))
		}
	}()
	if reason := ValidateCodexInput(input); reason != "" {
		return DenyCodex(reason)
	}
	var ev event
	if json.Unmarshal(input, &ev) != nil {
		return DenyCodex(codexFailClosedMsg)
	}
	ev.CWD = codexCWD(ev.CWD)
	if r := h.codexSandboxCheck(ev); r != "" {
		return DenyCodex(r)
	}
	return PassCodex()
}

func codexCWD(rawCWD string) string {
	if s := strings.TrimSpace(rawCWD); s != "" {
		return expandHome(s)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func codexShellWorkdir(baseCWD string, toolInput map[string]any) string {
	if toolInput == nil {
		return baseCWD
	}
	for _, k := range []string{codexWorkdirKey, "cwd"} {
		if s, ok := toolInput[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				dir := expandHome(s)
				if !filepath.IsAbs(dir) && baseCWD != "" {
					dir = filepath.Join(baseCWD, dir)
				}
				return filepath.Clean(dir)
			}
		}
	}
	return baseCWD
}

func (h Hook) codexSandboxCheck(ev event) string {
	switch {
	case isCodexShellTool(ev.ToolName):
		cmd, _, _ := extractCodexShellCommand(ev.ToolInput)
		synth := ev
		synth.CWD = codexShellWorkdir(ev.CWD, ev.ToolInput)
		synth.ToolName = bashTool
		synth.ToolInput = map[string]any{commandKey: cmd}
		return h.sandboxRefusal(synth)
	case ev.ToolName == codexApplyPatchTool:
		patch, _ := extractCodexPatch(ev.ToolInput)
		headers, reason := parseCodexPatchHeaders(patch)
		if reason != "" {
			return reason
		}
		for _, hdr := range headers {
			synth := ev
			synth.ToolName = writeTool
			synth.ToolInput = map[string]any{filePathKey: hdr.target}
			if r := h.sandboxRefusal(synth); r != "" {
				return r
			}
		}
		return ""
	case ev.ToolName == codexWriteStdinTool:
		chars, _ := ev.ToolInput[codexCharsKey].(string)
		if chars == "" {
			return ""
		}
		synth := ev
		synth.ToolName = bashTool
		synth.ToolInput = map[string]any{commandKey: chars}
		return h.sandboxRefusal(synth)
	case ev.ToolName == codexViewImageTool:
		p, _ := ev.ToolInput[pathKey].(string)
		synth := ev
		synth.ToolName = readTool
		synth.ToolInput = map[string]any{filePathKey: p}
		return h.sandboxRefusal(synth)
	case ev.ToolName == codexUpdatePlanTool:
		return ""
	default:
		return h.sandboxRefusal(ev)
	}
}

// DecideCodex reads a Codex PreToolUse JSON event from input, runs the Hook's
// checks on it, and returns the response in Codex's PreToolUse JSON format.
// Every internal error, malformed payload, or unrecognized tool argument shape
// fails closed with DenyCodex.
func (h Hook) DecideCodex(input []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = DenyCodex(fmt.Sprintf("Refused: internal panic in PreToolUse guard (%v); failing closed.", r))
		}
	}()
	if h.ConfigErr != nil {
		return DenyCodex("Refused: cannot load beekeeper configuration (" + h.ConfigErr.Error() + "); failing closed.")
	}
	if reason := ValidateCodexInput(input); reason != "" {
		return DenyCodex(reason)
	}
	var ev event
	if err := json.Unmarshal(input, &ev); err != nil {
		return DenyCodex(codexFailClosedMsg)
	}
	ev.CWD = codexCWD(ev.CWD)
	if h.Project == "" {
		h.Project = ev.CWD
	}

	switch {
	case ev.ToolName == codexApplyPatchTool:
		patch, _ := extractCodexPatch(ev.ToolInput)
		reason, ctx := h.checkCodexApplyPatch(ev, patch, ev.CWD)
		if reason != "" {
			return DenyCodex(reason)
		}
		if ctx != "" {
			return answerCodex(codexSpecificOutput{Context: ctx})
		}
		return PassCodex()

	case ev.ToolName == codexWriteStdinTool:
		if r := h.guideRefusal(ev); r != "" {
			return DenyCodex(r)
		}
		chars, _ := ev.ToolInput[codexCharsKey].(string)
		if chars == "" {
			return PassCodex()
		}
		if r := h.codexCommandRefusal(ev, chars, ev.CWD); r != "" {
			return DenyCodex(r)
		}
		return PassCodex()

	case ev.ToolName == codexViewImageTool:
		p, _ := ev.ToolInput[pathKey].(string)
		if r := h.codexPathRefusal(p, ev.CWD, false); r != "" {
			return DenyCodex(r)
		}
		synth := ev
		synth.ToolName = readTool
		synth.ToolInput = map[string]any{filePathKey: p}
		return h.decideCodexEvent(synth, "", "", "")

	case ev.ToolName == codexUpdatePlanTool:
		return PassCodex()

	case isCodexShellTool(ev.ToolName):
		origCmd, cmdKey, _ := extractCodexShellCommand(ev.ToolInput)
		shellCWD := codexShellWorkdir(ev.CWD, ev.ToolInput)
		synth := ev
		synth.CWD = shellCWD
		synth.ToolName = bashTool
		synth.ToolInput = map[string]any{commandKey: origCmd}
		if isPatch, patchReason, patchCtx := h.codexBashPureApplyPatch(synth, origCmd, shellCWD); isPatch {
			if patchReason != "" {
				return DenyCodex(patchReason)
			}
			if patchCtx != "" {
				return answerCodex(codexSpecificOutput{Context: patchCtx})
			}
			return PassCodex()
		}
		bashReason, bashCtx := h.codexBashRefusal(synth, origCmd, shellCWD)
		if bashReason != "" {
			return DenyCodex(bashReason)
		}
		return h.decideCodexEvent(synth, origCmd, cmdKey, bashCtx)

	default:
		if r := h.codexToolPathRefusal(ev); r != "" {
			return DenyCodex(r)
		}
		return h.decideCodexEvent(ev, "", "", "")
	}
}

func (h Hook) decideCodexEvent(ev event, origCmd, cmdKey, extraContext string) []byte {
	h.freshLabs(ev)
	raw := h.labProxy(ev, h.decide(ev))
	var out hookOutput
	if len(raw) > 0 {
		var wrapper struct {
			Output hookOutput `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return DenyCodex("Refused: failed to decode internal PreToolUse decision; failing closed.")
		}
		out = wrapper.Output
		if out.PermissionDecision == decisionDeny {
			return DenyCodex(out.Reason)
		}
	}
	reads := h.repoReads(ev)
	ctx := out.AdditionalContext
	for _, add := range []string{extraContext, reads} {
		if add != "" && !strings.Contains(ctx, add) {
			ctx = strings.TrimSpace(ctx + "\n\n" + add)
		}
	}
	if ev.ToolName == bashTool && origCmd != "" && out.UpdatedInput != nil {
		if newCmd, ok := out.UpdatedInput[commandKey].(string); ok && newCmd != "" && newCmd != origCmd {
			if cmdKey == "" {
				cmdKey = commandKey
			}
			return answerCodex(codexSpecificOutput{
				PermissionDecision: decisionAllow,
				Context:            ctx,
				UpdatedInput:       map[string]any{cmdKey: newCmd},
			})
		}
	}
	if ctx != "" {
		return answerCodex(codexSpecificOutput{
			Context: ctx,
		})
	}
	return PassCodex()
}

// codexCommandRefusal runs all Bash refusal checks on cmd without returning a
// command rewrite (used for write_stdin and unquoted heredoc command
// substitutions).
func (h Hook) codexCommandRefusal(ev event, cmd, cwd string) string {
	if isPatch, patchReason, _ := h.codexBashPureApplyPatch(ev, cmd, cwd); isPatch {
		return patchReason
	}
	if r, _ := h.codexBashRefusal(ev, cmd, cwd); r != "" {
		return r
	}
	synth := ev
	synth.ToolName = bashTool
	synth.ToolInput = map[string]any{commandKey: cmd}
	raw := h.decide(synth)
	if len(raw) == 0 {
		return ""
	}
	var wrapper struct {
		Output hookOutput `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return "Refused: failed to decode internal PreToolUse decision; failing closed."
	}
	if wrapper.Output.PermissionDecision == decisionDeny {
		return wrapper.Output.Reason
	}
	return ""
}

// checkCodexApplyPatch validates an apply_patch payload against path guards,
// secret files, protected unlock helpers, delete/folder/guide/outbound/sandbox
// guards, and mandatory repo reads.
func (h Hook) checkCodexApplyPatch(ev event, patch, cwd string) (reason, context string) {
	if r := h.guideRefusal(ev); r != "" {
		return r, ""
	}
	headers, errReason := parseCodexPatchHeaders(patch)
	if errReason != "" {
		return errReason, ""
	}
	var addedLines strings.Builder
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			addedLines.WriteString(line[1:])
			addedLines.WriteByte('\n')
		}
	}
	addedContent := addedLines.String()

	for _, hdr := range headers {
		op := hdr.op
		target := hdr.target
		if r := h.codexPathRefusal(target, cwd, true); r != "" {
			return r, ""
		}
		fullPath := wordPath(target, cwd)
		if fullPath == "" {
			fullPath = target
		}
		if op == "Delete File" {
			if r := deleteRefusal("rm -- " + ShellQuote(fullPath)); r != "" {
				return r, ""
			}
		}
		writeEv := ev
		writeEv.ToolName = writeTool
		writeEv.CWD = cwd
		writeEv.ToolInput = map[string]any{
			filePathKey: fullPath,
			contentKey:  addedContent,
		}
		if r := h.transcriptFileRefusal(writeEv); r != "" {
			return r, ""
		}
		if r := h.folderRefusal(writeEv); r != "" {
			return r, ""
		}
		if r := h.Outbound.toolRefusal(writeEv.ToolName, writeEv.ToolInput); r != "" {
			return r, ""
		}
		if r := h.sandboxRefusal(writeEv); r != "" {
			return r, ""
		}
		if context == "" {
			context = h.repoReads(writeEv)
		}
	}
	return "", context
}

// heredocSubstitutions extracts command substitutions ($(...), $((...)), and
// `...`) from an unquoted here-document body, where single and double quotes do
// not suppress shell expansion (only backslash escapes do).
func heredocSubstitutions(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\\' && i+1 < len(body) {
			i++
			continue
		}
		if c == '$' && i+1 < len(body) && body[i+1] == '(' {
			end := findClosingParen(body, i+1)
			if i+2 < len(body) && body[i+2] == '(' && end > i+3 && body[end-1] == ')' {
				out = append(out, heredocSubstitutions(body[i+3:end-1])...)
			} else if end > i+2 && end <= len(body) {
				out = append(out, body[i+2:end])
			}
			i = min(end, len(body))
			continue
		}
		if c == '`' {
			end := i + 1
			var inner strings.Builder
			for end < len(body) && body[end] != '`' {
				if body[end] == '\\' && end+1 < len(body) && (body[end+1] == '`' || body[end+1] == '\\' || body[end+1] == '$') {
					end++
				}
				inner.WriteByte(body[end])
				end++
			}
			out = append(out, inner.String())
			i = min(end, len(body))
			continue
		}
	}
	return out
}

// codexBashPureApplyPatch checks if a Bash command is a pure
// `apply_patch <<'EOF' ... EOF` (optionally preceded by `cd <dir> &&`),
// validating its targets and patch body directly so patching a repository file
// whose basename is in UnlockCommands is not mistaken for running the helper.
func (h Hook) codexBashPureApplyPatch(ev event, cmd, cwd string) (isPure bool, reason, context string) {
	sc := scanShell(cmd)
	if len(sc.heredocs) != 1 {
		return false, "", ""
	}
	hd := sc.heredocs[0]
	if hd.op < 0 || hd.start <= hd.op || hd.end < hd.start || hd.end > len(cmd) {
		return false, "", ""
	}
	m := heredocOp.FindStringSubmatch(cmd[hd.op : hd.start-1])
	if m == nil || strings.TrimSpace(sc.plain[hd.op+len(m[0]):hd.start-1]) != "" {
		return false, "", ""
	}
	if strings.Contains(strings.TrimSpace(cmd[hd.end:]), "\n") {
		return false, "", ""
	}
	head := strings.TrimSpace(sc.plain[:hd.op])
	if head == "" || strings.ContainsAny(head, "|;()`$<>") {
		return false, "", ""
	}
	headSc := scanShell(head)
	segs := headSc.segments()
	if len(segs) == 0 || len(segs) > 2 {
		return false, "", ""
	}
	patchCWD := cwd
	switch len(segs) {
	case 1:
		w := shellWords(headSc.plain[segs[0].start:segs[0].end])
		if len(w) != 1 || w[0] != codexApplyPatchTool {
			return false, "", ""
		}
	case 2:
		if segs[0].after != "&&" {
			return false, "", ""
		}
		w0 := shellWords(headSc.plain[segs[0].start:segs[0].end])
		w1 := shellWords(headSc.plain[segs[1].start:segs[1].end])
		if len(w0) != 2 || w0[0] != "cd" || len(w1) != 1 || w1[0] != codexApplyPatchTool {
			return false, "", ""
		}
		states := walkSegments(headSc, cwd, h.Upstream.Git != nil)
		if len(states) < 2 || !states[1].known || states[1].dir == "" {
			return true, "Refused: apply_patch after an unfollowed `cd` has an unknown working directory; failing closed.", ""
		}
		patchCWD = states[1].dir
	}
	patchBody := cmd[hd.start:hd.end]
	if !hd.quoted {
		for _, sub := range heredocSubstitutions(patchBody) {
			if r := h.codexCommandRefusal(ev, sub, patchCWD); r != "" {
				return true, r, ""
			}
		}
		return true, "Refused: apply_patch heredoc delimiter must be quoted (<<'EOF') so shell command substitutions cannot run inside the patch body.", ""
	}
	r, ctx := h.checkCodexApplyPatch(ev, patchBody, patchCWD)
	return true, r, ctx
}

func guardedCodexHomes() []string {
	var dirs []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = expandHome(p)
		if !filepath.IsAbs(p) {
			return
		}
		clean := filepath.Clean(p)
		if !slices.Contains(dirs, clean) {
			dirs = append(dirs, clean)
		}
		if real, err := resolveSymlinkPath(clean); err == nil && real != "" && !slices.Contains(dirs, real) {
			dirs = append(dirs, real)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".codex"))
		add(filepath.Join(home, ".local", "state", "beekeeper", "codex", "home"))
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		add(filepath.Join(xdg, "beekeeper", "codex", "home"))
	}
	if ch := strings.TrimSpace(os.Getenv("CODEX_HOME")); ch != "" {
		add(ch)
	}
	return dirs
}

// resolveSymlinkPath resolves symlinks on p, or on its nearest existing
// ancestor when p itself does not exist or is a dangling symlink.
func resolveSymlinkPath(p string) (string, error) {
	return resolveSymlinkDepth(p, 0)
}

func resolveSymlinkDepth(p string, depth int) (string, error) {
	if depth > 16 {
		return filepath.Clean(p), nil
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real), nil
	}
	if link, err := os.Readlink(p); err == nil && link != "" {
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(p), link)
		}
		return resolveSymlinkDepth(filepath.Clean(link), depth+1)
	}
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	if dir == p {
		return filepath.Clean(p), nil
	}
	realDir, err := resolveSymlinkDepth(dir, depth+1)
	if err != nil {
		return filepath.Clean(p), err
	}
	joined := filepath.Join(realDir, base)
	if real, err := filepath.EvalSymlinks(joined); err == nil {
		return filepath.Clean(real), nil
	}
	if link, err := os.Readlink(joined); err == nil && link != "" {
		if !filepath.IsAbs(link) {
			link = filepath.Join(realDir, link)
		}
		return resolveSymlinkDepth(filepath.Clean(link), depth+1)
	}
	return filepath.Clean(joined), nil
}

func isUnderDir(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	p = filepath.Clean(p)
	dir = filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

func isGuardedCodexHomePath(rawPath, cwd string) bool {
	return guardedCodexPath(rawPath, cwd, true)
}

// A link replacement follows its parent directory, but not the old link's
// target. Keep the Codex home write guard without treating that as a read.
func guardedCodexPath(rawPath, cwd string, followLeaf bool) bool {
	p := wordPath(rawPath, cwd)
	if p == "" {
		return false
	}
	cands := []string{p}
	if followLeaf {
		if real, err := resolveSymlinkPath(p); err == nil && real != "" && real != p {
			cands = append(cands, real)
		}
	}
	if dirReal, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil && dirReal != "" {
		joined := filepath.Clean(filepath.Join(dirReal, filepath.Base(p)))
		if !slices.Contains(cands, joined) {
			cands = append(cands, joined)
		}
	}
	homes := guardedCodexHomes()
	for _, c := range cands {
		for _, h := range homes {
			if isUnderDir(c, h) {
				return true
			}
		}
		slash := filepath.ToSlash(c)
		if strings.HasSuffix(slash, "/codex/home") || strings.Contains(slash, "/codex/home/") ||
			strings.HasSuffix(slash, "/.codex") || strings.Contains(slash, "/.codex/") {
			return true
		}
	}
	return false
}

func containsGuardedCodexHomeRef(s string) bool {
	if s == "" {
		return false
	}
	for _, ref := range []string{"/.codex", "~/codex/home", "/codex/home", "$CODEX_HOME", "${CODEX_HOME}"} {
		if strings.Contains(s, ref) {
			return true
		}
	}
	for _, h := range guardedCodexHomes() {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

// codexPathRefusal checks a file path for secret files (both unresolved and
// symlink-resolved), guarded Codex homes on write, and installed unlock helpers.
func (h Hook) codexPathRefusal(rawPath, cwd string, write bool) string {
	p := wordPath(rawPath, cwd)
	if p == "" {
		return ""
	}
	sf := newSecretFiles(h.SecretFiles)
	cands := []string{p}
	if real, err := resolveSymlinkPath(p); err == nil && real != "" && real != p {
		cands = append(cands, real)
	}
	if dirReal, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil && dirReal != "" {
		joined := filepath.Clean(filepath.Join(dirReal, filepath.Base(p)))
		if !slices.Contains(cands, joined) {
			cands = append(cands, joined)
		}
	}
	for _, c := range cands {
		if file := sf.match(c); file != "" {
			return leak{what: "access to " + file + ", a file that holds secret values", safe: fileReadSafe(file)}.reason()
		}
		if strings.HasSuffix(filepath.ToSlash(c), "/auth.json") && isGuardedCodexHomePath(c, cwd) {
			return leak{what: "access to " + c + ", a file that holds secret values", safe: fileReadSafe(c)}.reason()
		}
	}
	if write && isGuardedCodexHomePath(rawPath, cwd) {
		return codexHomeRefusalMsg
	}
	if len(h.UnlockCommands) > 0 {
		base := filepath.Base(p)
		if slices.Contains(h.UnlockCommands, base) {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				localBin := filepath.Join(home, ".local", "bin")
				homeBin := filepath.Join(home, "bin")
				for _, c := range cands {
					if isUnderDir(c, localBin) || isUnderDir(c, homeBin) {
						return leak{what: base + ", a vault unlock", never: true, unlock: true}.reason()
					}
				}
			}
			if write {
				if cwd == "" {
					return leak{what: base + ", a vault unlock", never: true, unlock: true}.reason()
				}
				cwdReal, _ := resolveSymlinkPath(cwd)
				for _, c := range cands {
					if !isUnderDir(c, cwd) && (cwdReal == "" || !isUnderDir(c, cwdReal)) {
						return leak{what: base + ", a vault unlock", never: true, unlock: true}.reason()
					}
				}
			}
		}
	}
	return ""
}

func (h Hook) codexToolPathRefusal(ev event) string {
	if ev.ToolInput == nil {
		return ""
	}
	write := ev.ToolName == writeTool || ev.ToolName == editTool || ev.ToolName == multiEditTool || ev.ToolName == notebookTool
	for _, k := range []string{filePathKey, pathKey, notebookPathKey} {
		if s, ok := ev.ToolInput[k].(string); ok && s != "" {
			if r := h.codexPathRefusal(s, ev.CWD, write); r != "" {
				return r
			}
		}
	}
	return ""
}

var (
	anyRedirRe   = regexp.MustCompile(`(?:^|[\s;|&()])((?:\d+)?(?:>>|\|>|>\||<>|<<-|<<<|<<|>|<)|&>>|&>)\s*([^\s;|&()<>]+)`)
	writeCmdsMap = map[string]bool{
		"cp": true, "mv": true, "ln": true, "ins" + "tall": true, "dd": true,
		"rm": true, "rmdir": true, "unlink": true, "shred": true, "trash": true, "trash-put": true,
		teeCmd: true, "trun" + "cate": true, "touch": true, "mkdir": true, "chmod": true, "chown": true,
		codexPatchKey: true,
	}
)

// codexBashRefusal checks a Bash command for writes/redirections/modifications
// to guarded Codex homes ($CODEX_HOME and ~/.codex), secret files via symlinked
// paths, inline apply_patch commands, unquoted heredoc command substitutions,
// and option/redirection abuse on exempted beekeeper text commands.
func (h Hook) codexBashRefusal(ev event, cmd, cwd string) (reason, context string) {
	if script := standaloneLinkScript(cmd); script != "" {
		if raw, state := readScript(wordPath(script, cwd)); state == scriptRead {
			if r, ctx := h.codexBashTextRefusal(ev, string(raw), cwd); r != "" {
				return r, ctx
			}
		}
	}
	return h.codexBashTextRefusal(ev, cmd, cwd)
}

func (h Hook) codexBashTextRefusal(ev event, cmd, cwd string) (reason, context string) {
	rawSc := scanShell(cmd)
	for _, hd := range rawSc.heredocs {
		if !hd.quoted && hd.start <= hd.end && hd.end <= len(cmd) {
			for _, sub := range heredocSubstitutions(cmd[hd.start:hd.end]) {
				if r := h.codexCommandRefusal(ev, sub, cwd); r != "" {
					return r, ""
				}
			}
		}
	}
	outerPlain, inners, _ := opaquePlain(rawSc.plain)
	sc := scanShell(outerPlain)
	sc.heredocs = rawSc.heredocs
	segs := sc.segments()
	vars := scanVars(sc, segs)
	metadata := metadataFunctions(cmd)
	allowance := simpleLinkAllowance(cmd, metadata)
	states := walkSegments(sc, cwd, h.Upstream.Git != nil)

	for _, in := range inners {
		r, ctx := h.codexBashRefusal(ev, in, cwd)
		if r != "" {
			return r, ""
		}
		if context == "" {
			context = ctx
		}
	}

	// Check every redirection in the outer command (stdout, stderr, &>, <>, <, <<<).
	for i, st := range states {
		sg := st.seg
		segPlain := sc.plain[sg.start:sg.end]
		for _, m := range anyRedirRe.FindAllStringSubmatch(segPlain, -1) {
			op, target := m[1], strings.Trim(m[2], `"'`)
			if target == "" || strings.HasPrefix(target, "&") {
				continue
			}
			isWrite := strings.Contains(op, ">")
			if r := h.codexPathRefusal(target, st.dir, isWrite); r != "" {
				return r, ""
			}
			if !isWrite && (isGuardedCodexHomePath(target, st.dir) || containsGuardedCodexHomeRef(target)) {
				return codexHomeRefusalMsg, ""
			}
		}

		rawWords := shellWords(segPlain)
		if len(rawWords) == 0 {
			continue
		}
		for _, words := range expandWordsList(rawWords, vars) {
			words = allowance.resolve(words)
			r, ctx := h.codexSegmentRefusal(ev, cmd, sc, segs, i, words, st.dir, st.known, metadata, allowance.allowed)
			if r != "" {
				return r, ""
			}
			if context == "" {
				context = ctx
			}
		}
	}
	return "", context
}

func (h Hook) codexSegmentRefusal(ev event, cmd string, sc shellScan, segs []segment, segIdx int, words []string, dir string, known bool, metadata map[string]string, linksAllowed bool) (reason, context string) {
	c := commandAt(words)
	if c >= len(words) {
		return "", ""
	}
	name := path.Base(words[c])
	args := words[c+1:]
	sf := newSecretFiles(h.SecretFiles)
	link := linkArgs(name, args, metadata)
	isSymbolic := linksAllowed && sf.symbolicMetadata(link, []string{dir})
	flag, helper := metadata[name]
	// linkChecked: a link to a guarded file that passes; the Bash decision
	// refuses what reads it (secretlinksmade.go)
	linkChecked := false
	if (name == "ln" || name == linkCmd || helper) && !isSymbolic {
		g := secretGuard{unlock: h.UnlockCommands, cwd: dir, files: sf, metadata: metadata, funcs: functionBodies(cmd), vars: scanVars(sc, segs), cut: cutBySubstitution(segs[segIdx])}
		l, checked := g.linkLeak(name, args, []string{dir}, 0)
		if l != nil {
			return l.reason(), ""
		}
		linkChecked = checked
	}
	// lnArgs: the ln a checked link runs, nil for a helper whose body was
	// inlined, which keeps the refusal of a Codex home path handed to it
	var lnArgs []string
	switch {
	case linkChecked && !helper:
		lnArgs = args
	case linkChecked && flag != "":
		lnArgs = link
	}
	isMeta := metaCmds[name] || isSymbolic || linkChecked
	if helper {
		isMeta = isSymbolic || linkChecked
	}
	if name == findCmd {
		if file := sf.findSource(args, []string{dir}, false); file != "" {
			return leak{what: "a find that runs a reader on " + file + ", a file that holds secret values", safe: fileReadSafe(file)}.reason(), ""
		}
		isMeta = isMeta && !findReads(args)
	}
	// A function defined on the command line has an unchecked body: what it
	// is handed may be written, so a guarded Codex home path is refused.
	if helper && !isSymbolic && lnArgs == nil {
		for _, a := range args {
			if a = strings.TrimPrefix(strings.TrimPrefix(a, "if="), "of="); !strings.HasPrefix(a, "-") && (isGuardedCodexHomePath(a, dir) || containsGuardedCodexHomeRef(a)) {
				return codexHomeRefusalMsg, ""
			}
		}
	}

	// Check command word itself against guarded codex homes.
	if (strings.Contains(words[c], "/") || strings.Contains(words[c], "~") || strings.Contains(words[c], "$")) &&
		isGuardedCodexHomePath(words[c], dir) {
		return codexHomeRefusalMsg, ""
	}

	// If apply_patch is invoked inside a non-pure Bash command line, validate
	// its heredoc body and known directory or fail closed.
	if name == codexApplyPatchTool {
		if !known || dir == "" {
			return "Refused: apply_patch after an unfollowed `cd` has an unknown working directory; failing closed.", ""
		}
		sg := segs[segIdx]
		foundHeredoc := false
		for _, hd := range sc.heredocs {
			if hd.op >= sg.start && hd.op < sg.end {
				foundHeredoc = true
				if !hd.quoted {
					for _, sub := range heredocSubstitutions(cmd[hd.start:hd.end]) {
						if r := h.codexCommandRefusal(ev, sub, dir); r != "" {
							return r, ""
						}
					}
					return "Refused: apply_patch heredoc delimiter must be quoted (<<'EOF') so shell command substitutions cannot run inside the patch body.", ""
				}
				r, ctx := h.checkCodexApplyPatch(ev, cmd[hd.start:hd.end], dir)
				if r != "" {
					return r, ""
				}
				if context == "" {
					context = ctx
				}
			}
		}
		if !foundHeredoc {
			return "Refused: apply_patch in Bash without an inline heredoc patch cannot be verified; failing closed.", ""
		}
	}

	// Check `beekeeper` option values (--config, --as) and command substitutions.
	if name == selfCmd {
		for i := 0; i < len(args); i++ {
			a := args[i]
			var optVal string
			switch {
			case (a == flagConfig || a == flagAs) && i+1 < len(args):
				optVal = args[i+1]
				i++
			case strings.HasPrefix(a, "--config="):
				optVal = strings.TrimPrefix(a, "--config=")
			case strings.HasPrefix(a, "--as="):
				optVal = strings.TrimPrefix(a, "--as=")
			}
			if optVal == "" {
				continue
			}
			if optVal == unknownMarker || strings.Contains(optVal, unknownMarker) ||
				isGuardedCodexHomePath(optVal, dir) || containsGuardedCodexHomeRef(optVal) {
				return codexHomeRefusalMsg, ""
			}
			if r := h.codexPathRefusal(optVal, dir, false); r != "" {
				return r, ""
			}
			for _, pf := range h.PersonFolders {
				if isUnderDir(wordPath(optVal, dir), expandHome(pf)) {
					return "Refused: `" + a + "` names a protected folder.", ""
				}
			}
		}
	}

	// Check commands that write, modify, copy, link, or delete files against guarded codex homes.
	isWriteCmd := writeCmdsMap[name] ||
		((name == "s"+"ed" || name == gsedCmd || name == perlCmd) && hasInPlaceFlag(args))
	if isSymbolic {
		if r := codexLinkWriteRefusal(link, dir); r != "" {
			return r, ""
		}
	} else if lnArgs != nil {
		words, ok := lnLinkWords(lnArgs, dir)
		if !ok {
			return codexHomeRefusalMsg, ""
		}
		for _, w := range words {
			if guardedCodexPath(w, dir, false) || containsGuardedCodexHomeRef(w) {
				return codexHomeRefusalMsg, ""
			}
		}
	} else if isWriteCmd {
		for _, a := range args {
			a = strings.TrimPrefix(strings.TrimPrefix(a, "if="), "of=")
			if strings.HasPrefix(a, "-") {
				continue
			}
			if r := h.codexPathRefusal(a, dir, true); r != "" {
				return r, ""
			}
		}
	}

	// Check inline interpreter scripts (python -c, perl -e, etc.) and sourced files.
	if name == sourceCmd || name == "." {
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && isGuardedCodexHomePath(a, dir) {
				return codexHomeRefusalMsg, ""
			}
		}
	}
	if interpreters[name].code != nil || len(interpreters[name].file) > 0 {
		for _, a := range args {
			if isGuardedCodexHomePath(a, dir) || containsGuardedCodexHomeRef(a) {
				return codexHomeRefusalMsg, ""
			}
		}
	}
	if shells[name] || name == evalCmd {
		for _, a := range args {
			if isGuardedCodexHomePath(a, dir) {
				return codexHomeRefusalMsg, ""
			}
		}
	}

	// Check every argument of non-metadata commands for secret files (e.g. symlinked auth.json).
	isQuietGrep := grepCmds[name] && slices.ContainsFunc(args, grepQuiet.MatchString)
	isJQ := name == "jq" || name == gojqCmd || name == "yq"
	if !isMeta && !isQuietGrep && !isJQ && !isBeekeeperTextSubcommand(name, args) {
		for _, a := range args {
			if _, v, ok := strings.Cut(a, "="); ok {
				a = v
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if r := h.codexPathRefusal(a, dir, false); r != "" {
				return r, ""
			}
		}
	}
	return "", context
}

func codexLinkWriteRefusal(args []string, dir string) string {
	for _, p := range symbolicLinkPaths(args, dir) {
		if guardedCodexPath(p.link, dir, false) {
			return codexHomeRefusalMsg
		}
	}
	return ""
}

func isBeekeeperTextSubcommand(name string, args []string) bool {
	if name != selfCmd {
		return false
	}
	var sub []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == flagConfig || a == flagAs || a == "--for" || a == "--ref" || a == "--due" ||
			a == "--default" || a == "--status-quo" || a == "--why" || a == "--on" || a == "--report" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		sub = append(sub, a)
		if len(sub) == 2 {
			break
		}
	}
	if len(sub) < 2 {
		return false
	}
	switch sub[0] + " " + sub[1] {
	case "note add", "log add", "agents idle", "agents park", "agents note":
		return true
	}
	return false
}
