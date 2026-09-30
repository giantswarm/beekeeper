package guard

import (
	"regexp"
	"slices"

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
		return l.Env == browserLease && session != "" && (l.Session == session || l.HostSession == "local_"+session)
	}) {
		return ""
	}
	return "Refused: `" + short(m[1]) + "` opens a page in Timo's browser, in front of whatever he is working in, " +
		"and he closes it as a stray pop-up. Only the session holding the browser lease opens one: ask your supervisor " +
		"for \"browser\", claim it with `beekeeper lease claim browser -p \"<purpose>\"` after its yes, run the sign-in, " +
		"and release the lease right after."
}
