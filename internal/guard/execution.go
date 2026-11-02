package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const (
	executionConfigFlag  = "--config"
	executionRunCommand  = "run"
	executionUnset       = "--unset"
	executionSignal      = "--signal"
	executionSudo        = "sudo"
	executionMerge       = "merge"
	executionGraphQL     = "graphql"
	executionCurl        = "curl"
	executionDash        = "dash"
	executionSplitString = "--split-string"
	executionKillAfter   = "--kill-after"
	executionNice        = "nice"
	executionTimeout     = "timeout"
	executionNohup       = "nohup"
	executionStdbuf      = "stdbuf"
	executionSystemdRun  = "systemd-run"
	executionPerl        = "perl"
	executionPatch       = "patch"
	executionEnv         = "env"
)

// ExecutionDeny is also used when the host's configuration or journal cannot
// load: a command hook must return an explicit blocking decision on errors.
func ExecutionDeny(reason string) []byte {
	return answer(hookOutput{PermissionDecision: decisionDeny, Reason: reason})
}

// DecideExecution narrows the first remote adapter's tool surface and then
// runs the existing guards. It deliberately has no fail-open scope path.
// Hooks inspect supported tool calls; they are not an OS sandbox for code
// executed by those calls or an account owner changing the harness itself.
func (h Hook) DecideExecution(raw []byte, session, workspace string, protected []string) (out []byte) {
	defer func() {
		if recover() != nil {
			out = ExecutionDeny("execution guard failed; tool call denied")
		}
	}()
	var ev event
	if json.Unmarshal(raw, &ev) != nil || ev.ToolInput == nil || ev.Session != session || session == "" || !filepath.IsAbs(workspace) {
		return ExecutionDeny("invalid managed execution tool event")
	}
	if h.ConfigErr != nil {
		return ExecutionDeny("execution guard configuration is unavailable")
	}
	if ev.CWD == "" {
		ev.CWD = workspace
	}
	if !executionWithin(workspace, ev.CWD) {
		return ExecutionDeny("tool working directory is outside its owned workspace")
	}
	switch ev.ToolName {
	case bashTool:
		command, ok := ev.ToolInput[commandKey].(string)
		if !ok || command == "" {
			return ExecutionDeny("invalid execution command")
		}
		if bg, _ := ev.ToolInput["run_in_background"].(bool); bg {
			return ExecutionDeny("remote background tool calls are unavailable in this slice; use foreground commands and component parks")
		}
		if err := executionShell(command, workspace, ev.CWD, protected, 0); err != nil {
			return ExecutionDeny(err.Error())
		}
	case readTool, writeTool, editTool, grepTool, globTool:
		key := "file_path"
		if ev.ToolName == grepTool || ev.ToolName == globTool {
			key = "path"
		}
		p, _ := ev.ToolInput[key].(string)
		if p == "" && key == "file_path" {
			return ExecutionDeny("file tool requires a target")
		}
		if p == "" && key == "path" {
			p = workspace
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(ev.CWD, p)
		}
		if p == "" || !executionWithin(workspace, p) {
			return ExecutionDeny("file tool target is outside its owned workspace")
		}
	default:
		return ExecutionDeny("tool is not supported by the managed remote adapter; use component commands for notes and reports")
	}
	// The execution journal and tool identity above have already established
	// this session as a managed worker, independent of the local starts roster.
	h.Transcripts.Managed = func(string) (bool, error) { return true, nil }
	return h.Decide(raw)
}

// codexWorkdirKey is the shell tools' working-directory argument.
const codexWorkdirKey = "workdir"

// ExecutionDenyCodex is ExecutionDeny in codex's PreToolUse format, which a
// codex hook needs: codex treats a non-JSON answer or an error exit as a pass.
func ExecutionDenyCodex(reason string) []byte { return DenyCodex(reason) }

