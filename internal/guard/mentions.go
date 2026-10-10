package guard

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// The mention guard refuses an agent's GitHub post that @-mentions someone:
// a comment, review, issue or pull request text an agent writes goes out
// under a person's account, and a mention there pings a colleague in that
// person's name. The text names who is meant without the @.

// mentionTools are the connector (MCP) tools, by the end of their names,
// whose input is posted to GitHub.
var mentionTools = []string{
	"add_issue_comment", "add_comment_to_pending_review", "add_reply_to_pull_request_comment",
	"pull_request_review_write", "create_pull_request", "update_pull_request", "issue_write",
	"update_issue_comment",
}

// ghPosts are the gh subcommands that post text: issue and pr comment,
// create, edit and review, and api (a comments or reviews endpoint).
var ghPosts = map[string]bool{"comment": true, "create": true, verbEdit: true, "review": true, "close": true}

// mentionHead is what may stand before an @-mention: the start, a space,
// an opening bracket or punctuation; not a word, path, address or option
// character (user@host, go install x@v1, body=@file, `@code`).
var mention = regexp.MustCompile(`(?:^|[\s(\[{>,;:!?"'*_~])@([A-Za-z0-9][A-Za-z0-9-]{0,38})`)

// mentions returns the GitHub handles text @-mentions, a scoped package
// (@types/node) and a domain (@example.com) left out.
func mentions(text string) []string {
	var out []string
	for _, m := range mention.FindAllStringSubmatchIndex(text, -1) {
		rest := text[m[1]:]
		if strings.HasPrefix(rest, "/") || len(rest) > 1 && rest[0] == '.' && isWord(rest[1]) {
			continue
		}
		out = append(out, text[m[2]:m[3]])
	}
	return out
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// mentionRefusal says why a tool call that posts to GitHub is refused for
// an @-mention; "" passes it.
func mentionRefusal(tool string, input map[string]any, cwd string) string {
	var text string
	switch {
	case tool == bashTool:
		cmd, _ := input[commandKey].(string)
		text = ghPostText(cmd, cwd)
	case strings.HasPrefix(tool, mcpPrefix) && postsToGitHub(tool):
		text = strings.Join(stringValues(input), "\n")
	}
	ms := mentions(text)
	if len(ms) == 0 {
		return ""
	}
	return "Refused: the text this call posts to GitHub @-mentions " + strings.Join(dedupe(ms), ", ") +
		". An agent's comment, review, issue or pull request text goes out under a person's account and @-mentions no one: " +
		"name the person without the @ (\"marians\", not \"@marians\"), and ask the supervisor when someone has to be pinged."
}

func postsToGitHub(tool string) bool {
	for _, t := range mentionTools {
		if strings.HasSuffix(tool, "__"+t) {
			return true
		}
	}
	return false
}

// ghPostText is the text a command line's gh posts would send: the whole
// command line and the files their --body-file, -F and @file arguments
// name; "" when no segment posts.
func ghPostText(cmd, cwd string) string {
	sc := scanShell(cmd)
	posts := false
	var files []string
	for _, sg := range sc.segments() {
		words := shellWords(sc.plain[sg.start:sg.end])
		if !ghPost(words) {
			continue
		}
		posts = true
		for i, w := range words {
			switch {
			case (w == "--body-file" || w == "-F") && i+1 < len(words):
				files = append(files, words[i+1])
			case strings.HasPrefix(w, "--body-file="):
				files = append(files, strings.TrimPrefix(w, "--body-file="))
			}
		}
	}
	if !posts {
		return ""
	}
	parts := []string{cmd}
	for _, f := range files {
		if _, v, ok := strings.Cut(f, "=@"); ok {
			f = v
		}
		if f == "" || f == "-" {
			continue
		}
		if !filepath.IsAbs(f) {
			f = filepath.Join(cwd, expandHome(f))
		}
		if b, err := os.ReadFile(expandHome(f)); err == nil && len(b) < 1<<20 {
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n")
}

// ghPost says whether words are a gh call that posts text.
func ghPost(words []string) bool {
	if len(words) < 3 || path.Base(words[0]) != "gh" {
		return false
	}
	switch words[1] {
	case "issue", "pr":
		return ghPosts[words[2]]
	case ghAPISub:
		for _, w := range words[2:] {
			if strings.Contains(w, "/comments") || strings.Contains(w, "/reviews") ||
				strings.HasSuffix(w, "/issues") || strings.HasSuffix(w, "/pulls") {
				return true
			}
		}
	}
	return false
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if k := strings.ToLower(x); !seen[k] {
			seen[k] = true
			out = append(out, x)
		}
	}
	return out
}
