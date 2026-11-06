package guard

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// browserLease is the lease on the person's Chrome.
const browserLease = "browser"

// browserOpener: a command that opens a page in the person's browser: an
// OAuth sign-in (gh auth login --web) or a URL handed to the desktop
// (xdg-open). The page opens in front of whatever the person is working in,
// and they close it as a stray pop-up.
var browserOpener = regexp.MustCompile(`(?m)` + pos + `(gh\s+auth\s+login\b[^;&|\n]*\s(?:-w|--web)\b|xdg-open\s)`)

// musterLogin: a muster sign-in, with the wrappers and assignments in front
// of it (group 1) and its arguments up to the end of the simple command
// (group 2).
var musterLogin = regexp.MustCompile(`(?m)` + start + `(` + posWrappers + `)muster\s+auth\s+login\b([^;&|\n]*)`)

// musterBoolFlags are the boolean flags of muster auth login and its parents:
// they take no separate value. Every other flag without "=" may take the next
// word as its value, as pflag reads it, so a --no-browser right after one is
// no flag of its own.
var musterBoolFlags = map[string]bool{allFlag: true, "--silent": true, "--force": true, "--quiet": true, "-q": true}

// musterNoBrowserEnv is the environment variable muster reads in place of
// --no-browser.
const musterNoBrowserEnv = "MUSTER_NO_BROWSER"

// browserRefusal returns why the hook refuses cmd, "" when it passes. A muster
// sign-in that opens a browser is refused in every session (musterLoginRefusal);
// one with --no-browser opens nothing and passes in every session. Any other
// command that opens the person's browser runs only in the session holding the
// browser lease.
func (h Hook) browserRefusal(cmd, session string) string {
	for _, later := range laterCommands(cmd) {
		if r := h.browserRefusal(later, session); r != "" {
			return r
		}
	}
	if r := musterLoginRefusal(cmd); r != "" {
		return r
	}
	cmd = commandText(cmd)
	m := browserOpener.FindStringSubmatch(cmd)
	if m == nil || h.Leases == nil {
		return ""
	}
	if slices.ContainsFunc(h.Leases(), func(l lease.Holder) bool {
		return l.Env == browserLease && heldBy(l, session)
	}) {
		return ""
	}
	return fmt.Sprintf("Matched token %q at byte %d. ", strings.Fields(m[1])[0], strings.Index(cmd, m[1])) + "Refused: `" + short(m[1]) + "` opens a page in the person's browser, in front of whatever they are working in, " +
		"and they close it as a stray pop-up. Only the session holding the browser lease opens one: ask your supervisor " +
		"for \"browser\", claim it with `beekeeper lease claim browser -p \"<purpose>\"` after its yes, run the sign-in, " +
		"and release the lease right after."
}

// musterLoginRefusal returns why the hook refuses a muster sign-in in cmd that
// opens a browser, "" when cmd runs none. muster opens the person's default
// browser, outside the Claude in Chrome tab group, where neither a session's
// browser tools nor beekeeper browse reach the page, so not even the session
// holding the browser lease can finish it. With --no-browser muster prints
// the sign-in URL and waits for its callback, and a browse step opens it.
func musterLoginRefusal(cmd string) string {
	cmd = commandText(cmd)
	for _, m := range musterLogin.FindAllStringSubmatchIndex(cmd, -1) {
		prefix, args := cmd[m[2]:m[3]], cmd[m[4]:m[5]]
		if noBrowserArgs(args) || noBrowserAssigned(prefix) {
			continue
		}
		return fmt.Sprintf("Matched token %q at byte %d. ", "muster", m[3]) + "Refused: `muster auth login` opens the person's default browser, outside the Claude in Chrome tab group, " +
			"where neither this session's browser tools nor `beekeeper browse` reach the page: the sign-in tab sits in front " +
			"of the person and never finishes. Run `muster auth login --no-browser` instead (it passes in every session: it " +
			"opens nothing, prints the sign-in URL and waits for the callback, so start it in the background), then open " +
			"that URL in a browse step under the browser lease: ask your supervisor for \"browser\", claim it with " +
			"`beekeeper lease claim browser -p \"<purpose>\"` after its yes, run `beekeeper browse \"Open <the printed URL>, " +
			"sign in with its SSO button, report the final page\"`, and release the lease right after."
	}
	return ""
}

