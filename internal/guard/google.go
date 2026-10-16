package guard

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Google's sign-in and consent pages are on one host. Its OAuth flow opens
// with the authorization request, whose redirect_uri names the App's
// callback host (the account chooser carries it, a sign-in challenge in its
// continue URL); the pages after it ("Sign in to <app>", the consent of
// scopes, the unverified-app warning) carry only the client id, which the
// Chrome tools redact, under one title for all of them. So the hook reads
// the callback host of a tab's flow from the tab's earlier pages on that
// host (Page.Flow, lastPages), and decides a call on a page of the flow
// from the Google Apps on record: allowed for the App whose callback host
// the flow names (and whose client id the URL carries, when not redacted),
// refused for any other callback host, a third party's consent. A flow
// whose callback host the transcript does not show is decided as a consent
// page elsewhere.
const googleHost = "accounts.google.com"

// googleGrantPaths open the paths of Google's OAuth pages where a click is a
// grant: the authorization endpoint, "Sign in to <app>", the consent of
// scopes, the unverified-app warning.
var googleGrantPaths = []string{"/o/oauth2/", "/signin/oauth/"}

// GoogleForm is how a consent for a Google OAuth client the person owns and
// asked for is recorded.
const GoogleForm = "`beekeeper app allow <name> --google --callback <host> --word \"<the person's words>\"`"

// googleGrant reports whether path is one of Google's OAuth grant pages.
func googleGrant(path string) bool {
	return slices.ContainsFunc(googleGrantPaths, func(p string) bool { return strings.HasPrefix(path, p) })
}

// GoogleCallback is the callback host of the Google authorization request
// u carries: its redirect_uri's host, or that of the request its continue
// URL carries; "" for none.
func GoogleCallback(u *url.URL) string {
	q := u.Query()
	if r, err := url.Parse(q.Get("redirect_uri")); err == nil && r.Host != "" {
		return strings.ToLower(r.Host)
	}
	if c, err := url.Parse(q.Get("continue")); err == nil && strings.EqualFold(c.Hostname(), googleHost) {
		if r, err := url.Parse(c.Query().Get("redirect_uri")); err == nil && r.Host != "" {
			return strings.ToLower(r.Host)
		}
	}
	return ""
}

// googleClick decides a call on page, at u on Google's host, in the call's
// own words: allowed on any page of a flow whose callback host is a Google
// App's on record, refused on a grant page of a flow for any other callback
// host; a flow the transcript names no callback host of is decided as a
// consent page elsewhere, and so is a grant page whose client id is
// another's than the record of its callback host.
func googleClick(apps []App, u *url.URL, page Page, words string) Decision {
	callback := GoogleCallback(u)
	if callback == "" {
		callback = page.Flow
	}
	clientID := u.Query().Get("client_id")
	if a := googleApp(apps, callback, clientID); a != nil {
		return Decision{Allow, fmt.Sprintf("beekeeper allows it: Google's sign-in to the person's own App %s (callback %s), on record since %s by %s: %s",
			a.Name, callback, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
	}
	if callback != "" && googleGrant(u.Path) {
		return Decision{Deny, fmt.Sprintf("Refused: %s is Google's consent page (an OAuth grant) of an App whose callback is %s, which beekeeper has no "+
			"record of as the person's own. A consent is a grant of the person's identity, answered only for an App the person owns and asked for, "+
			"which %s records on their word; every other grant stays refused.", u.Path, callback, GoogleForm)}
	}
	return elsewhere(u, page.Title, words)
}

// googleApp is the Google App on record whose callback hosts hold callback
// and whose client id, when the URL carries one unredacted and the record
// one too, is clientID; nil for none.
func googleApp(apps []App, callback, clientID string) *App {
	if callback == "" {
		return nil
	}
	for i := range apps {
		a := &apps[i]
		if !a.Google || !slices.Contains(a.Callbacks, callback) {
			continue
		}
		if clientID != "" && clientID != chromeRedacted && a.ClientID != "" && clientID != a.ClientID {
			continue
		}
		return a
	}
	return nil
}
