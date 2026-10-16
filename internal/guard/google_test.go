package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Google OAuth client on record and the pages of Google's sign-in the
// decision is tested on, as the Chrome tools report them: the account
// chooser carries the authorization request's redirect_uri, the pages after
// it a redacted client id only, a sign-in challenge the request in its
// continue URL.
const (
	googleApp      = "example-portal"
	googleCallback = "portal.example.org"
	googleClientID = "1234-abc.apps.googleusercontent.com"
	googleTitle    = "Sign in - Google Accounts"
	chooserPage    = "https://accounts.google.com/v3/signin/accountchooser?client_id=REDACTED&redirect_uri=https%3A%2F%2F" + googleCallback +
		"%2Fapi%2Fauth%2Fgoogle%2Fhandler%2Fframe&response_type=code&scope=openid+profile+email&service=lso&flowName=GeneralOAuthFlow"
	thirdChooser = "https://accounts.google.com/v3/signin/accountchooser?client_id=REDACTED&redirect_uri=https%3A%2F%2Fapp.example.net" +
		"%2Foauth%2Fcallback&response_type=code&service=lso&flowName=GeneralOAuthFlow"
	signInToPage  = "https://accounts.google.com/signin/oauth/id?authuser=0&part=p&flowName=GeneralOAuthFlow&as=a&client_id=REDACTED&rapt=r&requestPath=/signin/oauth/v3/consent"
	scopesPage    = "https://accounts.google.com/signin/oauth/v3/consent?as=a&authuser=0&client_id=REDACTED&flowName=GeneralOAuthFlow&hl=en&part=p&rapt=r&xsrf=REDACTED"
	warningPage   = "https://accounts.google.com/signin/oauth/warning?authuser=0&part=p&flowName=GeneralOAuthFlow&as=a&client_id=REDACTED&rapt=r"
	challengePage = "https://accounts.google.com/v3/signin/challenge/sk/webauthn?TL=t&authuser=0&continue=https%3A%2F%2Faccounts.google.com%2Fo%2Foauth2%2Fv2%2Fauth" +
		"%3Fclient_id%3Dx%26redirect_uri%3Dhttps%253A%252F%252F" + googleCallback + "%252Fcb&flowName=GlifWebSignIn"
	continueClick = "Clicks Continue"
)

var googleRecord = []App{{Name: googleApp, Google: true, Callbacks: []string{googleCallback}, Declared: "the person's word (a Google OAuth client)",
	Word: "sign in to my portal", By: "the guide", At: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)}}

// On Google's sign-in, the flow of a Google App on record is allowed by its
// callback host, from the page's own request or the tab's flow; a consent
// page of a flow for any other callback host is refused naming the form, a
// third party's; a flow whose callback host is unknown is decided as a
// consent page elsewhere; a GitHub record of the same callback covers no
// Google page, nor a Google record GitHub's consent page.
func TestGoogleConsent(t *testing.T) {
	github := []App{{Name: googleApp, ClientID: ownClientID, Callbacks: []string{googleCallback}, Declared: ownDeclared}}
	otherID := strings.Replace(signInToPage, "client_id=REDACTED", "client_id=9999-zzz.apps.googleusercontent.com", 1)
	for name, tc := range map[string]struct {
		page     Page
		apps     []App
		words    string
		decision string
		reason   string
	}{
		"own App's account chooser":             {Page{URL: chooserPage, Title: googleTitle}, googleRecord, "Clicks the account", Allow, "the person's own App " + googleApp + " (callback " + googleCallback + "), on record since 2026-10-16 by the guide: sign in to my portal"},
		"own App's Sign in to, by its flow":     {Page{URL: signInToPage, Title: googleTitle, Flow: googleCallback}, googleRecord, continueClick, Allow, googleApp},
		"own App's consent of scopes":           {Page{URL: scopesPage, Title: googleTitle, Flow: googleCallback}, googleRecord, continueClick, Allow, googleApp},
		"own App's unverified-app warning":      {Page{URL: warningPage, Title: googleTitle, Flow: googleCallback}, googleRecord, continueClick, Allow, googleApp},
		"own App's challenge, by continue":      {Page{URL: challengePage, Title: googleTitle}, googleRecord, "Clicks Try again", Allow, googleApp},
		"own App by its unredacted client id":   {Page{URL: strings.Replace(signInToPage, "REDACTED", googleClientID, 1), Title: googleTitle, Flow: googleCallback}, []App{{Name: googleApp, Google: true, ClientID: googleClientID, Callbacks: []string{googleCallback}}}, continueClick, Allow, googleApp},
		"another client id of the own callback": {Page{URL: otherID, Title: googleTitle, Flow: googleCallback}, []App{{Name: googleApp, Google: true, ClientID: googleClientID, Callbacks: []string{googleCallback}}}, continueClick, Deny, "callback is " + googleCallback},
		"a third party's Sign in to":            {Page{URL: signInToPage, Title: googleTitle, Flow: "app.example.net"}, googleRecord, continueClick, Deny, "of an App whose callback is app.example.net, which beekeeper has no record of as the person's own"},
		"a third party's consent of scopes":     {Page{URL: scopesPage, Title: googleTitle, Flow: "app.example.net"}, googleRecord, continueClick, Deny, GoogleForm},
		"a third party's consent, no record":    {Page{URL: scopesPage, Title: googleTitle, Flow: googleCallback}, nil, continueClick, Deny, GoogleForm},
		"a third party's account chooser":       {Page{URL: thirdChooser, Title: googleTitle}, googleRecord, "Clicks the account", "", ""},
		"a GitHub record of the same callback":  {Page{URL: scopesPage, Title: googleTitle, Flow: googleCallback}, github, continueClick, Deny, GoogleForm},
		"Sign in to of an unknown flow":         {Page{URL: signInToPage, Title: googleTitle}, googleRecord, continueClick, "", ""},
		"Allow on an unknown flow":              {Page{URL: signInToPage, Title: googleTitle}, googleRecord, "Clicks Allow", Deny, "is a consent page (an OAuth grant) on accounts.google.com"},
		"consent of scopes of an unknown flow":  {Page{URL: scopesPage, Title: googleTitle}, googleRecord, continueClick, Deny, "/signin/oauth/v3/consent is a consent page"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Click(tc.apps, tc.page, tc.words)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Click = %+v, want %q with %q", d, tc.decision, tc.reason)
			}
		})
	}
	// GitHub's consent page of the same callback host is no Google record's.
	if d := Click(googleRecord, Page{URL: strings.ReplaceAll(ownPage, ownCallback, googleCallback), Title: ownTitle}, clicksAuthorize); d.Decision != Deny {
		t.Errorf("GitHub's consent page under a Google record: %+v, want refused", d)
	}
}

