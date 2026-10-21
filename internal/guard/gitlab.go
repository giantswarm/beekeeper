package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A self-hosted GitLab's consent page is its OAuth authorize page
// (Doorkeeper's), "Authorize <application>", on the GitLab's own host: its
// URL carries the application's client id (its uid) and the redirect_uri of
// the authorization request. The hook decides a call on it from the GitLab
// applications on record for that host: allowed for the application whose
// client id the URL carries (or, where the Chrome tools redacted the id, whose
// name alone the page's title and the text the tools read from it carry: the
// title is the site's name, "GitLab", on current GitLab) and whose callback
// host the redirect names,
// refused for any other application or callback host there. A GitLab no
// record names is decided as a consent page elsewhere.
const gitlabAuthorize = "/oauth/authorize"

// GitLabForm is how a consent for a self-hosted GitLab's OAuth application
// the person owns and asked for is recorded.
const GitLabForm = "`beekeeper app allow <name> --gitlab <host> --client-id <uid> --callback <host> --declared <ref> --word \"<the person's words>\"`"

// gitlabHost reports whether a GitLab application on record is on host.
func gitlabHost(apps []App, host string) bool {
	return slices.ContainsFunc(apps, func(a App) bool { return a.GitLab != "" && strings.EqualFold(a.GitLab, host) })
}

// gitlabTitleSuffixes close GitLab's authorize page title after the
// application's name, outermost first: the site's name, the question.
var gitlabTitleSuffixes = []string{" · GitLab", " | GitLab", " to use your account?"}

// gitlabTitled is the application name a GitLab authorize page's title
// carries, exactly: "Authorize <name>[ to use your account?][ · GitLab]"; ""
// for any other title. A name is matched whole, never by its prefix.
func gitlabTitled(title string) string {
	t := strings.TrimSpace(title)
	if !strings.HasPrefix(t, authorizeTitle) {
		return ""
	}
	for _, s := range gitlabTitleSuffixes {
		t = strings.TrimSuffix(t, s)
	}
	return strings.TrimSpace(strings.TrimPrefix(t, authorizeTitle))
}

// gitlabNamed are the application names a GitLab authorize page carries
// where the Chrome tools redacted its client id: the title's, and each
// "Authorize <name>" its text shows (the heading "Authorize <name> to use your
// account?", the button "Authorize <name>"), never the bare title "GitLab".
func gitlabNamed(page Page) []string {
	var names []string
	if n := gitlabTitled(page.Title); n != "" {
		names = append(names, n)
	}
	for _, m := range gitlabAuthorizeText.FindAllStringSubmatch(page.Text, -1) {
		if n := strings.TrimSpace(m[1]); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

// gitlabAuthorizeText is an "Authorize <name>" a GitLab authorize page's
// text shows, in the page's own forms only: a button's or heading's label in
// its accessibility tree or a find's match (`button "Authorize <name>"`,
// quoted once or twice), or a whole line of its text; the name holds no
// quote and runs to the heading's question or the label's or line's end.
var gitlabAuthorizeText = regexp.MustCompile(`(?m)(?:\b(?:button|heading) "{1,2}|^[^\S\n]*)Authorize ([^"\n]+?)(?: to use your account\?)?(?:"|[^\S\n]*$)`)

// gitlabNames reports whether the names a page carries name the
// application name alone.
func gitlabNames(names []string, name string) bool {
	return len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return !strings.EqualFold(n, name) })
}

// gitlabConsent decides a call on the authorize page u of a GitLab on
// record, the tab's page.
func gitlabConsent(apps []App, u *url.URL, page Page) Decision {
	host := strings.ToLower(u.Hostname())
	q := u.Query()
	clientID := q.Get("client_id")
	callback := ""
	if r, err := url.Parse(q.Get("redirect_uri")); err == nil {
		callback = strings.ToLower(r.Host)
	}
	var names []string
	if clientID == chromeRedacted {
		names = gitlabNamed(page)
	}
	for _, a := range apps {
		if !strings.EqualFold(a.GitLab, host) || !slices.Contains(a.Callbacks, callback) {
			continue
		}
		if (clientID != "" && clientID == a.ClientID) || (clientID == chromeRedacted && gitlabNames(names, a.Name)) {
			return Decision{Allow, fmt.Sprintf("beekeeper allows the click: the consent page on %s of the person's own GitLab application %s (callback %s), "+
				"declared in %s, on record since %s by %s: %s", host, a.Name, callback, a.Declared, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
		}
	}
	which := "of client id " + clientID
	switch {
	case len(names) > 0:
		which = strconv.Quote(authorizeTitle + strings.Join(names, ", "))
	case clientID == "" || clientID == chromeRedacted:
		which = strconv.Quote(page.Title)
	}
	return Decision{Deny, fmt.Sprintf("Refused: %s's consent page %s (callback %s) names a GitLab application beekeeper has no record of. "+
		"A consent is a grant of the person's identity, answered only for an application the person owns and asked for, which %s "+
		"records on their word; every other grant stays refused.", host, which, callback, GitLabForm)}
}
