package cmd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// modeDontAsk refuses every call no allow rule covers, without a prompt.
const modeDontAsk = "dontAsk"

// browseWait bounds a browse turn by default: a navigate that waits longer
// waits on something only a person answers (a sign-in page, a dialog).
const browseWait = 8 * time.Minute

// browsePreamble opens a browse turn's prompt.
const browsePreamble = "beekeeper browse: you run browser steps for another Claude session, with the Claude in Chrome tools only. " +
	"Never type a credential: sign in only through the person's existing session (an SSO or identity provider button). " +
	"Take a screenshot of each page the steps name as their proof. End with a short plain report of what you saw and did, " +
	"or of what stopped you. The steps:\n\n"

func (a *app) browseCmd() *cobra.Command {
	var model, dir string
	var wait time.Duration
	c := &cobra.Command{
		Use:   "browse <steps>",
		Short: "Run browser steps in a headless turn that never waits on a site approval",
		Long: `browse runs <steps> in a headless Claude Code turn in --dir that has the
CLI's own Claude in Chrome tools and nothing else: "claude -p --chrome
--tools '' --strict-mcp-config --permission-mode dontAsk --allowedTools
'mcp__claude-in-chrome__*'", no shell, no file tools, no other MCP server,
and every call outside the Chrome tools refused rather than asked. The
CLI's Chrome connection takes the site permissions of the Chrome extension,
so a navigate to a site no session was allowed on never waits on a person's
site approval.

It exists for the desktop turns of the sessions beekeeper starts: Claude
Desktop holds such a session's navigate to a new site for a person's site
approval, which neither bypassPermissions nor a hook answers (beekeeper
hook pretooluse refuses those calls and names this command). The turn's
report is printed, then its transcript and each screenshot it took, saved
under <stateDir>/browse/<id>/ as an image file the caller reads.

The turn shares the person's Chrome: hold the browser lease while it runs.
A turn that does not end within --timeout is stopped and the command fails:
a page that waits on a person (a sign-in form, a dialog) is the usual cause.

A consent page (an OAuth grant: GitHub's green Authorize button, an identity
provider's Allow) is a permission grant to the turn's classifier, which
refuses the click, or the whole task at its first call, unless the steps say
whose grant it is. Phrase a consent click the person asked for with all
three: whose App it is (the person's own, by name), that the person asked
for this one click, and which App and callback host the consent page must
name, so a page for any other App or callback is not clicked. For example:

  beekeeper browse "The GitHub App <app> is the person's own; they asked
  for this one click. Open <sign-in URL>. On GitHub's page 'Authorize
  <app>', whose callback is https://<callback host>/..., click Authorize
  once; click nothing else that grants access. Report the App name and the
  callback the page showed, and the final URL."

Every other grant stays refused. A call the classifier refused is named
below the report, one "refused:" line per call, with the call's own words
and the classifier's reason; a [Permission Grant] line quotes the step that
reads as a grant and points here.

A page in a Chrome window the compositor does not draw (occluded, or on
another workspace) is not rendered: GitHub keeps its Authorize button
disabled there and every screenshot times out ("the renderer may be
frozen"). Those timeouts are reported as "not rendered:" below the report;
bring the window to the front and run the steps again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			steps := strings.TrimSpace(args[0])
			if steps == "" {
				return usageErr("browse needs the steps to run")
			}
			return a.browse(cmd.Context(), steps, dir, model, wait)
		},
	}
	c.Flags().StringVar(&model, "model", "", "the turn's model (default: Claude Code's)")
	c.Flags().StringVar(&dir, "dir", ".", "the turn's working directory")
	c.Flags().DurationVar(&wait, "timeout", browseWait, "stop the turn and fail after this long")
	return c
}

func (a *app) browse(ctx context.Context, steps, dir, model string, wait time.Duration) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	id := uuid.NewString()
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c := exec.CommandContext(ctx, bin, browseArgv(id, model, browsePreamble+steps)...) //nolint:gosec // the claude CLI on PATH
	c.Dir = dir
	c.Env = browseEnv(os.Environ())
	c.Stderr = os.Stderr
	report, err := c.Output()
	if ctx.Err() != nil {
		err = fmt.Errorf("the browse turn did not end within %s and was stopped: a page waiting on a person (a sign-in form, a dialog) is the usual cause", wait)
	}
	if _, werr := fmt.Fprintln(a.out, strings.TrimSpace(string(report))); werr != nil {
		return werr
	}
	path := transcriptOf(a.cfg, id)
	if path == "" {
		return errors.Join(err, fmt.Errorf("browse turn %s left no transcript", id))
	}
	found, ferr := readFindings(path)
	shots, serr := saveScreenshots(path, filepath.Join(a.cfg.StateDir, "browse", id))
	lines := append(found.lines(steps), "transcript: "+path)
	for _, s := range shots {
		lines = append(lines, "screenshot: "+s)
	}
	if len(shots) == 0 {
		lines = append(lines, "screenshot: none taken")
	}
	if _, werr := fmt.Fprintln(a.out, strings.Join(lines, "\n")); werr != nil {
		return werr
	}
	return errors.Join(err, ferr, serr)
}

// The turn's classifier names a call it refused in the call's error result:
// classifierDenial opens it, classifierReason carries its reason, and
// reasonGrant is the reason for a consent click.
const (
	classifierDenial = "denied by the Claude Code auto mode classifier"
	reasonGrant      = "Permission Grant"
)

var classifierReason = regexp.MustCompile(`Reason: \[([^\]]*)\]`)

// frozenRenderer is in the error of a screenshot of a tab whose window the
// compositor does not draw: the capture times out.
const frozenRenderer = "renderer may be frozen"

// chromeToolPrefix is the Claude in Chrome tools' common prefix, dropped from
// a call's words.
const chromeToolPrefix = "mcp__claude-in-chrome__"

// consentWords are the words of a step that reads as a grant to the
// classifier.
var consentWords = regexp.MustCompile(`(?i)\b(authori[sz]e|consent|grant|approve|allow)\b`)

// consentHint is how a consent click the person asked for is phrased, the
// help text's long form.
const consentHint = "a consent click the person asked for is phrased as `beekeeper browse --help` says: whose App it is, " +
	"that the person asked for this one click, which App and callback host the page must name; every other grant stays refused"

// A refusal is one call of the turn the classifier denied: the call's own
// words and the classifier's reason.
type refusal struct {
	Call   string
	Reason string
}

// browseFindings are what a turn's transcript says beyond its report: the
// calls the classifier refused and the screenshots of a page that is not
// rendered.
type browseFindings struct {
	Refused     []refusal
	NotRendered int
}

// readFindings reads the findings of the transcript: a tool result that
// carries the classifier's denial names the call it answers, one whose
// screenshot timed out on a frozen renderer counts as not rendered.
func readFindings(transcript string) (browseFindings, error) {
	var f browseFindings
	calls := map[string]string{}
	err := scanTranscript(transcript, func(line []byte) {
		for _, b := range contentBlocks(line) {
			switch b.Type {
			case "tool_use":
				calls[b.ID] = callWords(b.Name, b.Input)
			case "tool_result":
				if !b.IsError {
					continue
				}
				text := resultText(b.Content)
				if strings.Contains(text, classifierDenial) {
					reason := ""
					if m := classifierReason.FindStringSubmatch(text); m != nil {
						reason = m[1]
					}
					f.Refused = append(f.Refused, refusal{Call: calls[b.ToolUseID], Reason: reason})
				}
				if strings.Contains(text, frozenRenderer) {
					f.NotRendered++
				}
			}
		}
	})
	return f, err
}

// lines are the findings as the lines printed below the report: one
// "refused:" line per refused call, the consent hint once when a refusal
// is a [Permission Grant], quoting the step that reads as a grant, and one
// "not rendered:" line for the screenshots of an undrawn window.
func (f browseFindings) lines(steps string) []string {
	var out []string
	grant := false
	for _, r := range f.Refused {
		out = append(out, fmt.Sprintf("refused: %s: the auto mode classifier denied it as [%s]", r.Call, r.Reason))
		grant = grant || r.Reason == reasonGrant
	}
	if grant {
		hint := consentHint
		if p := consentPhrase(steps); p != "" {
			hint = fmt.Sprintf("the steps' %q reads as a grant: %s", p, hint)
		}
		out = append(out, hint)
	}
	if n := f.NotRendered; n > 0 {
		s := "s"
		if n == 1 {
			s = ""
		}
		out = append(out, fmt.Sprintf("not rendered: %d screenshot%s timed out on a frozen renderer: the page's Chrome window is not drawn "+
			"(occluded, or on another workspace), where a consent page keeps its Authorize button disabled; "+
			"bring the window to the front and run the steps again", n, s))
	}
	return out
}

// phraseRunes bounds a quoted step.
const phraseRunes = 160

// consentPhrase is the first step of steps that reads as a grant, bounded
// to phraseRunes, "" when none does.
func consentPhrase(steps string) string {
	for _, s := range strings.FieldsFunc(steps, func(r rune) bool { return r == '.' || r == ';' || r == '\n' }) {
		if s = strings.TrimSpace(s); consentWords.MatchString(s) {
			if r := []rune(s); len(r) > phraseRunes {
				return string(r[:phraseRunes]) + "…"
			}
			return s
		}
	}
	return ""
}

// callWords are a Chrome call's own words: the tool, its action, its URL
// and the summary the model wrote for it, a batch's actions after it.
func callWords(name string, input json.RawMessage) string {
	var in struct {
		Action  string `json:"action"`
		Summary string `json:"action_summary"`
		URL     string `json:"url"`
		Actions []struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"actions"`
	}
	_ = json.Unmarshal(input, &in)
	words := []string{strings.TrimPrefix(name, chromeToolPrefix)}
	if in.Action != "" {
		words = append(words, in.Action)
	}
	if in.URL != "" {
		words = append(words, in.URL)
	}
	if in.Summary != "" {
		words = append(words, strconv.Quote(in.Summary))
	}
	var actions []string
	for _, a := range in.Actions {
		actions = append(actions, callWords(a.Name, a.Input))
	}
	if len(actions) > 0 {
		words = append(words, strings.Join(actions, ", "))
	}
	return strings.Join(words, " ")
}

