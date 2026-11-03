package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The client, its sign-in host and a client id of no record the tests use,
// and what a decision on the device-code page names.
const (
	codex       = "app_EMoamEEZ73f0CkXaXp7hrann"
	openai      = "auth.openai.com"
	otherClient = "app_other"
	onDevice    = "device-code page"
)

// A consent to a third party's OAuth client on record is answered on that
// client's consent page alone, for a call whose own words name the
// account on record, and so is its device-code page: another account, a call naming none, a record of
// another account or no record, another client id the URL carries and the
// page on another host are refused; another page on the client's host is a
// consent page elsewhere, and a page there that is none gets no decision.
func TestThirdPartyConsent(t *testing.T) {
	const (
		unnamed = "do not name it"
		account = "person@example.com"
		page    = "https://auth.openai.com/sign-in-with-chatgpt/codex/consent"
		device  = "https://auth.openai.com/deviceauth/callback"
		title   = "Sign in to Codex with ChatGPT"
		words   = "Clicks Continue to sign in to Codex as " + account
	)
	rec := App{Name: "openai-codex", OAuth: openai, ClientID: codex, Account: account, Declared: "the person's word", Word: "note: yes"}
	other := rec
	other.Account = "other@example.com"
	for name, tc := range map[string]struct {
		apps        []App
		url, words  string
		want, names string
	}{
		"its page, its account":             {[]App{rec}, page, words, Allow, "as " + account},
		"a trailing slash":                  {[]App{rec}, page + "/", words, Allow, ""},
		"the account in capitals":           {[]App{rec}, page, strings.ToUpper(words), Allow, ""},
		"its client id in the URL":          {[]App{rec}, page + "?client_id=" + codex, words, Allow, ""},
		"a redacted client id":              {[]App{rec}, page + "?client_id=REDACTED", words, Allow, ""},
		"another account":                   {[]App{rec}, page, "Clicks Continue as other@example.com", Deny, unnamed},
		"no account named":                  {[]App{rec}, page, "Clicks Continue", Deny, unnamed},
		"a record of another account":       {[]App{other}, page, words, Deny, unnamed},
		"no record":                         {nil, page, words, Deny, OAuthForm},
		"a Google record":                   {[]App{{Name: "g", Google: true, Callbacks: []string{openai}, Account: account}}, page, words, Deny, OAuthForm},
		"another client id in the URL":      {[]App{rec}, page + "?client_id=" + otherClient, words, Deny, otherClient},
		"another client's consent page":     {[]App{rec}, "https://auth.openai.com/sign-in-with-chatgpt/other/consent", words, Deny, "consent page"},
		"another host":                      {[]App{rec}, "https://auth.example.net/sign-in-with-chatgpt/codex/consent", words, Deny, "consent page"},
		"its host's authorize page granted": {[]App{rec}, "https://auth.openai.com/oauth/authorize?client_id=" + codex + "&response_type=code", "Clicks Allow on the authorize page", Deny, ""},
		"its host's device page":            {[]App{rec}, "https://auth.openai.com/codex/device", "Types the code", "", ""},
		"its device-code page, its account": {[]App{rec}, device, "Types the device code for " + account, Allow, onDevice},
		"its device-code page, client id":   {[]App{rec}, device + "?client_id=" + codex, "Clicks Continue for " + account, Allow, ""},
		"its device-code page, other id":    {[]App{rec}, device + "?client_id=" + otherClient, "Clicks Continue for " + account, Deny, otherClient},
		"its device-code page, no account":  {[]App{rec}, device, "Types the device code", Deny, unnamed},
		"its device-code page, other acct":  {[]App{other}, device, "Types the device code for " + account, Deny, unnamed},
		"its device-code page, no record":   {nil, device, "Types the device code for " + account, Deny, OAuthForm},
		"device-code path on another host":  {[]App{rec}, "https://auth.example.net/deviceauth/callback", "Types the device code for " + account, "", ""},
	} {
		d := Click(tc.apps, Page{URL: tc.url, Title: title}, tc.words)
		if d.Decision != tc.want || !strings.Contains(d.Reason, tc.names) {
			t.Errorf("%s: %+v, want %q naming %q", name, d, tc.want, tc.names)
		}
	}
}

