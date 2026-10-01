package guard

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The guide asks its person and relays the answers; it never works itself.
// For the guide role's holder the hook refuses the tool calls that do work
// (an edit, a commit or push, a merge, a review, a click in the browser)
// and checks its questions as `note add` checks a note for the person.

// editTools change files.
var editTools = []string{"Edit", "Write", "NotebookEdit"}

// guideWork is a command that does work: a git commit or push, a devctl
// merge or promotion, a gh merge or review.
var guideWork = regexp.MustCompile(`(?m)` + mergePos + `(` +
	`git(?:\s+(?:-[Cc]\s*\S+|--[\w-]+(?:=\S+)?))*\s+(?:commit|push)\b` +
	`|(?:(?:~|\.\.?|\$\{?\w+\}?)?/(?:[^\s;&|()'"<>=]*/)?)?devctl\s+(?:pr\s+merge|release\s+promote)\b` +
	`|gh\s+pr\s+(?:merge|review)\b)`)

// browserTools is the person's Chrome (Claude in Chrome).
const browserTools = mcpPrefix + "claude-in-chrome__"

// browserReads are the browser tools that only read: open a page or tab,
// read it, look at it.
var browserReads = []string{
	"tabs_context", "tabs_context_mcp", "tabs_create", "tabs_create_mcp", "navigate",
	"read_page", "get_page_text", "find", "read_console_messages", "read_network_requests", "shortcuts_list",
}

// actionKey is the input of the browser's computer tool that names its
// action.
const actionKey = "action"

// computerReads are the actions of the browser's computer tool that only
// look: a screenshot, a scroll, a zoom, a wait, a hover.
var computerReads = []string{"screenshot", "scroll", "scroll_to", "zoom", "wait", "hover"}

// connectorWork are the GitHub connector tools that merge, review or push.
var connectorWork = []string{
	"merge_pull_request", "update_pull_request_branch", "push_files", "create_or_update_file", "delete_file",
	"pull_request_review_write", "create_pending_pull_request_review", "create_and_submit_pull_request_review",
	"submit_pending_pull_request_review", "add_comment_to_pending_review", "add_pull_request_review_comment_to_pending_review",
}

// guideRefusal returns why the hook refuses the guide's call, "" when it
// passes: a call that does work, in the guide's session. The guide lookup
// runs only for such a call, so every other call costs nothing.
func (h Hook) guideRefusal(ev event) string {
	what := work(ev)
	if what == "" || h.Guide == nil {
		return ""
	}
	if guide, _ := h.Guide(ev.Session); !guide {
		return ""
	}
	return "Refused: the guide asks and relays, it never works itself, and " + what + " is work. " +
		"Hand it to the supervisor in one line (SendMessage to the session `beekeeper supervisor status` names): " +
		"what to do, on which issue or PR (its full URL), and the person's words that asked for it. Then carry on guiding."
}

// work names the work ev does, "" for a call that asks, relays or reads.
func work(ev event) string {
	switch name := ev.ToolName; {
	case slices.Contains(editTools, name):
		return "`" + name + "`"
	case name == bashTool:
		cmd, _ := ev.ToolInput[commandKey].(string)
		if m := commandWork(cmd); m != "" {
			return "`" + short(m) + "`"
		}
	case strings.HasPrefix(name, browserTools):
		tool := strings.TrimPrefix(name, browserTools)
		action, _ := ev.ToolInput[actionKey].(string)
		if slices.Contains(browserReads, tool) || tool == "computer" && slices.Contains(computerReads, action) {
			return ""
		}
		if action != "" {
			tool += " " + action
		}
		return "a browser action (" + tool + "; the guide only reads pages)"
	case strings.HasPrefix(name, mcpPrefix) && strings.Contains(strings.ToLower(name), "github"):
		if tool := name[strings.LastIndex(name, "__")+2:]; slices.Contains(connectorWork, tool) {
			return "the connector's " + tool
		}
	}
	return ""
}

// commandWork is the first command in cmd that does work, at a command
// position or inside a shell's -c string; "" when none does.
func commandWork(cmd string) string {
	if m := guideWork.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	for _, m := range shellC.FindAllStringSubmatchIndex(cmd, -1) {
		body := cmd[m[1]:]
		if w := commandWork(body[:closingQuote(body, cmd[m[2]])]); w != "" {
			return w
		}
	}
	return ""
}

// Question is one question of the guide's AskUserQuestion call, in the
// parts `note add` checks: the text up to its first label, the "Status
// quo:", "Why:" and "Checked:" labelled parts, and each option as
// "<label>: <description>", the description its consequence.
type Question struct {
	Text, StatusQuo, Why, Checked string
	Options                       []string
}

// askLabel opens a labelled part of a question.
var askLabel = regexp.MustCompile(`(?i)\b(status quo|why|checked)\s*:`)

// parseQuestion reads one entry of AskUserQuestion's questions.
func parseQuestion(raw any) Question {
	m, _ := raw.(map[string]any)
	text, _ := m["question"].(string)
	var q Question
	labels := askLabel.FindAllStringSubmatchIndex(text, -1)
	q.Text = text
	if len(labels) > 0 {
		q.Text = text[:labels[0][0]]
	}
	for i, l := range labels {
		end := len(text)
		if i+1 < len(labels) {
			end = labels[i+1][0]
		}
		v := strings.TrimSpace(text[l[1]:end])
		switch strings.ToLower(text[l[2]:l[3]]) {
		case "status quo":
			q.StatusQuo = v
		case "why":
			q.Why = v
		case "checked":
			q.Checked = v
		}
	}
	q.Text = strings.TrimSpace(q.Text)
	opts, _ := m["options"].([]any)
	for _, o := range opts {
		om, _ := o.(map[string]any)
		label, _ := om["label"].(string)
		desc, _ := om["description"].(string)
		q.Options = append(q.Options, label+": "+desc)
	}
	return q
}

// questionRefusal returns why the hook refuses the guide's AskUserQuestion
// call, "" when every question carries what the person needs to answer it
// without asking back; nil CheckQuestion checks nothing.
func (h Hook) questionRefusal(input map[string]any, person string) string {
	if h.CheckQuestion == nil {
		return ""
	}
	qs, _ := input["questions"].([]any)
	var lacks []string
	for i, raw := range qs {
		if m := h.CheckQuestion(parseQuestion(raw)); len(m) > 0 {
			lacks = append(lacks, fmt.Sprintf("question %d lacks: %s", i+1, strings.Join(m, "; ")))
		}
	}
	if len(lacks) == 0 {
		return ""
	}
	if person == "" {
		person = "the person"
	}
	return "Refused: a question for " + person + " carries what they need to answer it without asking back, as `beekeeper note add` checks a note:\n" +
		strings.Join(lacks, "\n") + "\n" +
		"In a question, --status-quo is a \"Status quo: …\" part of its text, --why a \"Why: …\" part, --checked a \"Checked: …\" part; " +
		"each option's description is its consequence; every issue or PR is named by its full URL."
}
