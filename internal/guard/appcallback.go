package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Adding a callback URL to an App is a change of its settings: on the App's
// settings page under the organisation's settings the person clicks Add
// Callback URL, types the URL and clicks Save changes. The hook answers
// those calls from the record of the one callback URL the person asked for
// (App.CallbackURL), a URL on one of the callback hosts the App's manifest
// declares: the navigation to the recorded App's settings page, a click
// there that names Add Callback URL, Save changes or the recorded URL, and
// typing exactly the recorded URL are allowed; a call there that names
// another URL or another field, removes anything, or types anything else
// (a key included) is refused. Without such a record the settings page is
// left to the classifier, as before.

// CallbackForm is how one callback URL of an App the person owns and asked
// for is recorded.
const CallbackForm = "`beekeeper app allow <name> --callback-url <url> --word \"<the person's words>\"`"

// settingsPath is an App's settings page under the organisation's settings.
var settingsPath = regexp.MustCompile(`^/organizations/([^/]+)/settings/apps/([^/]+)/?$`)

// urlWords are the URLs a call's words carry.
var urlWords = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// otherWords name a call that removes something or acts on another part of
// the settings page than its callback URLs.
var otherWords = regexp.MustCompile(`(?i)\b(remove|delete|clear|secret|private key|webhook|generate|regenerate|revoke|transfer|suspend|uninstall|homepage|setup url)\b`)

// callbackLabels are the page's labels of adding a callback URL.
var callbackLabels = []string{"add callback url", "save changes"}

// callbackNavigation decides a navigation to GitHub's page at path: the
// settings page of an App on record for a callback URL is allowed; no
// decision for any other page.
func callbackNavigation(apps []App, path string) (Decision, bool) {
	m := settingsPath.FindStringSubmatch(path)
	if m == nil {
		return Decision{}, false
	}
	recs := callbackRecords(apps, m[1], m[2])
	if len(recs) == 0 {
		return Decision{}, false
	}
	return Decision{Allow, "beekeeper allows it: the settings page of " + callbackWords(recs[0])}, true
}

// callbackClick decides a call in words on GitHub's page u when it is the
// settings page of an App on record for a callback URL; no decision for any
// other page.
func callbackClick(apps []App, u *url.URL, words string) (Decision, bool) {
	m := settingsPath.FindStringSubmatch(u.Path)
	if m == nil {
		return Decision{}, false
	}
	recs := callbackRecords(apps, m[1], m[2])
	if len(recs) == 0 {
		return Decision{}, false
	}
	for _, a := range recs {
		if addsCallback(a, words) {
			return Decision{Allow, "beekeeper allows the call: the settings page of " + callbackWords(a)}, true
		}
	}
	return Decision{Deny, fmt.Sprintf("Refused: on the settings page of the person's own App %s the call %q is neither a callback URL "+
		"on record (%s), Add Callback URL nor Save changes; every other change there stays refused.", recs[0].Name, short(words), callbackURLs(recs))}, true
}

// addsCallback reports whether a call's words are a step of adding the
// recorded callback URL: they name no other URL and no other part of the
// page, and name Add Callback URL, Save changes, or a callback with the
// recorded URL.
func addsCallback(a *App, words string) bool {
	if otherWords.MatchString(words) {
		return false
	}
	urls := urlWords.FindAllString(words, -1)
	for _, w := range urls {
		if !sameURL(w, a.CallbackURL) {
			return false
		}
	}
	if len(urls) > 0 {
		return containsFold(words, "callback")
	}
	for _, l := range callbackLabels {
		if containsFold(words, l) {
			return true
		}
	}
	return false
}

// typedCallback decides input typed on the page (a text, a key, a form
// value) that Click allowed in the call's words: on the settings page of an
// App on record for a callback URL only exactly that URL is typed; d
// unchanged on any other page.
func typedCallback(apps []App, page Page, text string, d Decision) Decision {
	u, err := url.Parse(strings.TrimSpace(page.URL))
	if err != nil || !strings.EqualFold(u.Hostname(), githubHost) {
		return d
	}
	m := settingsPath.FindStringSubmatch(u.Path)
	if m == nil {
		return d
	}
	recs := callbackRecords(apps, m[1], m[2])
	if len(recs) == 0 || slices.ContainsFunc(recs, func(a *App) bool { return strings.TrimSpace(text) == a.CallbackURL }) {
		return d
	}
	return Decision{Deny, fmt.Sprintf("Refused: on the settings page of the person's own App %s the input %q is not a callback URL on record "+
		"(%s); only such a URL is typed there.", recs[0].Name, short(text), callbackURLs(recs))}
}

// CanonicalCallback reports whether raw is a callback URL as a record keeps
// it: http(s), a host, no user info, a path and query of URL-safe
// characters, written as Go's URL parser writes it back.
func CanonicalCallback(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && canonicalCallback.MatchString(raw) && u.User == nil && u.String() == raw
}

// canonicalCallback is a callback URL's form.
var canonicalCallback = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/%=&?+-]*)?$`)

// sameURL reports whether two URLs are the same callback: scheme and host
// in any case, a trailing slash or punctuation dropped.
func sameURL(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimRight(strings.TrimSpace(s), ".,;:)]}")
		u, err := url.Parse(s)
		if err != nil {
			return s
		}
		u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
		return strings.TrimSuffix(u.String(), "/")
	}
	return norm(a) == norm(b)
}

// callbackRecords are the records of callback URLs of the App whose
// settings page under org's settings slug names, none for an App on no
// such record.
func callbackRecords(apps []App, org, slug string) []*App {
	var out []*App
	for i := range apps {
		if a := &apps[i]; a.CallbackURL != "" && strings.EqualFold(a.Org, org) && strings.EqualFold(AppSlug(a.Name), slug) {
			out = append(out, a)
		}
	}
	return out
}

// callbackURLs name the callback URLs of recs.
func callbackURLs(recs []*App) string {
	urls := make([]string, 0, len(recs))
	for _, a := range recs {
		urls = append(urls, a.CallbackURL)
	}
	return strings.Join(urls, " ")
}

// callbackWords name the record of a callback URL: the App, where it is
// declared, the URL, and the person's word.
func callbackWords(a *App) string {
	return fmt.Sprintf("the person's own App %s, declared in %s, on record for the callback URL %s since %s by %s: %s",
		a.Name, a.Declared, a.CallbackURL, a.At.UTC().Format("2006-01-02"), a.By, a.Word)
}
