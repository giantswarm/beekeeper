package guard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var mergeHead = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var mergeEndpoint = regexp.MustCompile(`^(?:https://api\.github\.com/)?/?repos/([\w.-]+/[\w.-]+)/pulls/([0-9]+)/merge$`)
var mergePullURL = regexp.MustCompile(`^https://github\.com/([\w.-]+/[\w.-]+)/pull/([0-9]+)$`)

// Only a component-recorded go is authority. The head must also be enforced
// by GitHub atomically; a preflight read alone leaves a race with a push.
func (h Hook) mergeGo(repo, number, head string) string {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || repoSlug(repo) == "" {
		return "Refused: name the merge's repository and numeric pull request explicitly."
	}
	if !mergeHead.MatchString(head) {
		return "Refused: pin the reviewed head with --match-head-commit <full SHA> (gh pr merge) or -f sha=<full SHA> (gh api)."
	}
	if h.MergeGo == nil {
		return "Refused: no go from the supervisor recorded on disk; park for desk review and the exact-head merge go."
	}
	if why := h.MergeGo(h.mergeSession, repo, n, head); why != "" {
		return "Refused: " + why
	}
	return ""
}

func (h Hook) ghMergeGo(args, pos []string, repo, cwd string, known bool) string {
	if len(pos) >= 2 && pos[0] == "pr" && pos[1] == executionMerge {
		number := ""
		if len(pos) == 3 {
			number = pos[2]
		}
		if m := mergePullURL.FindStringSubmatch(number); m != nil {
			repo, number = m[1], m[2]
		}
		if repo == "" {
			repo = h.Upstream.baseRepo(cwd, "", known)
		}
		head := ""
		for i, arg := range args {
			if arg == "--auto" || strings.HasPrefix(arg, "--auto=") {
				return "Refused: deferred automatic merges outlive the supervisor's exact-head go; merge synchronously."
			}
			if arg == "--match-head-commit" && i+1 < len(args) {
				head = args[i+1]
			}
			if v, ok := strings.CutPrefix(arg, "--match-head-commit="); ok {
				head = v
			}
		}
		return h.mergeGo(repo, number, head)
	}
	if len(pos) == 0 || pos[0] != ghAPISub {
		return ""
	}
	words := append([]string{"gh"}, args...)
	endpoint := ""
	if len(pos) > 1 {
		endpoint = pos[1]
	}
	if endpoint == executionGraphQL {
		if strings.Contains(strings.Join(args, " ")+ghAPIBodies(words, cwd), "mergePullRequest") {
			return "Refused: use gh pr merge with --match-head-commit so the supervisor go can be checked by repository, pull request and head."
		}
		return ""
	}
	if !writeMethods[ghAPIMethod(args)] {
		return ""
	}
	m := mergeEndpoint.FindStringSubmatch(endpoint)
	if m == nil {
		if strings.Contains(endpoint, "/merge") {
			return "Refused: the guard cannot resolve this merge endpoint; use gh pr merge with --repo and --match-head-commit."
		}
		return ""
	}
	head := ""
	for i, arg := range args {
		if arg == flagInput || strings.HasPrefix(arg, flagInput+"=") {
			return "Refused: pass the merge head directly with -f sha=<full SHA>, without --input."
		}
		value := ""
		if (arg == "-f" || arg == "-F" || arg == flagField || arg == flagRawField) && i+1 < len(args) {
			value = args[i+1]
		}
		for _, prefix := range []string{flagField + "=", flagRawField + "=", "-f", "-F"} {
			if v, ok := strings.CutPrefix(arg, prefix); ok && v != "" {
				value = v
			}
		}
		if v, ok := strings.CutPrefix(value, "sha="); ok {
			if head != "" {
				return "Refused: pass exactly one merge head."
			}
			head = v
		}
	}
	if ghAPIMethod(args) != "PUT" {
		return fmt.Sprintf("Refused: unsupported merge method %s; use gh pr merge.", ghAPIMethod(args))
	}
	return h.mergeGo(m[1], m[2], head)
}

// Scripts must expose the merge as a separate tool command. Otherwise the
// script could change its target after the hook checked its contents.
var looseDeskMerge = regexp.MustCompile(`\bgh` + sep + `(?:pr` + sep + `merge\b|api[^\n]*?/merge\b)`)

func (h Hook) hiddenDeskMerge(text, where string, kind textKind) string {
	if kind == textShell {
		// Use the same shell parser as direct commands, including leading API
		// flags. Only executable calls reach MergeGo; prose never does.
		scriptHook := h
		scriptHook.MergeGo = func(string, string, int, string) string {
			return "run the script's merge as its own command with --match-head-commit"
		}
		if why := scriptHook.pushRefusal(text, ""); why != "" {
			return why + " (in " + where + ")"
		}
		return ""
	} else if kind == textCode {
		found := false
		literals := stringLiterals(text)
		for _, match := range looseDeskMerge.FindAllStringIndex(text, -1) {
			for _, literal := range literals {
				if literal[0] <= match[0] && match[0] < literal[1] && literalCommand.MatchString(text[literal[0]:match[0]]) {
					found = true
				}
			}
		}
		if !found {
			return ""
		}
	} else if !looseDeskMerge.MatchString(text) {
		return ""
	}
	return "Refused: a GitHub merge in " + where + " runs outside the supervisor go check; run the merge as its own command with --match-head-commit."
}
