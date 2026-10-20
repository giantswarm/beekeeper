package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Adding a callback URL to an App is a change of its settings: on the App's
// settings page under the organisation's settings the person clicks Add
// Callback URL, types the URL and clicks Save changes. The hook answers
// those calls from the record of the one callback URL the person asked for
// (App.CallbackURL), a URL on one of the callback hosts the App's manifest
// declares: the navigation to the recorded App's settings page and the
// calls there that name a callback, the recorded URL or Save changes are
// allowed; a call there that names another URL, removes or deletes
// anything, or names none of them is refused. Without such a record the
// settings page is left to the classifier, as before.

// CallbackForm is how one callback URL of an App the person owns and asked
// for is recorded.
const CallbackForm = "`beekeeper app allow <name> --callback-url <url> --word \"<the person's words>\"`"

// settingsPath is an App's settings page under the organisation's settings.
var settingsPath = regexp.MustCompile(`^/organizations/([^/]+)/settings/apps/([^/]+)/?$`)

// urlWords are the URLs a call's words carry.
var urlWords = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// removeWords name a call that takes something off the page.
var removeWords = regexp.MustCompile(`(?i)\b(remove|delete|clear)\b`)

// callbackNavigation decides a navigation to GitHub's page at path: the
// settings page of an App on record for a callback URL is allowed; no
// decision for any other page.
func callbackNavigation(apps []App, path string) (Decision, bool) {
	m := settingsPath.FindStringSubmatch(path)
	if m == nil {
		return Decision{}, false
	}
	a := callbackRecord(apps, m[1], m[2])
	if a == nil {
		return Decision{}, false
	}
	return Decision{Allow, "beekeeper allows it: the settings page of " + callbackWords(a)}, true
}

// callbackClick decides a call in words on GitHub's page u when it is the
// settings page of an App on record for a callback URL; no decision for any
// other page.
func callbackClick(apps []App, u *url.URL, words string) (Decision, bool) {
	m := settingsPath.FindStringSubmatch(u.Path)
	if m == nil {
		return Decision{}, false
	}
	a := callbackRecord(apps, m[1], m[2])
	if a == nil {
		return Decision{}, false
	}
	if addsCallback(a, words) {
		return Decision{Allow, "beekeeper allows the call: the settings page of " + callbackWords(a)}, true
	}
	return Decision{Deny, fmt.Sprintf("Refused: on the settings page of the person's own App %s the call %q is neither the one callback URL "+
		"on record (%s), its Add Callback URL nor Save changes; every other change there stays refused.", a.Name, short(words), a.CallbackURL)}, true
}

// addsCallback reports whether a call's words are a step of adding the
// recorded callback URL: they name no other URL and remove nothing, and
// name a callback, the recorded URL or the page's Save.
func addsCallback(a *App, words string) bool {
	if removeWords.MatchString(words) {
		return false
	}
	for _, w := range urlWords.FindAllString(words, -1) {
		if !sameURL(w, a.CallbackURL) {
			return false
		}
	}
	return savesChanges(words) || containsFold(words, "callback") || urlWords.MatchString(words)
}

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

// callbackRecord is the App on record for a callback URL whose settings
// page under org's settings slug names, nil for none.
func callbackRecord(apps []App, org, slug string) *App {
	for i := range apps {
		if a := &apps[i]; a.CallbackURL != "" && strings.EqualFold(a.Org, org) && strings.EqualFold(AppSlug(a.Name), slug) {
			return a
		}
	}
	return nil
}

// callbackWords name the record of a callback URL: the App, where it is
// declared, the URL, and the person's word.
func callbackWords(a *App) string {
	return fmt.Sprintf("the person's own App %s, declared in %s, on record for the callback URL %s since %s by %s: %s",
		a.Name, a.Declared, a.CallbackURL, a.At.UTC().Format("2006-01-02"), a.By, a.Word)
}