// resultText is the text of a tool result's content: a string, or the text
// blocks of an array.
func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		texts = append(texts, b.Text)
	}
	return strings.Join(texts, "\n")
}

// contentBlock is one block of a transcript line's message content: a
// tool_use with its id, name and input, a tool_result with the id of the
// call it answers, its content and whether it is an error.
type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// contentBlocks are the blocks of one transcript line's message content,
// none for a line that is not a message or whose content is plain text.
func contentBlocks(line []byte) []contentBlock {
	var entry struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return nil
	}
	var blocks []contentBlock
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return nil
	}
	return blocks
}

// scanTranscript calls fn with each line of the transcript.
func scanTranscript(transcript string, fn func(line []byte)) error {
	f, err := os.Open(transcript) //nolint:gosec // the browse turn's own transcript
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

// browseTools are the only tools a browse turn has: the CLI's own Claude in
// Chrome tools.
const browseTools = "mcp__claude-in-chrome__*"

// The CLI flags that narrow a turn's tools: toolsFlag the built-in tools,
// strictMCPConfigFlag the MCP servers to those named, allowedToolsFlag the
// calls allowed without asking.
const (
	toolsFlag           = "--tools"
	strictMCPConfigFlag = "--strict-mcp-config"
	allowedToolsFlag    = "--allowedTools"
)

// browseArgv is a browse turn's command line after the binary: one headless
// turn under id with the CLI's own Chrome connection and nothing else: no
// built-in tool (no shell, no file tools), no MCP server but Chrome's, and in
// dontAsk mode every call the Chrome tools' allow rule does not cover is
// refused rather than asked.
func browseArgv(id, model, prompt string) []string {
	argv := []string{"-p", chromeFlag, toolsFlag, "", strictMCPConfigFlag, permissionModeFlag, modeDontAsk,
		allowedToolsFlag, browseTools, sessionIDFlag, id}
	if model != "" {
		argv = append(argv, modelFlag, model)
	}
	return append(argv, "--", prompt)
}

// browseEnv is env without the variables a Claude Code CLI gives the
// commands of its session: a browse turn is a session of its own, not a
// child of the desktop session that runs the command.
func browseEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || k == "CLAUDE_PID" || k == "CLAUDE_EFFORT" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// saveScreenshots writes every image a tool result in the transcript carries
// into dir as shot-<n>.<ext>, in order, and returns their paths.
func saveScreenshots(transcript, dir string) ([]string, error) {
	var out []string
	var werr error
	err := scanTranscript(transcript, func(line []byte) {
		for _, img := range resultImages(line) {
			if werr != nil {
				return
			}
			raw, err := base64.StdEncoding.DecodeString(img.Data)
			if err != nil {
				continue
			}
			if werr = os.MkdirAll(dir, 0o700); werr != nil {
				return
			}
			p := filepath.Join(dir, fmt.Sprintf("shot-%d.%s", len(out)+1, imageExt(img.MediaType)))
			if werr = os.WriteFile(p, raw, 0o600); werr != nil {
				return
			}
			out = append(out, p)
		}
	})
	return out, errors.Join(err, werr)
}

// imageSource is an inline base64 image of a tool result.
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// resultImages are the inline images of the tool results on one transcript
// line.
func resultImages(line []byte) []imageSource {
	var out []imageSource
	for _, b := range contentBlocks(line) {
		if b.Type != "tool_result" {
			continue
		}
		var inner []struct {
			Type   string      `json:"type"`
			Source imageSource `json:"source"`
		}
		if json.Unmarshal(b.Content, &inner) != nil {
			continue
		}
		for _, c := range inner {
			if c.Type == "image" && c.Source.Type == "base64" && c.Source.Data != "" {
				out = append(out, c.Source)
			}
		}
	}
	return out
}

// imageExt is the file extension of an image's media type.
func imageExt(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	}
	return "png"
}