// A record of a third party's consent is no GitHub App's: GitHub's consent
// page does not take its client id.
func TestThirdPartyNotGitHub(t *testing.T) {
	apps := []App{{Name: "x", OAuth: openai, ClientID: "app_1", Callbacks: []string{"cb.example.org"}, Account: "person@example.com"}}
	page := "https://github.com/login/oauth/authorize?client_id=app_1&redirect_uri=https%3A%2F%2Fcb.example.org%2Fcb"
	if d := Click(apps, Page{URL: page, Title: "Authorize x"}, "Clicks Authorize"); d.Decision != Deny {
		t.Errorf("GitHub's consent page = %+v, want a refusal", d)
	}
}

// Only a client the hook knows the consent page of is one a consent is
// recorded for.
func TestThirdPartyClientOf(t *testing.T) {
	if c, ok := ThirdPartyClientOf("Auth.OpenAI.com", codex); !ok || c.Consent != "/sign-in-with-chatgpt/codex/consent" {
		t.Errorf("Codex = %+v, %v", c, ok)
	}
	for _, x := range [][2]string{{"auth.openai.com", otherClient}, {"auth.example.net", "app_EMoamEEZ73f0CkXaXp7hrann"}} {
		if _, ok := ThirdPartyClientOf(x[0], x[1]); ok {
			t.Errorf("ThirdPartyClientOf(%q, %q) known", x[0], x[1])
		}
	}
}

// On a known client's device-code page only a device code, whole or a
// box's part, or a key between the boxes is typed: other text, a
// credential say, is refused, and any other page keeps its decision.
func TestTypedDeviceCode(t *testing.T) {
	allowed := Decision{Allow, "on record"}
	device := Page{URL: "https://auth.openai.com/deviceauth/callback"}
	for _, text := range []string{"ABCD-EFGHI", "abcd1234x", "A", keyEnter, "Tab", "BackSpace"} {
		if d := typedDeviceCode(device, text, allowed); d != allowed {
			t.Errorf("typed %q = %+v, want it allowed", text, d)
		}
	}
	for _, text := range []string{"hunter2!x", "person@example.com", "ABCDE-FGHIJ", "ABCD-EFG-HI", "a b", "", "ABCDEFGHIJ"} {
		if d := typedDeviceCode(device, text, allowed); d.Decision != Deny || strings.Contains(d.Reason, text+"\"") {
			t.Errorf("typed %q = %+v, want a refusal not quoting it", text, d)
		}
	}
	consent := Page{URL: "https://auth.openai.com/sign-in-with-chatgpt/codex/consent"}
	if d := typedDeviceCode(consent, "hunter2!x", allowed); d != allowed {
		t.Errorf("consent page = %+v, want it kept", d)
	}
}

// The hook decides a browse turn's calls on the device-code page from the
// record: the code typed and Continue, each naming the account, are
// allowed; another text typed or a call naming no account is refused.
func TestDeviceCodeDecision(t *testing.T) {
	const account = "person@example.com"
	apps := func() []App {
		return []App{{Name: "openai-codex", OAuth: openai, ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Account: account, Word: "note: yes"}}
	}
	own := transcriptWith(t, Page{URL: "https://auth.openai.com/deviceauth/callback", Title: "Use your device code to grant access to Codex CLI"}, false)
	types := func(text, summary string) map[string]any {
		return tabInput("type", map[string]any{textKey: text, summaryKey: summary})
	}
	for name, tc := range map[string]struct {
		tool     string
		input    map[string]any
		decision string
		reason   string
	}{
		"types the code":             {browserTools + chromeComputer, types("ABCD-EFGHI", "Types the device code for "+account), decisionAllow, onDevice},
		"clicks Continue":            {browserTools + chromeComputer, tabInput(leftClick, map[string]any{summaryKey: "Clicks Continue for " + account}), decisionAllow, onDevice},
		"the code as a form value":   {browserTools + chromeForm, map[string]any{refKey: refTwelve, valueKey: "ABCD", summaryKey: "Sets the first boxes for " + account, tabKey: json.Number(tab7)}, decisionAllow, onDevice},
		"types the code, no account": {browserTools + chromeComputer, types("ABCD-EFGHI", "Types the device code"), decisionDeny, account},
		"types a password":           {browserTools + chromeComputer, types("correct horse battery", "Types the password for "+account), decisionDeny, "only the device code"},
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