// DecideExecutionCodex is DecideExecution for the remote codex adapter. The
// session is the codex thread the runner recorded. Shell commands, patch
// targets and image reads stay inside the owned workspace and pass the same
// remote command guard before the full codex guard runs; every other codex
// tool is refused.
func (h Hook) DecideExecutionCodex(raw []byte, session, workspace string, protected []string) (out []byte) {
	defer func() {
		if recover() != nil {
			out = ExecutionDenyCodex("execution guard failed; tool call denied")
		}
	}()
	if reason := ValidateCodexInput(raw); reason != "" {
		return ExecutionDenyCodex(reason)
	}
	var ev event
	if json.Unmarshal(raw, &ev) != nil || ev.Session != session || session == "" || !filepath.IsAbs(workspace) {
		return ExecutionDenyCodex("invalid managed execution tool event")
	}
	if h.ConfigErr != nil {
		return ExecutionDenyCodex("execution guard configuration is unavailable")
	}
	if ev.CWD == "" {
		ev.CWD = workspace
	}
	if !filepath.IsAbs(ev.CWD) || !executionWithin(workspace, ev.CWD) {
		return ExecutionDenyCodex("tool working directory is outside its owned workspace")
	}
	target := func(p string) bool {
		if !filepath.IsAbs(p) {
			p = filepath.Join(ev.CWD, p)
		}
		return executionWithin(workspace, p)
	}
	switch {
	case isCodexShellTool(ev.ToolName):
		command, _, _ := extractCodexShellCommand(ev.ToolInput)
		if strings.TrimSpace(command) == "" {
			return ExecutionDenyCodex("invalid execution command")
		}
		cwd := codexShellWorkdir(ev.CWD, ev.ToolInput)
		if !target(cwd) {
			return ExecutionDenyCodex("tool working directory is outside its owned workspace")
		}
		if err := executionShell(command, workspace, cwd, protected, 0); err != nil {
			return ExecutionDenyCodex(err.Error())
		}
	case ev.ToolName == codexApplyPatchTool:
		patch, _ := extractCodexPatch(ev.ToolInput)
		headers, reason := parseCodexPatchHeaders(patch)
		if reason != "" {
			return ExecutionDenyCodex(reason)
		}
		for _, hdr := range headers {
			if !target(hdr.target) {
				return ExecutionDenyCodex("patch target is outside its owned workspace")
			}
		}
	case ev.ToolName == codexWriteStdinTool:
		// An empty write only polls a running command; typed input would be
		// a shell command the guard never inspected.
		if chars, _ := ev.ToolInput[codexCharsKey].(string); chars != "" {
			return ExecutionDenyCodex("uninspected shell input is not supported remotely; run a guarded command directly")
		}
	case ev.ToolName == codexViewImageTool:
		if p, _ := ev.ToolInput[pathKey].(string); !target(p) {
			return ExecutionDenyCodex("file tool target is outside its owned workspace")
		}
	case ev.ToolName == codexUpdatePlanTool || ev.ToolName == codexWebTool:
	default:
		return ExecutionDenyCodex("tool is not supported by the managed remote adapter; use component commands for notes and reports")
	}
	if h.Project == "" {
		h.Project = workspace
	}
	h.Transcripts.Managed = func(string) (bool, error) { return true, nil }
	return h.DecideCodex(raw)
}

func executionWithin(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	r, err := filepath.Rel(root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return false
	}
	// For a not-yet-created file, resolve the nearest existing ancestor.
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			rel, err := filepath.Rel(root, real)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(p) == p {
			return false
		}
		p = filepath.Dir(p)
	}
}

func executionLiteral(w *syntax.Word) (string, error) {
	return expand.Literal(&expand.Config{Env: expand.ListEnviron(os.Environ()...)}, w)
}

