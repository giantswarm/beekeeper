package guard

import (
	"fmt"
	"net/url"
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
// client or another account. Any other page on the host is decided as a
// consent page elsewhere.

// ThirdPartyClient is a third party's OAuth client whose consent page the
// hook knows: the sign-in host, the client id, the path of its consent page.
type ThirdPartyClient struct {
	Host, ClientID, Consent, Name string
}

// ThirdPartyClients are the third parties' clients a consent is recorded
// for: a client not here has no consent page the hook knows, so none is
// recorded.
var ThirdPartyClients = []ThirdPartyClient{
	{Host: "auth.openai.com", ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Consent: "/sign-in-with-chatgpt/codex/consent", Name: "OpenAI's Codex sign-in with ChatGPT"},
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

// thirdPartyClick decides a call in words on page, at u on a known
// client's sign-in host: on the consent page of a client, allowed for the
// record of that client whose account the words name, refused otherwise;
// any other page there is decided as a consent page elsewhere.
func thirdPartyClick(apps []App, u *url.URL, page Page, words string) Decision {
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(strings.ToLower(u.Path), "/")
	i := slices.IndexFunc(ThirdPartyClients, func(c ThirdPartyClient) bool {
		return strings.EqualFold(c.Host, host) && c.Consent == path
	})
	if i < 0 {
		return elsewhere(u, page.Title, words)
	}
	c := ThirdPartyClients[i]
	// The page's URL names the client where it carries one unredacted.
	if id := u.Query().Get("client_id"); id != "" && id != chromeRedacted && id != c.ClientID {
		return Decision{Deny, fmt.Sprintf("Refused: %s on %s carries the client id %s, not %s's (%s): a consent to it is on no record; "+
			"every other grant stays refused.", c.Consent, host, id, c.Name, c.ClientID)}
	}
	lower := strings.ToLower(words)
	var accounts []string
	for _, a := range apps {
		if !strings.EqualFold(a.OAuth, host) || a.ClientID != c.ClientID || a.Account == "" {
			continue
		}
		if strings.Contains(lower, a.Account) {
			return Decision{Allow, fmt.Sprintf("beekeeper allows it: the consent page of %s (client %s on %s) as %s, on record as %s since %s by %s: %s",
				c.Name, c.ClientID, host, a.Account, a.Name, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
		}
		accounts = append(accounts, a.Account)
	}
	if len(accounts) > 0 {
		return Decision{Deny, fmt.Sprintf("Refused: on the consent page of %s (client %s on %s) the record names the account the consent is given as, %s, "+
			"and this call's own words do not name it: click Continue only for that account, named in the call's summary.",
			c.Name, c.ClientID, host, strings.Join(accounts, " or "))}
	}
	return Decision{Deny, fmt.Sprintf("Refused: %s on %s is the consent page of %s (client %s), a third party's OAuth client beekeeper has no "+
		"consent on record for. A consent is a grant of the person's identity, answered only on their word for the account they named, which %s "+
		"records; every other grant stays refused.", c.Consent, host, c.Name, c.ClientID, OAuthForm)}
}