// googleTranscript writes a browse turn's transcript whose tool results show
// tab 7 on each of urls in turn.
func googleTranscript(t *testing.T, urls ...string) string {
	t.Helper()
	var lines []string
	for _, u := range urls {
		lines = append(lines, resultLine(strconv.Quote(`Clicked. Tab Context: - Available tabs:   • tabId 7: "`+googleTitle+`" ("`+u+`")`)))
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The tab's flow on Google's sign-in carries the callback host of the
// account chooser it came from; a page off Google's host ends the flow, and
// a later chooser of another request starts a new one.
func TestGoogleFlow(t *testing.T) {
	for name, tc := range map[string]struct {
		urls []string
		flow string
	}{
		"chooser, then Sign in to":         {[]string{chooserPage, signInToPage}, googleCallback},
		"chooser, Sign in to, then scopes": {[]string{chooserPage, signInToPage, scopesPage}, googleCallback},
		"Sign in to without a chooser":     {[]string{signInToPage}, ""},
		"a page elsewhere ends the flow":   {[]string{chooserPage, "https://" + googleCallback + "/", signInToPage}, ""},
		"another request starts a new one": {[]string{chooserPage, signInToPage, thirdChooser, scopesPage}, "app.example.net"},
	} {
		t.Run(name, func(t *testing.T) {
			p, ok := lastPage(googleTranscript(t, tc.urls...), tab7)
			if !ok || p.Flow != tc.flow || p.URL != tc.urls[len(tc.urls)-1] {
				t.Errorf("lastPage = %+v, %v; want flow %q", p, ok, tc.flow)
			}
		})
	}
}

// The hook answers a click on Google's Sign in to page of the own App from
// the tab's flow in the transcript, and refuses the consent of a third
// party's.
func TestGoogleConsentDecision(t *testing.T) {
	click := tabInput(leftClick, map[string]any{"ref": "ref_9", "action_summary": continueClick})
	for name, tc := range map[string]struct {
		urls     []string
		decision string
	}{
		"own App":     {[]string{chooserPage, signInToPage}, decisionAllow},
		"third party": {[]string{thirdChooser, signInToPage, scopesPage}, decisionDeny},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeComputer, click)
			ev["permission_mode"] = "dontAsk"
			ev[sessionKey] = browseTurn
			ev["transcript_path"] = googleTranscript(t, tc.urls...)
			d := decideEvent(t, Hook{Apps: func() []App { return googleRecord }}, ev)
			if d == nil || d.PermissionDecision != tc.decision {
				t.Errorf("decided %+v, want %s", d, tc.decision)
			}
		})
	}
}
