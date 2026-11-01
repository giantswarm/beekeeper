package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var mergeHead = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var mergeEndpoint = regexp.MustCompile(`^(?:https://api\.github\.com/)?/?repos/([\w.-]+/[\w.-]+)/pulls/([0-9]+)/merge$`)
var mergePullURL = regexp.MustCompile(`^https://github\.com/([\w.-]+/[\w.-]+)/pull/([0-9]+)$`)

// mergeMutation is a GraphQL mutation that merges or arranges a merge.
var mergeMutation = regexp.MustCompile(`\b(?:mergePullRequest|enablePullRequestAutoMerge|enqueuePullRequest|mergeBranch)\b`)

// gatedMerge refuses any merge of a repository whose merges belong to the
// gate: its lane, protection check and release wait run only through
// devctl pr merge, whatever go a raw merge carries.
func (h Hook) gatedMerge(repo, number string) string {
	owner, _, _ := strings.Cut(repo, "/")
	if !slices.ContainsFunc(h.GatedOwners, func(o string) bool { return strings.EqualFold(o, owner) }) {
		return ""
	}
	if number == "" {
		number = "<n>"
	}
	return fmt.Sprintf("Refused: %s's merges belong to the gate (its lane, protection check and release wait); a raw merge bypasses it. Run `devctl pr merge %s %s --update-branch` on the loop's go; the hook puts the gate in front of it.", repo, repo, number)
}

// gatedHint names the gated command for a merge whose repository the guard
// cannot read (GraphQL, a script).
func (h Hook) gatedHint() string {
	if len(h.GatedOwners) == 0 {
		return ""
	}
	return " On a repository of " + strings.Join(h.GatedOwners, ", ") + " the merge is `devctl pr merge <owner/repo> <n> --update-branch` through the gate."
}

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
		if why := h.gatedMerge(repo, number); why != "" {
			return why
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
		if mergeMutation.MatchString(strings.Join(args, " ") + ghAPIBodies(words, cwd)) {
			return "Refused: use gh pr merge with --match-head-commit so the supervisor go can be checked by repository, pull request and head." + h.gatedHint()
		}
		return ""
	}
	if !writeMethods[ghAPIMethod(args)] {
		return ""
	}
	m := mergeEndpoint.FindStringSubmatch(endpoint)
	if m == nil {
		if strings.Contains(endpoint, "/merge") {
			return "Refused: the guard cannot resolve this merge endpoint; use gh pr merge with --repo and --match-head-commit." + h.gatedHint()
		}
		return ""
	}
	if why := h.gatedMerge(m[1], m[2]); why != "" {
		return why
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

// apiDeskMerge is a merge through GitHub's API from a program's string
// literal: the REST merge endpoint or a GraphQL merge mutation.
var apiDeskMerge = regexp.MustCompile(`(?i:api\.github\.com)/repos/[^\s"'/]+/[^\s"'/]+/pulls/[^\s"'/]+/merge\b|\b(?:mergePullRequest|enablePullRequestAutoMerge|enqueuePullRequest|mergeBranch)\b`)

// splitMergePath is a literal that ends a URL built in pieces on the merge
// endpoint ("/merge"), in a program that names GitHub's API.
var splitMergePath = regexp.MustCompile(`/merge$`)

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
		for _, match := range apiDeskMerge.FindAllStringIndex(text, -1) {
			for _, literal := range literals {
				if literal[0] <= match[0] && match[0] < literal[1] {
					found = true
				}
			}
		}
		if strings.Contains(strings.ToLower(text), "api.github.com") {
			for _, literal := range literals {
				if splitMergePath.MatchString(text[literal[0]:literal[1]]) {
					found = true
				}
			}
		}
		if !found {
			return ""
		}
	} else if !looseDeskMerge.MatchString(text) && !apiDeskMerge.MatchString(text) {
		return ""
	}
	return "Refused: a GitHub merge in " + where + " runs outside the supervisor go check; run the merge as its own command with --match-head-commit." + h.gatedHint()
}

