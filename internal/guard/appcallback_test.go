package guard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The App on record for one callback URL and its settings page under the
// organisation's settings.
const (
	callbackApp     = "Example Lab Agents"
	callbackSlug    = "example-lab-agents"
	callbackURL     = "https://agents.example.org/callback/github"
	callbackDecl    = "example/github@0123456789ab:apps/" + callbackSlug + "/manifest.json"
	settingsPage    = "https://github.com/organizations/" + deviceOrg + "/settings/apps/" + callbackSlug
	notTheCallback  = "neither the one callback URL"
	addsCallbackURL = "Clicks Add Callback URL for " + callbackApp
	byGuide         = "the guide"
	notTypedURL     = "is not the one callback URL on record"
)

var callbackRecorded = []App{{Name: callbackApp, Org: deviceOrg, CallbackURL: callbackURL, Declared: callbackDecl,
	Word: "add the agents callback URL", By: byGuide, At: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}}

// A navigation to the settings page of the App on record for a callback
// URL is allowed with the record; without one, and to another App's
// settings page, the hook gives no decision, as before.
func TestCallbackNavigation(t *testing.T) {
	for name, tc := range map[string]struct {
		url      string
		apps     []App
		decision string
		reason   string
	}{
		"the recorded App's settings page": {settingsPage, callbackRecorded, Allow, "the settings page of the person's own App " + callbackApp + ", declared in " + callbackDecl + ", on record for the callback URL " + callbackURL + " since 2026-10-20 by the guide"},
		"with a trailing slash":            {settingsPage + "/", callbackRecorded, Allow, callbackApp},
		"another App's settings page":      {strings.Replace(settingsPage, callbackSlug, "other-app", 1), callbackRecorded, "", ""},
		"another organisation's":           {strings.Replace(settingsPage, "/"+deviceOrg+"/", "/other/", 1), callbackRecorded, "", ""},
		"a permission record only":         {"https://github.com/organizations/" + deviceOrg + "/settings/apps/" + deviceSlug, changeRecorded, "", ""},
		"its permissions page":             {settingsPage + "/permissions", callbackRecorded, Deny, PermissionForm},
	} {
		t.Run(name, func(t *testing.T) {
			d := Navigation(tc.apps, tc.url)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Navigation = %+v, want %q with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// On the recorded App's settings page the calls of adding the URL are
// allowed: Add Callback URL, a call naming the URL (spelt with another case
// or a trailing slash too), Save changes; a call that names another URL or
// another part of the page, removes anything or names none of them is
// refused. Another App's
// settings page is left to the classifier.
func TestCallbackClick(t *testing.T) {
	page := Page{URL: settingsPage, Title: "GitHub Apps - " + callbackApp}
	for name, tc := range map[string]struct {
		page     Page
		words    string
		decision string
		reason   string
	}{
		"add callback URL":       {page, addsCallbackURL, Allow, "the settings page of the person's own App " + callbackApp},
		"the new field, named":   {page, "Clicks the new callback field for " + callbackURL, Allow, callbackApp},
		"the URL, no callback":   {page, "Clicks the Homepage field for " + callbackURL, Deny, notTheCallback},
		"a callback field":       {page, "Clicks the new Callback URL field", Deny, notTheCallback},
		"the private key":        {page, "Clicks Generate a private key for " + callbackURL, Deny, notTheCallback},
		"types the URL":          {page, "Types the callback URL " + callbackURL, Allow, callbackApp},
		"the URL respelt":        {page, "Callback URL HTTPS://Agents.Example.org/callback/github/", Allow, callbackApp},
		"saves":                  {page, "Clicks Save changes", Allow, callbackApp},
		"another URL":            {page, "Types the callback URL https://evil.example.net/callback", Deny, notTheCallback + " on record (" + callbackURL + ")"},
		"the webhook URL":        {page, "Types https://agents.example.org/webhook into the Webhook URL field", Deny, notTheCallback},
		"removes a callback":     {page, "Clicks Remove for the callback URL https://old.example.org/callback", Deny, notTheCallback},
		"deletes the recorded":   {page, "Clicks Delete for the callback URL " + callbackURL, Deny, notTheCallback},
		"another setting":        {page, "Unchecks Active for the webhook", Deny, notTheCallback},
		"nothing said":           {page, "", Deny, notTheCallback},
		"another App's settings": {Page{URL: strings.Replace(settingsPage, callbackSlug, "other-app", 1)}, addsCallbackURL, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := Click(callbackRecorded, tc.page, tc.words)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Click = %+v, want %q with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// The hook decides a browse turn's calls on the settings page from the
// record: the navigation, exactly the URL typed or set are allowed; other
// input, a key, a respelt URL and a batch with another URL are refused.
func TestCallbackDecision(t *testing.T) {
	apps := func() []App { return callbackRecorded }
	own := transcriptWith(t, Page{URL: settingsPage, Title: "GitHub Apps - " + callbackApp}, false)
	types := func(text string) map[string]any {
		return tabInput("type", map[string]any{textKey: text, summaryKey: "Types the callback URL"})
	}
	for name, tc := range map[string]struct {
		tool     string
		input    map[string]any
		decision string
		reason   string
	}{
		"navigate to the settings page": {browserTools + chromeNavigation, map[string]any{urlKey: settingsPage}, decisionAllow, callbackURL},
		"types the recorded URL":        {browserTools + chromeComputer, types(callbackURL), decisionAllow, callbackApp},
		"types another URL":             {browserTools + chromeComputer, types("https://evil.example.net/"), decisionDeny, notTheCallback},
		"types other text, URL named":   {browserTools + chromeComputer, tabInput("type", map[string]any{textKey: "hello", summaryKey: "Types " + callbackURL}), decisionDeny, notTypedURL},
		"types the URL respelt":         {browserTools + chromeComputer, types(callbackURL + "/"), decisionDeny, notTypedURL},
		"presses a key":                 {browserTools + chromeComputer, tabInput("key", map[string]any{textKey: "Enter", summaryKey: "Presses Enter to add " + callbackURL}), decisionDeny, notTypedURL},
		"the URL as a form value":       {browserTools + chromeForm, map[string]any{refKey: refTwelve, valueKey: callbackURL, summaryKey: "Sets the callback URL", tabKey: json.Number(tab7)}, decisionAllow, callbackApp},
		"a batch with another URL": {browserTools + chromeBatch, map[string]any{actionsKey: []any{
			batchAction(chromeComputer, tabInput(leftClick, map[string]any{summaryKey: addsCallbackURL})),
			batchAction(chromeComputer, types("https://evil.example.net/"))}}, decisionDeny, notTheCallback},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(tc.tool, tc.input)
			ev["permission_mode"] = dontAsk
			ev[sessionKey] = browseTurn
			ev["transcript_path"] = own
			d := decideEvent(t, Hook{Apps: apps}, ev)
			if d == nil || d.PermissionDecision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("decided %+v, want %s with %q", d, tc.decision, tc.reason)
			}
		})
	}
}