// executionShell guards one remote command run in cwd, inside workspace.
// Relative operands are checked against every directory the command can be
// in: cwd and each literal cd or pushd target it names, since a subshell's
// change of directory may or may not outlast it. A dynamic change of
// directory cannot be followed and is refused.
func executionShell(command, workspace, cwd string, protected []string, depth int) error {
	if depth > 4 {
		return errors.New("nested execution commands exceed the guard limit")
	}
	if !filepath.IsAbs(cwd) {
		cwd = workspace
	}
	dirs := []string{filepath.Clean(cwd)}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return errors.New("remote command syntax cannot be guarded; use a direct shell command")
	}
	var refusal error
	syntax.Walk(f, func(n syntax.Node) bool {
		if refusal != nil {
			return false
		}
		if w, ok := n.(*syntax.Word); ok {
			value, err := executionLiteral(w)
			if err == nil {
				if key, _, ok := strings.Cut(value, "="); ok && executionIdentityVariable(key) {
					refusal = errors.New("managed execution identity and configuration cannot be overridden")
					return false
				}
				if value == "--as" || strings.HasPrefix(value, "--as=") {
					refusal = errors.New("managed execution identity cannot be overridden")
					return false
				}
				// Every redirection, including non-stdout descriptors, is a
				// word in this traversal. Prose arguments are not path words.
				p := value
				if _, v, found := strings.Cut(p, "="); found {
					p = v
				}
				if strings.HasPrefix(p, "-C/") {
					p = p[2:]
				}
				if !strings.ContainsAny(p, " \t\n") && executionProtectedTarget(p, workspace, dirs, protected) {
					refusal = errors.New("execution policy, credentials and other workspaces are protected")
					return false
				}
			}
		}
		if assign, ok := n.(*syntax.Assign); ok && assign.Name != nil && executionIdentityVariable(assign.Name.Value) {
			refusal = errors.New("managed execution identity and configuration cannot be overridden")
			return false
		}
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var words []string
		for _, w := range call.Args {
			v, err := executionLiteral(w)
			if err != nil {
				v = ""
			}
			words = append(words, v)
		}
		// Dynamic command words cannot establish lifecycle ownership.
		var source bytes.Buffer
		_ = syntax.NewPrinter().Print(&source, call.Args[0])
		if words[0] == "" || strings.ContainsAny(source.String(), "$`") {
			refusal = errors.New("dynamic executable is not supported by the remote guard")
			return false
		}
		words, refusal = executionUnwrap(words)
		if refusal != nil {
			return false
		}
		if len(words) == 0 {
			return true
		}
		name := filepath.Base(words[0])
		if name == "cd" || name == "pushd" {
			if slices.Contains(words[1:], "") {
				refusal = errors.New("dynamic directory change is not supported by the remote guard")
				return false
			}
			for _, target := range words[1:] {
				if target == "--" || strings.HasPrefix(target, "-") {
					continue
				}
				for _, d := range slices.Clone(dirs) {
					next := target
					if !filepath.IsAbs(next) {
						next = filepath.Join(d, next)
					}
					if next = filepath.Clean(next); !slices.Contains(dirs, next) {
						dirs = append(dirs, next)
					}
				}
				break
			}
		}
		switch name {
		case selfCmd:
			for _, word := range words[1:] {
				if word == "--" {
					break
				}
				if word == executionConfigFlag || strings.HasPrefix(word, "--config=") {
					refusal = errors.New("managed execution configuration cannot be overridden")
					return false
				}
			}
			args := executionVerb(words[1:])
			if len(args) > 1 && args[0] == "agents" {
				switch args[1] {
				case "start", "wake", "desktop", "resume", "handover", "hosts", "host-rpc", "coordinator-rpc", "execution-run", "execution-guard":
					refusal = errors.New("harness and host lifecycle belongs to the coordinating component")
					return false
				}
			}
			if len(args) > 0 && args[0] == executionRunCommand {
				target := executionRunTarget(args[1:])
				if len(target) > 0 {
					unwrapped, err := executionUnwrap(target)
					if err != nil {
						refusal = err
						break
					}
					if len(unwrapped) > 0 {
						var executable bytes.Buffer
						_ = syntax.NewPrinter().Print(&executable, call.Args[len(call.Args)-len(unwrapped)])
						if strings.ContainsAny(executable.String(), "$`") {
							refusal = errors.New("dynamic executable is not supported by the remote guard")
							break
						}
					}
					quoted := make([]string, len(target))
					for i, word := range target {
						quoted[i] = ShellQuote(word)
					}
					refusal = executionShell(strings.Join(quoted, " "), workspace, cwd, protected, depth+1)
				}
			}
		case unsetCmd:
			for _, arg := range words[1:] {
				if executionIdentityVariable(arg) {
					refusal = errors.New("managed execution identity and configuration cannot be removed")
				}
			}
		case "systemctl", executionSystemdRun, "loginctl", executionSudo, "claude", "codex", "agy", ompCommand:
			refusal = errors.New("harness and service lifecycle belongs to the coordinating component")
		case "gh", "devctl":
			args := executionVerb(words[1:])
			if len(args) > 1 && (args[0] == "pr" && args[1] == executionMerge || args[0] == "release" && args[1] == "promote") || len(args) > 0 && args[0] == "alias" {
				refusal = errors.New("remote merge authorization is unavailable; see https://github.com/giantswarm/beekeeper/issues/1716")
			}
			if len(args) > 0 && args[0] == "api" {
				for _, arg := range args[1:] {
					lower := strings.ToLower(arg)
					if strings.Contains(lower, "/merge") || lower == executionGraphQL || lower == "-f" || lower == "--field" || lower == "--raw-field" || strings.HasPrefix(lower, "--field=") || strings.HasPrefix(lower, "--raw-field=") || strings.HasPrefix(lower, "query=") && strings.Contains(lower, "mutation") || lower == "post" || lower == "put" || lower == executionPatch || lower == "delete" || lower == "--input" || strings.HasPrefix(lower, "--input=") {
						refusal = errors.New("remote API mutations require the coordinating component; remote merge authority is unavailable")
					}
				}
			}
		case executionCurl, "wget", "python", "python3", "node", executionPerl, "ruby":
			args := strings.ToLower(strings.Join(words[1:], " "))
			if strings.Contains(args, "mergepullrequest") || strings.Contains(args, "/merge") || strings.Contains(args, "pr merge") {
				refusal = errors.New("remote merge authorization is unavailable; see https://github.com/giantswarm/beekeeper/issues/1716")
			}
		case "git":
			for _, a := range words[1:] {
				if strings.Contains(a, "alias.") || strings.Contains(a, "core.sshCommand") {
					refusal = errors.New("execution command aliases cannot bypass the remote guard")
				}
			}
		case bashCmd, "sh", zshCmd, executionDash, evalCmd, sourceCmd, ".":
			var script string
			if name == evalCmd {
				script = strings.Join(words[1:], " ")
			} else {
				for i, arg := range words[1:] {
					if strings.HasPrefix(arg, "-") && strings.Contains(arg, "c") && i+2 < len(words) {
						script = words[i+2]
						break
					}
				}
			}
			if script != "" {
				refusal = executionShell(script, workspace, cwd, protected, depth+1)
			} else {
				refusal = errors.New("uninspected shell input is not supported remotely; run a guarded command directly")
			}
		}
		return refusal == nil
	})
	return refusal
}

