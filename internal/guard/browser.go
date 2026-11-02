package guard

import (
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

// musterNoBrowserFlag: an argument with which muster opens no browser: the
// flag (its value, if given, in group 1) or a help request.
var musterNoBrowserFlag = regexp.MustCompile(`(?:^|\s)(?:--no-browser(?:=(\S*))?|-h|--help)(?:\s|$)`)

// musterNoBrowserEnv: an assignment of the environment variable muster reads
// in place of the flag, its value in group 1; musterNoBrowserExport its
// export by an earlier command of the call.
var (
	musterNoBrowserEnv    = regexp.MustCompile(`(?:^|\s)MUSTER_NO_BROWSER=(\S*)`)
	musterNoBrowserExport = regexp.MustCompile(`(?m)` + start + `export\s+MUSTER_NO_BROWSER=([^\s;&|)]*)`)
)

// browserRefusal returns why the hook refuses cmd, "" when it passes. A muster
// sign-in that opens a browser is refused in every session (musterLoginRefusal);
// one with --no-browser opens nothing and passes in every session. Any other
// command that opens the person's browser runs only in the session holding the
// browser lease.
func (h Hook) browserRefusal(cmd, session string) string {
	if r := musterLoginRefusal(cmd); r != "" {
		return r
	}
	m := browserOpener.FindStringSubmatch(cmd)
	if m == nil || h.Leases == nil {
		return ""
	}
	if slices.ContainsFunc(h.Leases(), func(l lease.Holder) bool {
		return l.Env == browserLease && heldBy(l, session)
	}) {
		return ""
	}
	return "Refused: `" + short(m[1]) + "` opens a page in the person's browser, in front of whatever they are working in, " +
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
	for _, m := range musterLogin.FindAllStringSubmatchIndex(cmd, -1) {
		prefix, args := cmd[m[2]:m[3]], cmd[m[4]:m[5]]
		if noBrowserFlag(args) || noBrowserEnv(musterNoBrowserEnv.FindStringSubmatch(prefix)) || noBrowserExported(cmd[:m[0]]) {
			continue
		}
		return "Refused: `muster auth login` opens the person's default browser, outside the Claude in Chrome tab group, " +
			"where neither this session's browser tools nor `beekeeper browse` reach the page: the sign-in tab sits in front " +
			"of the person and never finishes. Run `muster auth login --no-browser` instead (it passes in every session: it " +
			"opens nothing, prints the sign-in URL and waits for the callback, so start it in the background), then open " +
			"that URL in a browse step under the browser lease: ask your supervisor for \"browser\", claim it with " +
			"`beekeeper lease claim browser -p \"<purpose>\"` after its yes, run `beekeeper browse \"Open <the printed URL>, " +
			"sign in with its SSO button, report the final page\"`, and release the lease right after."
	}
	return ""
}

// noBrowserFlag reports whether a muster sign-in's arguments say it opens no
// browser: --no-browser (a value, if given, true as a boolean flag reads it)
// or a help request.
func noBrowserFlag(args string) bool {
	m := musterNoBrowserFlag.FindStringSubmatch(args)
	if m == nil {
		return false
	}
	if !strings.Contains(m[0], "--no-browser=") {
		return true
	}
	on, err := strconv.ParseBool(m[1])
	return err == nil && on
}

// noBrowserExported reports whether an earlier command of the call exports
// MUSTER_NO_BROWSER, the last export with a value that disables the browser.
func noBrowserExported(before string) bool {
	ms := musterNoBrowserExport.FindAllStringSubmatch(before, -1)
	return len(ms) > 0 && noBrowserEnv(ms[len(ms)-1])
}

// noBrowserEnv reports whether an assignment of MUSTER_NO_BROWSER (its value in
// m[1]) disables muster's browser as muster reads it: any value but empty or a
// false one (0, f, false, no, off).
func noBrowserEnv(m []string) bool {
	if m == nil {
		return false
	}
	switch strings.ToLower(strings.Trim(m[1], `"'`)) {
	case "", "0", "f", "false", "no", "off":
		return false
	}
	return true
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
