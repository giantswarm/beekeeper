package guard

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// A self-hosted GitLab's consent page is its OAuth authorize page
// (Doorkeeper's), "Authorize <application>", on the GitLab's own host: its
// URL carries the application's client id (its uid) and the redirect_uri of
// the authorization request. The hook decides a call on it from the GitLab
// applications on record for that host: allowed for the application whose
// client id the URL carries (or whose name the title carries where the
// Chrome tools redacted the id) and whose callback host the redirect names,
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

// gitlabConsent decides a call on the authorize page u of a GitLab on
// record, titled title.
func gitlabConsent(apps []App, u *url.URL, title string) Decision {
	host := strings.ToLower(u.Hostname())
	q := u.Query()
	clientID := q.Get("client_id")
	callback := ""
	if r, err := url.Parse(q.Get("redirect_uri")); err == nil {
		callback = strings.ToLower(r.Host)
	}
	for _, a := range apps {
		if !strings.EqualFold(a.GitLab, host) || !slices.Contains(a.Callbacks, callback) {
			continue
		}
		if (clientID != "" && clientID == a.ClientID) || (clientID == chromeRedacted && strings.EqualFold(gitlabTitled(title), a.Name)) {
			return Decision{Allow, fmt.Sprintf("beekeeper allows the click: the consent page on %s of the person's own GitLab application %s (callback %s), "+
				"declared in %s, on record since %s by %s: %s", host, a.Name, callback, a.Declared, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
		}
	}
	which := "of client id " + clientID
	if clientID == "" || clientID == chromeRedacted {
		which = strconv.Quote(title)
	}
	return Decision{Deny, fmt.Sprintf("Refused: %s's consent page %s (callback %s) names a GitLab application beekeeper has no record of. "+
		"A consent is a grant of the person's identity, answered only for an application the person owns and asked for, which %s "+
		"records on their word; every other grant stays refused.", host, which, callback, GitLabForm)}
}