// executionProtectedTarget says whether path word p, resolved from any of
// dirs, reaches a protected root outside the owned workspace.
func executionProtectedTarget(p, workspace string, dirs, protected []string) bool {
	candidates := []string{p}
	if !filepath.IsAbs(p) {
		candidates = candidates[:0]
		for _, d := range dirs {
			candidates = append(candidates, filepath.Join(d, p))
		}
	}
	for _, c := range candidates {
		real := executionResolve(c)
		for _, root := range protected {
			if (executionLexicalWithin(root, c) || executionLexicalWithin(root, real)) && !executionWithin(workspace, c) {
				return true
			}
		}
	}
	return false
}

func executionResolve(p string) string {
	var suffix []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(p) == p {
			return p
		}
		suffix = append(suffix, filepath.Base(p))
		p = filepath.Dir(p)
	}
}

// ExecutionRulesAllowed rejects authentication stores before opening a rules
// file. Rules are an operator-owned document, never provider credentials.
func ExecutionRulesAllowed(p string, extra []string) bool {
	if newSecretFiles(extra).match(p) != "" {
		return false
	}
	p = executionResolve(p)
	base := strings.ToLower(filepath.Base(p))
	if strings.Contains(base, "credential") || strings.Contains(base, "token") || strings.HasPrefix(base, ".env") || strings.Contains(base, "secret") {
		return false
	}
	for _, q := range extra {
		if executionLexicalWithin(executionResolve(q), p) {
			return false
		}
	}
	home, _ := os.UserHomeDir()
	for _, q := range []string{".ssh", ".config/gh", ".claude/.credentials.json", ".codex/auth.json", ".aws", ".kube", ".netrc", ".git-credentials"} {
		if executionLexicalWithin(filepath.Join(home, q), p) {
			return false
		}
	}
	return filepath.IsAbs(p)
}

