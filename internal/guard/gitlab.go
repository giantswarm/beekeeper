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
// client id the URL carries, or, where the Chrome tools redacted the id, the
// consent form's hidden client_id carries (GitLabFormScript reads it), and
// whose callback host the redirect names; refused for any other application
// or callback host there. The application's name, which its owner chooses
// and Chrome's labels quote unescaped, identifies nothing. A GitLab no record
// names is decided as a consent page elsewhere.
const gitlabAuthorize = "/oauth/authorize"

// GitLabForm is how a consent for a self-hosted GitLab's OAuth application
// the person owns and asked for is recorded.
const GitLabForm = "`beekeeper app allow <name> --gitlab <host> --client-id <uid> --callback <host> --declared <ref> --word \"<the person's words>\"`"

// gitlabFormOpen opens GitLabFormScript's answer: the client id that
// follows it, or its refusal to name one.
const gitlabFormOpen = "beekeeper consent form: "

// GitLabFormScript reads the client id of a GitLab authorize page's consent
// form, which the Chrome tools redact everywhere else (the tab's URL, the
// hidden inputs of read_page and find): the one form on the page's own
// origin that posts to /oauth/authorize (the Cancel form's _method is
// delete), its hidden client_id. GitLab assigns the id; the application's
// owner chooses nothing of the answer.
const GitLabFormScript = `(() => {
  const forms = [...document.querySelectorAll('form')].filter(f => { const a = new URL(f.action, location.href);
    return a.origin === location.origin && a.pathname === '` + gitlabAuthorize + `' && !f.querySelector('input[name=_method]') && f.querySelector('input[name=client_id]'); });
  if (forms.length !== 1) return '` + gitlabFormOpen + `none: ' + forms.length + ' authorize forms';
  return '` + gitlabFormOpen + `application ' + forms[0].querySelector('input[name=client_id]').value;
})()`

// gitlabFormAnswer is GitLabFormScript's answer naming a client id.
var gitlabFormAnswer = regexp.MustCompile(`^` + regexp.QuoteMeta(gitlabFormOpen) + `application ([A-Za-z0-9_-]+)$`)

// isGitLabForm reports whether code is GitLabFormScript and nothing else.
func isGitLabForm(code string) bool {
	return strings.TrimSpace(code) == GitLabFormScript
}

// gitlabFormID is the client id a result of GitLabFormScript names, "" for
// none.
func gitlabFormID(body string) string {
	if m := gitlabFormAnswer.FindStringSubmatch(strings.TrimSpace(body)); m != nil && m[1] != chromeRedacted {
		return m[1]
	}
	return ""
}

// gitlabHost reports whether a GitLab application on record is on host.
func gitlabHost(apps []App, host string) bool {
	return slices.ContainsFunc(apps, func(a App) bool { return a.GitLab != "" && strings.EqualFold(a.GitLab, host) })
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
	if clientID == chromeRedacted {
		clientID = page.Form
	}
	for _, a := range apps {
		if !strings.EqualFold(a.GitLab, host) || !slices.Contains(a.Callbacks, callback) {
			continue
		}
		if clientID != "" && clientID == a.ClientID {
			return Decision{Allow, fmt.Sprintf("beekeeper allows the click: the consent page on %s of the person's own GitLab application %s (callback %s), "+
				"declared in %s, on record since %s by %s: %s", host, a.Name, callback, a.Declared, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
		}
	}
	which, how := "of client id "+clientID, ""
	if clientID == "" {
		which = strconv.Quote(page.Title)
		how = " The Chrome tools redact the client id the page's URL carries: read it from the consent form first, with javascript_tool " +
			"on the tab, this script verbatim:\n" + GitLabFormScript + "\nthen click again, with no other call on the tab between."
	}
	return Decision{Deny, fmt.Sprintf("Refused: %s's consent page %s (callback %s) names a GitLab application beekeeper has no record of. "+
		"A consent is a grant of the person's identity, answered only for an application the person owns and asked for, which %s "+
		"records on their word; every other grant stays refused.%s", host, which, callback, GitLabForm, how)}
}