// httpMerge refuses a pull request or branch merge, or a GraphQL merge
// mutation, sent to GitHub's API by an HTTP client (curl, wget, xh, httpie):
// the supervisor go is checked on gh pr merge and gh api alone, and a gated
// repository's merge is the gate's. It fails closed: only a request the guard
// reads as a plain GET or HEAD of a merge endpoint passes.
func (h Hook) httpMerge(client string, args []string) string {
	args = stripRedirects(args)
	read := h.httpRead(client, args)
	text := strings.Join(args, " ")
	for _, a := range args {
		target := strings.TrimPrefix(strings.TrimPrefix(a, "--url="), "--url ")
		lower := strings.ToLower(target)
		if !strings.Contains(lower, "api.github.com") {
			continue
		}
		path, _, _ := strings.Cut(target, "?")
		if decoded, err := url.PathUnescape(path); err == nil {
			path = decoded
		}
		lowerPath := strings.ToLower(path)
		if strings.Contains(lowerPath, "/graphql") {
			if mergeMutation.MatchString(text) || !read && strings.ContainsAny(text, "@") {
				return "Refused: a GraphQL merge through " + client + " runs outside the supervisor go check; use gh pr merge with --match-head-commit." + h.gatedHint()
			}
			continue
		}
		globbed := client == executionCurl && strings.ContainsAny(path, "{}[]") && !slices.Contains(args, "-g") && !slices.Contains(args, "--globoff")
		if !strings.Contains(lowerPath, "/merge") && !globbed {
			continue
		}
		if read && !globbed {
			continue
		}
		if m := mergeEndpoint.FindStringSubmatch(path); m != nil {
			if why := h.gatedMerge(m[1], m[2]); why != "" {
				return why
			}
		} else if m := githubAPI.FindStringSubmatch(path); m != nil && m[3] == "" {
			if why := h.gatedMerge(m[1]+"/"+m[2], ""); why != "" {
				return why
			}
		}
		return "Refused: a merge through " + client + " runs outside the supervisor go check; use gh pr merge with --repo and --match-head-commit." + h.gatedHint()
	}
	return ""
}

// httpRead says whether an HTTP client's call is surely a GET or HEAD with
// no body; anything the guard cannot read is not.
func (h Hook) httpRead(client string, args []string) bool {
	switch client {
	case executionCurl:
		method := ""
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--request" && i+1 < len(args):
				method = args[i+1]
				i++
			case strings.HasPrefix(a, "--request="):
				method = strings.TrimPrefix(a, "--request=")
			case curlData[strings.SplitN(a, "=", 2)[0]] || a == "--form" || a == executionUploadFile || a == "--json" || strings.HasPrefix(a, "--json="):
				return false
			case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 1:
				// A short-option cluster (-sX PUT, -sXPUT, -sd body).
				for j := 1; j < len(a); j++ {
					switch a[j] {
					case 'd', 'F', 'T':
						return false
					case 'X':
						if j+1 < len(a) {
							method = a[j+1:]
						} else if i+1 < len(args) {
							method = args[i+1]
							i++
						}
						j = len(a)
					}
				}
			}
		}
		method = strings.ToUpper(method)
		return method == "" || method == "GET" || method == "HEAD"
	case executionWget:
		for _, a := range args {
			if strings.HasPrefix(a, "--method") || strings.HasPrefix(a, "--post") || strings.HasPrefix(a, "--body") {
				return false
			}
		}
		return true
	}
	// xh and httpie: a write method as a word of its own, or a key=value or
	// key:=value item, which sends a body.
	for _, a := range args {
		if strings.HasPrefix(a, "-") || strings.Contains(strings.ToLower(a), "api.github.com") {
			continue
		}
		if writeMethods[strings.ToUpper(a)] || strings.Contains(a, "=") {
			return false
		}
	}
	return true
}