func executionLexicalWithin(root, p string) bool {
	r, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

func executionVerb(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "--repo", "-R", "--hostname", executionConfigFlag, "--request-id":
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		default:
			args = args[1:]
		}
	}
	return args
}

func executionRunTarget(args []string) []string {
	for len(args) > 0 {
		if args[0] == "--" {
			return args[1:]
		}
		if !strings.HasPrefix(args[0], "-") {
			return args
		}
		switch args[0] {
		case "--max", "--swap", "--wait", "--class":
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		default:
			args = args[1:]
		}
	}
	return nil
}

func executionUnwrap(words []string) ([]string, error) {
	for len(words) > 0 {
		name := filepath.Base(words[0])
		switch name {
		case executionEnv, commandKey, execCmd, executionNohup, executionNice, executionTimeout, executionStdbuf:
		default:
			return words, nil
		}
		words = words[1:]
		for len(words) > 0 {
			a := words[0]
			if name == executionEnv && strings.Contains(a, "=") && !strings.HasPrefix(a, "-") {
				words = words[1:]
				continue
			}
			if !strings.HasPrefix(a, "-") {
				break
			}
			if name == executionEnv && (a == "-S" || a == executionSplitString) {
				return nil, errors.New("split-string execution wrappers cannot be guarded remotely")
			}
			if name == executionEnv && (a == "-i" || a == "--ignore-environment") || name == execCmd && a == "-c" {
				return nil, errors.New("managed execution environment cannot be cleared")
			}
			needsValue := a == "-u" || a == executionUnset || a == "-C" || a == chdirFlag || a == "-n" || a == "-a" || a == "-s" || a == executionSignal || a == "-k" || a == executionKillAfter || a == "-o" || a == "-e" || a == "-i"
			// env -i takes no value; stdbuf -i does.
			if name == executionEnv && a == "-i" {
				needsValue = false
			}
			words = words[1:]
			if needsValue {
				if len(words) == 0 {
					return nil, errors.New("incomplete execution wrapper")
				}
				if name == executionEnv && (a == "-u" || a == executionUnset) && executionIdentityVariable(words[0]) {
					return nil, errors.New("managed execution identity and configuration cannot be removed")
				}
				words = words[1:]
			}
		}
		if name == executionTimeout {
			if len(words) < 2 {
				return nil, errors.New("incomplete timeout wrapper")
			}
			words = words[1:]
		}
		if len(words) > 0 && words[0] == "" {
			return nil, errors.New("dynamic executable is not supported remotely")
		}
	}
	return words, nil
}

func executionIdentityVariable(name string) bool {
	return name == "BEEKEEPER_EXECUTION_ID" || name == "BEEKEEPER_EXECUTION_COORDINATOR" || name == "BEEKEEPER_CONFIG"
}
