package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/secret"
)

// pageOp is the secret subcommand that captures a value a web page shows
// once.
const pageOp = "page"

// pageCapture is one value a page makes: the steps that make it, the click
// that creates it (the turn's declared create), and where it shows up: a
// match of pattern on the page, or a download glob matches.
type pageCapture struct {
	steps, create, match, download string
}

// The presets' patterns: a GitHub App's client secret (40 hex digits) and
// an Anthropic API key (the console shows the whole key once; its list
// shows it shortened, which no match takes).
const (
	githubAppMatch    = `\b[0-9a-f]{40}\b`
	anthropicKeyMatch = `sk-ant-[a-z0-9]+-[A-Za-z0-9_-]{60,}`
)

// anthropicKeys is the Anthropic console's API keys page.
const anthropicKeys = "https://console.anthropic.com/settings/keys"

// readerStep is the steps' last word for a value the page shows: the reader
// takes it, the dialog closes.
const readerStep = " Once the page shows the new value, run beekeeper's reader script once (javascript_tool), then close the page's dialog or notice if it has one."

func (a *app) secretPageCmd() *cobra.Command {
	var clientSecret, privateKey, anthropicKey, workspace, create, match, download string
	var wait time.Duration
	c := &cobra.Command{
		Use: "page op://<vault>/<item>/<field> --github-client-secret <org>/<app> | --github-private-key <org>/<app> | " +
			"--anthropic-key <name> [--workspace <name>] | --create \"<what, where>\" (--match <regex> | --download <glob>) -- <steps>",
		Short: "Create a value a web page shows once (a client secret, an API key, a private key) in the person's browser and store it into the vault, seen by no session",
		Long: `page captures a credential a web page shows once, or downloads once, into a
field of the shared vault without any session seeing it: a GitHub App's
client secret or private key, an API key a provider's console shows on
creation. An agent runs it while it holds the browser lease.

It runs the steps in a capture turn, a beekeeper browse turn (its tools
and rules, beekeeper browse --help) made blind: its hook passes an allow
list of Chrome calls alone, so the turn has no screenshot, zoom, page
text, page read, traffic read or batch, no drag, double or triple click, no key
but a plain one pressed alone (no chord, no function key) and no
navigate but to an http or https page; it finds its controls with find
and clicks them by ref; the one script it runs is beekeeper's reader,
which answers the matches of --match on the page. The
turn's own hook, a beekeeper process, takes the one match from the reader's
result and hands it over a private socket to this process; every result
reaches the model with images dropped and every match and every run of 16
or more token characters masked. The declared create (--create, or the
preset's) is the one creating click the turn's auto mode allows.

A value the page downloads (--download <glob>) is the newest file in
~/Downloads the glob matches that arrived during the turn; it is shredded
once the vault holds it.

The value goes into the field through the vault keeper's session
(secret.session) or the service account, as beekeeper secret store writes
one, the item created when absent. Afterwards the turn's transcript and
report carry the value nowhere: each place it appears is replaced by a mask
before the report is printed, and so is every match of --match, a stored
value or not. page answers the field, the length and the
keyed fingerprint; beekeeper log records them with the source.

Presets name the page and the value:

  --github-client-secret <org>/<app>  "Generate a new client secret" on
                                      the App's settings page
  --github-private-key <org>/<app>    "Generate a private key", the
                                      download <app>.*.private-key.pem
  --anthropic-key <name>              "Create Key" on the Anthropic
                                      console, the key named <name>, in
                                      --workspace when given

Every check runs before the turn: the field writable, a vault session held
(exit 78 otherwise), the pattern or glob given. A turn that ends with no
value, or with several different matches, stores nothing and says the page
may hold a new value no vault records.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash >= 0 && dash != 1 {
				return errors.New("page op://<vault>/<item>/<field> [flags] [-- <steps>]")
			}
			return cobra.RangeArgs(1, 2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			if r[0].Op == "" {
				return usageErr("%s: page writes a field of the shared vault, op://<vault>/<item>/<field>", args[0])
			}
			steps := ""
			if len(args) == 2 {
				steps = strings.TrimSpace(args[1])
			}
			pc, err := pagePreset(clientSecret, privateKey, anthropicKey, workspace, pageCapture{steps: steps, create: create, match: match, download: download})
			if err != nil {
				return err
			}
			if err := a.vaultReady([]intake{{ref: r[0]}}); err != nil {
				return err
			}
			return a.secretPage(cmd.Context(), r[0], pc, wait)
		},
	}
	f := c.Flags()
	f.StringVar(&clientSecret, "github-client-secret", "", "a new client secret of the GitHub App <org>/<app>")
	f.StringVar(&privateKey, "github-private-key", "", "a new private key of the GitHub App <org>/<app>, the browser's download")
	f.StringVar(&anthropicKey, "anthropic-key", "", "a new Anthropic API key of this name, created on the Anthropic console")
	f.StringVar(&workspace, "workspace", "", "the Anthropic console workspace the key belongs to (--anthropic-key)")
	f.StringVar(&create, "create", "", "the one creating click the steps make (\"<what, where>\"), allowed to the turn's auto mode")
	f.StringVar(&match, "match", "", "a regular expression the value, and nothing else on the page, matches")
	f.StringVar(&download, "download", "", "the value is the file the page downloads into ~/Downloads matching this glob")
	f.DurationVar(&wait, "timeout", browseWait, "stop the turn and fail after this long")
	return c
}

// pagePreset is the capture a preset names, or the generic one checked.
func pagePreset(clientSecret, privateKey, anthropicKey, workspace string, generic pageCapture) (pageCapture, error) {
	set := 0
	for _, s := range []string{clientSecret, privateKey, anthropicKey} {
		if s != "" {
			set++
		}
	}
	custom := generic.steps != "" || generic.create != "" || generic.match != "" || generic.download != ""
	switch {
	case set > 1 || set == 1 && custom:
		return pageCapture{}, usageErr("one preset, or --create with --match or --download and the steps")
	case workspace != "" && anthropicKey == "":
		return pageCapture{}, usageErr("--workspace goes with --anthropic-key")
	case clientSecret != "" || privateKey != "":
		app := clientSecret + privateKey
		org, slug, ok := strings.Cut(app, "/")
		if !ok || org == "" || slug == "" || strings.Contains(slug, "/") {
			return pageCapture{}, usageErr("a GitHub App is named <org>/<app>, not %q", app)
		}
		page := "Open https://github.com/organizations/" + org + "/settings/apps/" + slug + "."
		if clientSecret != "" {
			return pageCapture{
				steps:  page + ` Under "Client secrets" click "Generate a new client secret" once.` + readerStep,
				create: "a new client secret of the GitHub App " + app, match: githubAppMatch,
			}, nil
		}
		return pageCapture{
			steps:  page + ` Under "Private keys" click "Generate a private key" once: the browser downloads the key.`,
			create: "a new private key of the GitHub App " + app, download: slug + ".*.private-key.pem",
		}, nil
	case anthropicKey != "":
		in := ""
		if workspace != "" {
			in = fmt.Sprintf(" choose the workspace %q,", workspace)
		}
		return pageCapture{
			steps: fmt.Sprintf("Open %s. Click \"Create Key\" once. In its dialog%s type %q into the key's name field and click the dialog's "+
				"create button (\"Add\" or \"Create Key\") once.", anthropicKeys, in, anthropicKey) + readerStep,
			create: fmt.Sprintf("a new API key named %q on the Anthropic console", anthropicKey), match: anthropicKeyMatch,
		}, nil
	}
	switch {
	case generic.steps == "" || generic.create == "":
		return pageCapture{}, usageErr("a capture without a preset names --create \"<what, where>\" and the steps after --")
	case (generic.match == "") == (generic.download == ""):
		return pageCapture{}, usageErr("the value is a match on the page (--match) or a download (--download), one of them")
	case generic.match != "":
		if _, err := regexp.Compile(generic.match); err != nil {
			return pageCapture{}, usageErr("--match: %v", err)
		}
		generic.steps += readerStep
	}
	if generic.download != "" {
		if _, err := filepath.Match(generic.download, ""); err != nil || strings.Contains(generic.download, "/") {
			return pageCapture{}, usageErr("--download takes a file name glob in ~/Downloads, not %q", generic.download)
		}
	}
	return generic, nil
}

// secretPage runs the capture turn, takes its value, stores it, scrubs it
// from the turn's transcript and report, and logs and answers what was
// written.
func (a *app) secretPage(ctx context.Context, ref secret.Ref, pc pageCapture, wait time.Duration) error {
	start := time.Now()
	blind := &blindTurn{match: pc.match}
	var taken *pageValues
	if pc.match != "" {
		var stop func()
		var err error
		if blind.socket, taken, stop, err = listenForValue(a.cfg.StateDir); err != nil {
			return err
		}
		defer stop()
	}
	// the turn runs in the state directory: no repository's instructions
	// reach it, and its transcript lands in a project of beekeeper's own
	terr := a.browse(ctx, pc.steps, a.cfg.StateDir, "", wait, pc.create, nil, blind)
	value, from, err := pc.take(taken, start)
	stored := secret.Stored{}
	if err == nil {
		if stored, err = a.storeValue(ctx, ref, value); err != nil {
			err = fmt.Errorf("the page made %s, which the vault did not take, and the value is gone: %w", pc.create, err)
		}
	} else {
		err = refused("%v; nothing stored: the page may now hold %s that no vault records", err, pc.create)
	}
	// every value the page showed is masked, the stored one or not: each
	// the hook delivered, each match of the pattern
	hide := taken.all()
	if value != "" {
		hide = append(hide, value)
	}
	scrubbed := scrubTranscript(transcriptOf(a.cfg, blind.id), pc.match, hide)
	if value != "" && err == nil && pc.download != "" {
		err = secret.Shred(from)
	}
	report := scrub(blind.out.String(), pc.match, hide)
	if _, werr := fmt.Fprint(a.out, report); werr != nil {
		return werr
	}
	done := fmt.Sprintf("%d bytes, %s; %d transcript places masked", stored.Bytes, stored.Fingerprint, scrubbed)
	if err := a.secretLog(err, pageOp, "%s from %s (%s): %s", ref, from, pc.create, outcome(err, done)); err != nil {
		return errors.Join(err, terr)
	}
	// the value is stored; the turn's own failure (a tab left open) still
	// fails the call
	if err := a.secretPrint(stored, fmt.Sprintf("wrote %s: %d bytes, %s (from %s; %d places in the turn's transcript masked)\n",
		stored.Ref, stored.Bytes, stored.Fingerprint, from, scrubbed)); err != nil {
		return err
	}
	return terr
}

// take is the value the turn made: the one match the hook delivered, or
// the download that arrived since start; and where it came from.
func (pc pageCapture) take(taken *pageValues, start time.Time) (string, string, error) {
	if pc.download == "" {
		v, err := taken.one()
		return v, "the page (reader)", err
	}
	dir, err := downloadsDir()
	if err != nil {
		return "", "", err
	}
	f, err := secret.NewestFile(dir, pc.download)
	if err != nil {
		return "", "", err
	}
	fi, serr := os.Stat(f)
	if f == "" || serr != nil || fi.ModTime().Before(start) {
		return "", "", fmt.Errorf("no %s arrived during the turn", filepath.Join(dir, pc.download))
	}
	v, err := secret.ReadValueFile(f)
	return v, f, err
}

// pageValues are the distinct values the capture turn's hook delivered.
type pageValues struct {
	mu     sync.Mutex
	values []string
}

// all are the values delivered; none on a capture without a pattern.
func (p *pageValues) all() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.values)
}

// one is the single value delivered, or why there is none.
func (p *pageValues) one() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch len(p.values) {
	case 0:
		return "", errors.New("the turn's reader found no value on the page")
	case 1:
		return p.values[0], nil
	}
	return "", fmt.Errorf("the turn's reader found %d different values on the page", len(p.values))
}

// listenForValue opens the socket the capture turn's hook delivers the
// value on, in a directory of its own only this user reaches (MkdirTemp's
// 0700) under the runtime directory, else under fallback. stop closes it
// and removes the directory.
func listenForValue(fallback string) (string, *pageValues, func(), error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = fallback
	}
	dir, err := os.MkdirTemp(base, "bk-page-")
	if err != nil {
		return "", nil, nil, err
	}
	path := filepath.Join(dir, "s")
	if len(path) > maxSocketPath {
		_ = os.RemoveAll(dir) //nolint:gosec // the directory MkdirTemp made
		return "", nil, nil, fmt.Errorf("the capture's socket %s is longer than a unix socket's %d bytes: set XDG_RUNTIME_DIR to a short directory", path, maxSocketPath)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir) //nolint:gosec // the directory MkdirTemp made
		return "", nil, nil, err
	}
	got := &pageValues{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				line, err := bufio.NewReader(c).ReadString('\n')
				var v string
				if err != nil || json.Unmarshal([]byte(line), &v) != nil || v == "" {
					_, _ = fmt.Fprintln(c, "no value")
					return
				}
				got.mu.Lock()
				if !slices.Contains(got.values, v) {
					got.values = append(got.values, v)
				}
				got.mu.Unlock()
				_, _ = fmt.Fprintln(c, "ok")
			}(c)
		}
	}()
	return path, got, func() { _ = l.Close(); _ = os.RemoveAll(dir) }, nil //nolint:gosec // the directory MkdirTemp made
}

// maxSocketPath is the longest unix socket path every platform binds
// (macOS's sun_path, 104 bytes with its terminator).
const maxSocketPath = 103

// deliverTimeout bounds the hook's delivery well inside its own timeout
// (captureHookTimeout): the masked answer never waits on it.
const deliverTimeout = 3 * time.Second

// deliverValue hands the value to the capture's socket: the capture turn's
// hook, a beekeeper process, calls it.
func deliverValue(socket, value string) error {
	c, err := net.DialTimeout("unix", socket, deliverTimeout)
	if err != nil {
		return fmt.Errorf("beekeeper secret page is not listening: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(deliverTimeout))
	b, _ := json.Marshal(value)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	ack, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(ack) != "ok" {
		return errors.New(strings.TrimSpace(ack))
	}
	return nil
}

// scrub is text with every match of the pattern and every form of each
// value (as written, and JSON-escaped as a transcript holds a multi-line
// one) replaced by the mask.
func scrub(text, match string, values []string) string {
	if match != "" {
		if re, err := regexp.Compile(match); err == nil {
			text = re.ReplaceAllString(text, guard.CaptureMask)
		}
	}
	for _, v := range values {
		for _, form := range valueForms(v) {
			text = strings.ReplaceAll(text, form, guard.CaptureMask)
		}
	}
	return text
}

// valueForms are the forms value takes in a transcript: as written, and
// JSON-escaped, each trimmed of surrounding whitespace; none for "".
func valueForms(value string) []string {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	forms := []string{v}
	b, _ := json.Marshal(v)
	if esc := string(b[1 : len(b)-1]); esc != v {
		forms = append(forms, esc)
	}
	return forms
}

// scrubTranscript masks every match of the pattern and every form of each
// value in the transcript and answers how many places it masked; 0 for a
// transcript it cannot read.
func scrubTranscript(path, match string, values []string) int {
	if path == "" {
		return 0
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the capture turn's own transcript
	if err != nil {
		return 0
	}
	text := string(raw)
	out := scrub(text, match, values)
	if out == text {
		return 0
	}
	n := strings.Count(out, guard.CaptureMask) - strings.Count(text, guard.CaptureMask)
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if err := os.WriteFile(path, []byte(out), fi.Mode().Perm()); err != nil { //nolint:gosec // the capture turn's own transcript
		return 0
	}
	return n
}

// blindTurn makes a browse turn a capture turn: the Chrome tools that
// read the page go, its own hook (beekeeper hook capturepage) refuses
// screenshots and every script but the reader and masks every result,
// delivering the reader's match on socket. out collects what browse would
// print; id is the turn's session.
type blindTurn struct {
	match, socket, id string
	out               strings.Builder
}

// shape is a browse turn's argv made blind: the read tools disallowed, the
// capture hook beside the turn's own settings.
func (b *blindTurn) shape(argv []string, self string) []string {
	out := make([]string, 0, len(argv)+2)
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case settingsFlag:
			out = append(out, settingsFlag, b.settings(argv[i+1], self))
			i++
			continue
		case "--":
			reads := make([]string, len(guard.BlindReads))
			for k, r := range guard.BlindReads {
				reads[k] = chromeToolPrefix + r
			}
			out = append(out, disallowedToolsFlag, strings.Join(reads, ","))
		}
		out = append(out, argv[i])
	}
	return out
}

// captureHookTimeout is the capture hook's timeout in seconds, far above
// its delivery's deliverTimeout: a hook Claude Code stops passes the
// result unmasked.
const captureHookTimeout = 30

// disallowedToolsFlag removes tools from a turn.
const disallowedToolsFlag = "--disallowedTools"

// settings are a browse turn's settings with the capture hook on every
// Chrome call before and after it.
func (b *blindTurn) settings(base, self string) string {
	var s map[string]any
	if json.Unmarshal([]byte(base), &s) != nil {
		s = map[string]any{}
	}
	hooks, _ := s[hooksKey].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	command := guard.ShellQuote(self) + " hook capturepage"
	if b.match != "" {
		command += " --match " + guard.ShellQuote(b.match) + " --socket " + guard.ShellQuote(b.socket)
	}
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	entry := []any{map[string]any{"matcher": chromeToolPrefix + ".*", hooksKey: []hook{{Type: hookTypeCommand, Command: command, Timeout: captureHookTimeout}}}}
	hooks["PreToolUse"], hooks["PostToolUse"] = entry, entry
	s[hooksKey] = hooks
	out, _ := json.Marshal(s)
	return string(out)
}

// captureRule is the auto mode allow rule of a capture turn: the person's
// own credential, created on the page the steps name and taken by beekeeper
// into the person's vault on this machine, is no exfiltration; beekeeper's
// hook keeps the value from the turn.
func captureRule(create string) string {
	return fmt.Sprintf("Covered Credential Capture: the person runs beekeeper secret page to create exactly one credential of their own, %s, "+
		"on the page the steps name, in their own signed-in browser session, and to store it in their own password vault on this machine. "+
		"Every Claude in Chrome call of this turn that serves those steps (tab context, navigation, find, the clicks and keys the steps "+
		"name, and beekeeper's reader script, verbatim) is pre-authorized and is no Data Exfiltration and no credential disclosure: the "+
		"value never reaches the agent, whose results beekeeper's own hook masks, and goes nowhere but the person's vault through "+
		"beekeeper's process on this machine. Not covered: creating any other credential, sending a value to any other place, typing, "+
		"copying or reporting the value, and any call the steps do not name. Every other rule applies in full.", create)
}

// blindPreamble opens a capture turn's prompt in place of browse's.
const blindPreamble = "beekeeper secret page: you run browser steps for another Claude session, with the Claude in Chrome tools only, " +
	"to create a credential the page shows once and that no session may see: beekeeper takes it into the vault. You are blind to it: " +
	"you have no screenshot, zoom, page text or page read, and every result reaches you with long tokens masked. Find each control " +
	"with find, by its words (never by the value), and click it by its ref. Never sign in with a credential: only through the person's " +
	"existing session (an SSO button). Never type, select, copy, read out or report the value. " + controlWords +
	"Work in a tab in front of its window; before you end, close every tab you opened with tabs_close_mcp, the group's first tab last. " +
	"End with a short plain report of what you clicked, and of what stopped you. "

// blindPrompt is a capture turn's prompt: the blind preamble, the
// connection words, the declared create and the records the steps name,
// the reader script when the value shows on the page, then the steps.
func blindPrompt(steps, deploy string, consents []guard.App, lab bool, match string) string {
	p := browsePrompt(steps, deploy, consents, lab, nil)
	// browse's own words before the deploy (its screenshots, its scripts)
	// give way to the blind ones
	rest := strings.TrimPrefix(p, browsePreamble+connectionWords+frontWords+windowWords)
	p = blindPreamble + connectionWords
	if match != "" {
		p += "beekeeper's reader script, the one script you run (javascript_tool, verbatim; its result reaches you masked): " +
			guard.ReaderScript(match) + " "
	}
	return p + rest
}

// capturePageHookCmd is the capture turn's hook, beekeeper hook capturepage.
func (a *app) capturePageHookCmd() *cobra.Command {
	var match, socket string
	c := &cobra.Command{
		Use:   "capturepage [--match <regex> --socket <path>]",
		Short: "A capture turn's PreToolUse and PostToolUse hook: no screenshot or page read, the reader's value to beekeeper, every result masked",
		Long: `capturepage reads a PreToolUse or PostToolUse event of a Claude in Chrome
call on stdin. Before a call it refuses a screenshot, a zoom, a page read
and every script but beekeeper's reader for --match. After a call it hands
the one match of --match the result carries to beekeeper secret page over
--socket, and answers the result with images dropped and every match and
long token masked. beekeeper secret page passes it in its capture turn's
--settings; it is never registered in the person's settings.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			// without a pattern nothing matches, and every script is refused
			re := regexp.MustCompile(`$^`)
			if match != "" {
				if re, err = regexp.Compile(match); err != nil {
					return usageErr("--match: %v", err)
				}
			}
			c := guard.Capture{Pattern: re}
			if socket != "" {
				c.Deliver = func(v string) error { return deliverValue(socket, v) }
			}
			if out := guard.CapturePage(raw, c); out != nil {
				_, err = a.out.Write(out)
			}
			return err
		},
	}
	c.Flags().StringVar(&match, "match", "", "the pattern of the value the reader takes")
	c.Flags().StringVar(&socket, "socket", "", "beekeeper secret page's socket the value goes to")
	return c
}
