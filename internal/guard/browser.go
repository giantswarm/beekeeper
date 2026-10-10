package guard

import (
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// browserLease is the lease on the person's Chrome.
const browserLease = "browser"

// browserOpener: a command that opens a page in the person's browser: an
// OAuth sign-in (muster auth login, gh auth login --web) or a URL handed to
// the desktop (xdg-open). The page opens in front of whatever the person is
// working in, and they close it as a stray pop-up.
var browserOpener = regexp.MustCompile(`(?m)` + pos + `(muster\s+auth\s+login\b|gh\s+auth\s+login\b[^;&|\n]*\s(?:-w|--web)\b|xdg-open\s)`)

// browserRefusal returns why the hook refuses cmd, "" when it passes: a
// command that opens the person's browser runs only in the session holding
// the browser lease.
func (h Hook) browserRefusal(cmd, session string) string {
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
		"files to Read; a deploy or create click the task covers is declared with `--allow-deploy \"<what, where>\"`. " +
		"Hold the browser lease while it runs, as for any browser work."
}
