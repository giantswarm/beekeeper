package guard

import (
	"strings"
	"testing"
)

// A consent to a third party's OAuth client on record is answered on that
// client's consent page alone, for a call whose own words name the
// account on record: another account, a call naming none, a record of
// another account or no record, another client id the URL carries and the
// page on another host are refused; another page on the client's host is a
// consent page elsewhere, and a page there that is none gets no decision.
func TestThirdPartyConsent(t *testing.T) {
	const (
		codex   = "app_EMoamEEZ73f0CkXaXp7hrann"
		openai  = "auth.openai.com"
		unnamed = "do not name it"
		account = "person@example.com"
		page    = "https://auth.openai.com/sign-in-with-chatgpt/codex/consent"
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
		"another client id in the URL":      {[]App{rec}, page + "?client_id=app_other", words, Deny, "app_other"},
		"another client's consent page":     {[]App{rec}, "https://auth.openai.com/sign-in-with-chatgpt/other/consent", words, Deny, "consent page"},
		"another host":                      {[]App{rec}, "https://auth.example.net/sign-in-with-chatgpt/codex/consent", words, Deny, "consent page"},
		"its host's authorize page granted": {[]App{rec}, "https://auth.openai.com/oauth/authorize?client_id=" + codex + "&response_type=code", "Clicks Allow on the authorize page", Deny, ""},
		"its host's device page":            {[]App{rec}, "https://auth.openai.com/codex/device", "Types the code", "", ""},
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
	apps := []App{{Name: "x", OAuth: "auth.openai.com", ClientID: "app_1", Callbacks: []string{"cb.example.org"}, Account: "person@example.com"}}
	page := "https://github.com/login/oauth/authorize?client_id=app_1&redirect_uri=https%3A%2F%2Fcb.example.org%2Fcb"
	if d := Click(apps, Page{URL: page, Title: "Authorize x"}, "Clicks Authorize"); d.Decision != Deny {
		t.Errorf("GitHub's consent page = %+v, want a refusal", d)
	}
}

// Only a client the hook knows the consent page of is one a consent is
// recorded for.
func TestThirdPartyClientOf(t *testing.T) {
	if c, ok := ThirdPartyClientOf("Auth.OpenAI.com", "app_EMoamEEZ73f0CkXaXp7hrann"); !ok || c.Consent != "/sign-in-with-chatgpt/codex/consent" {
		t.Errorf("Codex = %+v, %v", c, ok)
	}
	for _, x := range [][2]string{{"auth.openai.com", "app_other"}, {"auth.example.net", "app_EMoamEEZ73f0CkXaXp7hrann"}} {
		if _, ok := ThirdPartyClientOf(x[0], x[1]); ok {
			t.Errorf("ThirdPartyClientOf(%q, %q) known", x[0], x[1])
		}
	}
}
