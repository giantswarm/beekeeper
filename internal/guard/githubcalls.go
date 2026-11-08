package guard

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

const (
	ghHostnameFlag = "--hostname"
	ghReleaseSub   = "release"
	ghViewSub      = "view"
)

// GitHubCall describes an invocation, not measured GraphQL points. Arguments
// are deliberately omitted: queries and flags can contain private information.
type GitHubCall struct {
	Class string `json:"class"`
	Heavy bool   `json:"heavy,omitempty"`
}

// GitHubCalls reads shell calls in each supported PreToolUse payload without
// executing commands or contacting GitHub.
func GitHubCalls(raw []byte, format string) (string, []GitHubCall) {
	return githubCalls(raw, format, false)
}

// GitHubRESTCalls records admitted REST shell invocations without their arguments.
func GitHubRESTCalls(raw []byte, format string) (string, []GitHubCall) {
	return githubCalls(raw, format, true)
}

func githubCalls(raw []byte, format string, rest bool) (string, []GitHubCall) {
	var ev event
	if format == "agy" {
		var reason string
		ev, _, reason = parseAgyEvent(raw)
		if reason != "" {
			return "", nil
		}
	} else if json.Unmarshal(raw, &ev) != nil {
		return "", nil
	}
	var command string
	if format == "codex" {
		if !isCodexShellTool(ev.ToolName) {
			return ev.Session, nil
		}
		command, _, _ = extractCodexShellCommand(ev.ToolInput)
	} else {
		if ev.ToolName != bashTool {
			return ev.Session, nil
		}
		command, _ = ev.ToolInput[commandKey].(string)
	}
	if rest {
		return ev.Session, restCalls(command)
	}
	return ev.Session, graphQLCalls(command)
}

func graphQLCalls(command string) []GitHubCall {
	var calls []GitHubCall
	for _, words := range Commands(command) {
		// commandAt handles env assignments and common command wrappers.
		for i, word := range words {
			tool := filepath.Base(word)
			if tool != "gh" && tool != selfCmd {
				continue
			}
			if commandAt(words) != i {
				continue
			}
			args := words[i+1:]
			if tool == "gh" {
				for len(args) > 0 {
					if (args[0] == "-R" || args[0] == flagRepo || args[0] == ghHostnameFlag) && len(args) > 1 {
						args = args[2:]
						continue
					}
					if strings.HasPrefix(args[0], flagRepo+"=") || strings.HasPrefix(args[0], ghHostnameFlag+"=") {
						args = args[1:]
						continue
					}
					break
				}
			}
			if len(args) < 1 {
				break
			}
			class := ""
			if tool == selfCmd && args[0] == "board" {
				class = "beekeeper board"
			}
			if tool == "gh" && len(args) >= 2 {
				switch args[0] {
				case "project":
					if slices.Contains([]string{"item-list", "item-add", "item-edit", "item-delete", "item-archive", verbList, ghViewSub, "create", verbEdit, "close", "copy", verbDelete, "field-list", "field-create", "field-delete", "link", unlinkCmd, "mark-template"}, args[1]) {
						class = "gh project " + args[1]
					}
				case "api":
					if args[1] == executionGraphQL {
						class = "gh api graphql"
					}
				case "pr":
					if slices.Contains([]string{ghViewSub, verbList, "checks"}, args[1]) {
						class = "gh pr " + args[1]
					}
				case ghIssueSub:
					if slices.Contains([]string{ghViewSub, verbList}, args[1]) {
						class = "gh issue " + args[1]
					}
				case ghReleaseSub:
					if args[1] == verbList {
						class = "gh release list"
					}
				}
			}
			if class != "" && !slices.Contains(args, "--help") && !slices.Contains(args, "-h") {
				calls = append(calls, GitHubCall{Class: class, Heavy: class == "gh project item-list" || slices.Contains(args, "--paginate")})
			}
			break
		}
	}
	return calls
}

// REST endpoints and flags may contain private values; retain command classes only.
func restCalls(command string) []GitHubCall {
	var calls []GitHubCall
	for _, words := range Commands(command) {
		i := commandAt(words)
		if i < 0 || i >= len(words) || filepath.Base(words[i]) != "gh" {
			continue
		}
		args := words[i+1:]
		for len(args) > 1 && (args[0] == "-R" || args[0] == flagRepo || args[0] == ghHostnameFlag || strings.HasPrefix(args[0], flagRepo+"=") || strings.HasPrefix(args[0], ghHostnameFlag+"=")) {
			if strings.Contains(args[0], "=") {
				args = args[1:]
			} else {
				args = args[2:]
			}
		}
		if len(args) < 2 || slices.Contains(args, "--help") || slices.Contains(args, "-h") {
			continue
		}
		class := ""
		switch args[0] {
		case "api":
			// graphql may follow flags; never retain endpoint arguments.
			if !slices.Contains(args, executionGraphQL) {
				class = "gh api REST"
			}
		case "run", "workflow", "release":
			if slices.Contains([]string{"view", "list", "download", "watch", "create", "delete", "edit", "upload", "rerun", "cancel", "enable", "disable"}, args[1]) && !(args[0] == "release" && args[1] == "list") {
				class = "gh " + args[0] + " " + args[1]
			}
		case "issue":
			if slices.Contains([]string{"create", "comment", "edit", "close", "reopen", "delete"}, args[1]) {
				class = "gh issue " + args[1]
			}
		case "pr":
			if slices.Contains([]string{"diff", "comment", "review"}, args[1]) {
				class = "gh pr " + args[1]
			}
		}
		if class != "" {
			calls = append(calls, GitHubCall{Class: class, Heavy: slices.Contains(args, "--paginate") || slices.Contains(args, "--watch") || args[1] == "watch"})
		}
	}
	return calls
}