// noBrowserArgs reports whether a muster sign-in's arguments make it open no
// browser, read in order as pflag reads them: the last --no-browser decides
// (--no-browser=<v> as a boolean), a help request shows help and signs in to
// nothing, a value-taking flag swallows the next word, "--" ends the flags,
// and a shell comment ends the arguments.
func noBrowserArgs(args string) bool {
	noBrowser := false
	words := shellWords(stripComment(args))
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--":
			return noBrowser
		case w == "-h" || w == litHelp:
			return true
		case w == "--no-browser":
			noBrowser = true
		case strings.HasPrefix(w, "--no-browser="):
			on, err := strconv.ParseBool(strings.TrimPrefix(w, "--no-browser="))
			noBrowser = err == nil && on
		case strings.HasPrefix(w, "-") && !strings.Contains(w, "=") && !musterBoolFlags[w]:
			i++ // the flag's value, whatever it looks like
		}
	}
	return noBrowser
}

// stripComment cuts a shell comment off s: an unquoted # that starts a word.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				i++
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// noBrowserAssigned reports whether the assignments in front of a muster
// sign-in (prefix) disable its browser: the last MUSTER_NO_BROWSER assignment
// there, read as muster reads it (any value but empty or a false one). The
// session's environment and an earlier export are not this command's to
// judge (a later assignment, unset or subshell may undo them), so they never
// exempt it.
func noBrowserAssigned(prefix string) bool {
	on := false
	for _, w := range shellWords(prefix) {
		v, ok := strings.CutPrefix(w, musterNoBrowserEnv+"=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "0", "f", "false", "no", "off":
			on = false
		default:
			on = true
		}
	}
	return on
}

// heldBy reports whether the session holds the lease l.
func heldBy(l lease.Holder, session string) bool {
	return session != "" && (l.Session == session || l.HostSession == "local_"+session)
}

// desktopBrowserTools is the prefix of the Claude in Chrome tools as Claude
// Desktop serves them to its CLIs, the same name the CLI's own integration
// uses, compared without case.
const desktopBrowserTools = "mcp__claude_in_chrome__"

// isBrowserTool reports whether tool is a Claude in Chrome tool.
func isBrowserTool(tool string) bool {
	t := strings.ReplaceAll(strings.ToLower(tool), "-", "_")
	return strings.HasPrefix(t, desktopBrowserTools)
}

// desktopBrowserRefusal returns why the hook refuses a browser call of a
// session beekeeper started in bypass that now runs a desktop turn
// (acceptEdits, the mode the desktop's import gives it), "" when it passes.
// The desktop holds such a session's navigate to a site it was not allowed
// on yet for its person's site approval, which neither bypassPermissions
// nor a hook answers, and with nobody at the desktop the turn waits until
// the desktop aborts it. A headless turn runs the CLI's own Chrome
// connection in bypass, which never asks.
func (h Hook) desktopBrowserRefusal(ev event) string {
	if !isBrowserTool(ev.ToolName) || ev.Mode != ModeAcceptEdits || ev.Session == "" || h.Started == nil || !h.Started(ev.Session) {
		return ""
	}
	return "Refused: in a desktop turn of a session beekeeper started, Claude Desktop holds every navigate to a site the session " +
		"was not allowed on yet for a person's site approval, which nobody answers. Run the browser steps headless instead: " +
		"`beekeeper browse \"<the steps, and what to report back>\"` (Bash, timeout up to 10 minutes) runs them in a headless " +
		"turn on the CLI's own Chrome connection, which never asks, and prints its report and the screenshots it took as image " +
		"files to Read; a deploy or create click the task covers is declared with `--allow-deploy \"<what, where>\"`, a file the steps upload with `--file <path>`. " +
		"Hold the browser lease while it runs, as for any browser work."
}
