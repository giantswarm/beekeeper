package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// A third party's OAuth client is one neither the organisation nor the
// person owns: a vendor's public client the person signs in to with their
// own account there (OpenAI's Codex CLI, through which a person's ChatGPT
// subscription signs in). Its consent page is on the vendor's own sign-in
// host, at a path of that client alone, and carries no callback of the
// person's: the hook knows each such page (ThirdPartyClients) and decides a
// call on it from the third-party consents on record (App.OAuth), on the
// person's word alone: allowed for the client and account on record, the
// account named in the call's own words; refused for a record of no such
// client or another account. A client signed in by a device code has a
// second page of the same grant, where the code is entered: the record
// admits it alike, and only a device code is typed there (typedDeviceCode).
// Any other page on the host is decided as a consent page elsewhere.

// ThirdPartyClient is a third party's OAuth client whose consent page the
// hook knows: the sign-in host, the client id, the path of its consent page
// and of its device-code entry page ("" for a client with none).
type ThirdPartyClient struct {
	Host, ClientID, Consent, DeviceCode, Name string
}

// ThirdPartyClients are the third parties' clients a consent is recorded
// for: a client not here has no consent page the hook knows, so none is
// recorded.
var ThirdPartyClients = []ThirdPartyClient{
	{Host: "auth.openai.com", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Consent: "/sign-in-with-chatgpt/codex/consent", DeviceCode: "/deviceauth/callback", Name: "OpenAI's Codex sign-in with ChatGPT"},
}

// OAuthForm is how a consent to a third party's OAuth client, as the
// person's own account there, is recorded.
const OAuthForm = "`beekeeper app allow <name> --oauth <host> --client-id <id> --account <address> --word \"<the person's words>\"`"

// ThirdPartyClientOf is the known client of id on host; false for none.
func ThirdPartyClientOf(host, id string) (ThirdPartyClient, bool) {
	i := slices.IndexFunc(ThirdPartyClients, func(c ThirdPartyClient) bool {
		return strings.EqualFold(c.Host, host) && c.ClientID == id
	})
	if i < 0 {
		return ThirdPartyClient{}, false
	}
	return ThirdPartyClients[i], true
}

// thirdPartyHost reports whether host is a known client's sign-in host.
func thirdPartyHost(host string) bool {
	return slices.ContainsFunc(ThirdPartyClients, func(c ThirdPartyClient) bool { return strings.EqualFold(c.Host, host) })
}

// devicePage names a client's device-code entry page.
const devicePage = "the device-code page"

// thirdPartyPage is the known client whose consent or device-code entry
// page u is, and the page's name; false for any other page.
func thirdPartyPage(u *url.URL) (ThirdPartyClient, string, bool) {
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(strings.ToLower(u.Path), "/")
	for _, c := range ThirdPartyClients {
		switch {
		case !strings.EqualFold(c.Host, host):
		case c.Consent == path:
			return c, "the consent page", true
		case c.DeviceCode != "" && c.DeviceCode == path:
			return c, devicePage, true
		}
	}
	return ThirdPartyClient{}, "", false
}

// thirdPartyClick decides a call in words on page, at u on a known
// client's sign-in host: on the consent or device-code page of a client,
// allowed for the record of that client whose account the words name,
// refused otherwise; any other page there is decided as a consent page
// elsewhere.
func thirdPartyClick(apps []App, u *url.URL, page Page, words string) Decision {
	c, which, ok := thirdPartyPage(u)
	if !ok {
		return elsewhere(u, page.Title, words)
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(strings.ToLower(u.Path), "/")
	// The page's URL names the client where it carries one unredacted.
	if id := u.Query().Get("client_id"); id != "" && id != chromeRedacted && id != c.ClientID {
		return Decision{Deny, fmt.Sprintf("Refused: %s on %s carries the client id %s, not %s's (%s): a consent to it is on no record; "+
			"every other grant stays refused.", path, host, id, c.Name, c.ClientID)}
	}
	lower := strings.ToLower(words)
	var accounts []string
	for _, a := range apps {
		if !strings.EqualFold(a.OAuth, host) || a.ClientID != c.ClientID || a.Account == "" {
			continue
		}
		if strings.Contains(lower, a.Account) {
			return Decision{Allow, fmt.Sprintf("beekeeper allows it: %s of %s (client %s on %s) as %s, on record as %s since %s by %s: %s",
				which, c.Name, c.ClientID, host, a.Account, a.Name, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
		}
		accounts = append(accounts, a.Account)
	}
	if len(accounts) > 0 {
		return Decision{Deny, fmt.Sprintf("Refused: on %s of %s (client %s on %s) the record names the account the consent is given as, %s, "+
			"and this call's own words do not name it: act there only for that account, named in the call's summary.",
			which, c.Name, c.ClientID, host, strings.Join(accounts, " or "))}
	}
	return Decision{Deny, fmt.Sprintf("Refused: %s on %s is %s of %s (client %s), a third party's OAuth client beekeeper has no "+
		"consent on record for. A consent is a grant of the person's identity, answered only on their word for the account they named, which %s "+
		"records; every other grant stays refused.", path, host, which, c.Name, c.ClientID, OAuthForm)}
}

// deviceCode is what is typed on a device-code entry page: the code, whole
// or a part of it in one of its boxes (letters and digits, at most one
// dash), or a key that moves between the boxes or submits them (Enter,
// Tab, BackSpace), all nine characters at most.
var deviceCode = regexp.MustCompile(`^[A-Za-z0-9]{1,8}-?[A-Za-z0-9]{0,8}$`)

// typedDeviceCode decides text typed or set on page, which d allowed: on a
// known client's device-code page only a device code is typed (deviceCode
// bounds it to nine letters or digits); d stands for any other page.
func typedDeviceCode(page Page, text string, d Decision) Decision {
	u, err := url.Parse(strings.TrimSpace(page.URL))
	if err != nil {
		return d
	}
	c, which, ok := thirdPartyPage(u)
	if !ok || which != devicePage {
		return d
	}
	t := strings.TrimSpace(text)
	if deviceCode.MatchString(t) && len(strings.ReplaceAll(t, "-", "")) <= 9 {
		return d
	}
	return Decision{Deny, fmt.Sprintf("Refused: on the device-code page of %s (%s%s) only the device code is typed, "+
		"and this input of %d characters is none: no credential or other text is entered there.", c.Name, c.Host, c.DeviceCode, len(text))}
}
