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
a page that waits on a person (a sign-in form, a dialog) is the usual cause.`,
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
	shots, serr := saveScreenshots(path, filepath.Join(a.cfg.StateDir, "browse", id))
	lines := []string{"transcript: " + path}
	for _, s := range shots {
		lines = append(lines, "screenshot: "+s)
	}
	if len(shots) == 0 {
		lines = append(lines, "screenshot: none taken")
	}
	if _, werr := fmt.Fprintln(a.out, strings.Join(lines, "\n")); werr != nil {
		return werr
	}
	return errors.Join(err, serr)
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
	f, err := os.Open(transcript) //nolint:gosec // the browse turn's own transcript
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		for _, img := range resultImages(sc.Bytes()) {
			raw, err := base64.StdEncoding.DecodeString(img.Data)
			if err != nil {
				continue
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return out, err
			}
			p := filepath.Join(dir, fmt.Sprintf("shot-%d.%s", len(out)+1, imageExt(img.MediaType)))
			if err := os.WriteFile(p, raw, 0o600); err != nil {
				return out, err
			}
			out = append(out, p)
		}
	}
	return out, sc.Err()
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
	var entry struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return nil
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return nil
	}
	var out []imageSource
	for _, b := range blocks {
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
