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

// googleRequestPaths are the paths of Google's pages whose redirect_uri is
// the authorization request's own: the authorization endpoint and the
// account chooser it opens. Any other page's is not read.
var googleRequestPaths = []string{"/o/oauth2/auth", "/o/oauth2/v2/auth", "/v3/signin/accountchooser", "/o/oauth2/auth/oauthchooseaccount"}

// googleGrant reports whether path is one of Google's OAuth grant pages.
func googleGrant(path string) bool {
	path = strings.ToLower(path)
	return slices.ContainsFunc(googleGrantPaths, func(p string) bool { return strings.HasPrefix(path, p) })
}

// googleFlowPaths open the paths of the pages an OAuth flow on Google's
// sign-in passes: its request, its grant pages, a sign-in challenge.
var googleFlowPaths = append([]string{"/v3/signin/"}, googleGrantPaths...)

// GoogleCallback is the callback host of the Google authorization request
// u carries: its redirect_uri's host, or that of the request its continue
// URL carries; "" for none.
func GoogleCallback(u *url.URL) string {
	if !strings.EqualFold(u.Hostname(), googleHost) {
		return ""
	}
	if cb := requestCallback(u); cb != "" {
		return cb
	}
	if c, err := url.Parse(u.Query().Get("continue")); err == nil && strings.EqualFold(c.Hostname(), googleHost) {
		return requestCallback(c)
	}
	return ""
}

// requestCallback is the callback host of u, an authorization request on
// Google's host carrying a client id and exactly one redirect_uri; "" for
// any other page.
func requestCallback(u *url.URL) string {
	q := u.Query()
	if !slices.Contains(googleRequestPaths, strings.ToLower(u.Path)) || q.Get("client_id") == "" || len(q["redirect_uri"]) != 1 {
		return ""
	}
	r, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || r.Host == "" {
		return ""
	}
	return strings.ToLower(r.Host)
}

// googleFlow is a tab's OAuth flow on Google's sign-in: the callback host
// of its authorization request, and the request id (as) Google's pages
// after it carry, bound at the first of them.
type googleFlow struct {
	callback, request string
}

// next is the flow once the tab shows u on Google's host: a new
// authorization request starts a new flow; a page off the flow's paths, one
// carrying a redirect_uri of no request, or one of another request id than
// the flow's ends it. The account chooser carries no request id: the flow
// binds the one its first page after carries.
func (f googleFlow) next(u *url.URL) googleFlow {
	if cb := GoogleCallback(u); cb != "" {
		return googleFlow{callback: cb, request: u.Query().Get("as")}
	}
	path, q := strings.ToLower(u.Path), u.Query()
	if !slices.ContainsFunc(googleFlowPaths, func(p string) bool { return strings.HasPrefix(path, p) }) || carriesRequest(q) {
		return googleFlow{}
	}
	as := q.Get("as")
	switch {
	case as == "" || f.callback == "":
		return f
	case f.request == "":
		f.request = as
		return f
	case as != f.request:
		return googleFlow{}
	}
	return f
}

// carriesRequest reports whether q, a page's query that is no request
// GoogleCallback reads, may still carry one: a redirect_uri, or any
// continue or followup URL, whose nesting the hook does not unwrap.
func carriesRequest(q url.Values) bool {
	return q.Has("redirect_uri") || q.Get("continue") != "" || q.Get("followup") != ""
}

// onGoogle reports whether page is on Google's sign-in.
func onGoogle(page Page) bool {
	u, err := url.Parse(page.URL)
	return err == nil && strings.EqualFold(u.Hostname(), googleHost)
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
