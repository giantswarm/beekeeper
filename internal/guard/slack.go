package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A Slack app's install consent is Slack's page "Allow <app> to access
// Slack" on the workspace's own host (<workspace>.slack.com/oauth): its URL
// carries the app's client id and the scopes asked for, and an app with the
// incoming-webhook scope asks for the channel its webhook posts to ("Channel
// for webhook"), without which Allow stays disabled. A re-install that adds a
// scope is the same page. The hook decides a call on it from the Slack apps
// on record (App.Slack), on the person's word: for the app whose client id
// the URL carries on the recorded workspace (the app's name, which its owner
// chooses, identifies nothing; a redacted id is refused), typing a part of the
// recorded webhook channel's name, the picker's keys and its clicks are
// allowed, a call that names another channel is refused, and Allow, or any
// click that does not name the channel field, is allowed only once a call on
// the tab named the recorded channel (Page.Picked), so the install keeps it;
// the page of any other app or workspace is refused, and so is an app
// without a channel on record unless the page's scopes, as parsed, ask for
// no webhook. On the app's Install App page
// (api.slack.com/apps/<id>/install-on-team) the hook allows the recorded
// app's Install or Reinstall click, which opens that page, and refuses
// anything typed there and a click that names a token, a revoke or a copy.

// SlackForm is how the install consent of a Slack app the person owns and
// asked for is recorded.
const SlackForm = "`beekeeper app allow <name> --slack <workspace>.slack.com --app-id <id> --client-id <id> --webhook-channel <#channel> --word \"<the person's words>\"`"

// slackDomain is the domain every Slack workspace's host is under.
const slackDomain = ".slack.com"

// slackConsentPaths are the paths of Slack's install consent page on a
// workspace's host.
var slackConsentPaths = []string{"/oauth", "/oauth/authorize", "/oauth/v2/authorize"}

// slackInstallPage is the Install App page of a Slack app, under its
// settings on api.slack.com or app.slack.com, its app id the first group.
var slackInstallPage = regexp.MustCompile(`^/(?:apps|app-settings/T[A-Z0-9]+)/(A[A-Z0-9]+)/install-on-team/?$`)

// slackInstallWords name the Install App page's click that opens the
// consent: Install to <workspace>, Reinstall to <workspace>.
var slackInstallWords = regexp.MustCompile(`(?i)\b(re-?)?install`)

// slackInstallOther names the Install App page's other controls: its
// tokens, their revoke, regeneration and copy.
var slackInstallOther = regexp.MustCompile(`(?i)token|revoke|regenerat|copy|delete|remove|uninstall`)

// slackChannelNamed is a channel a call's words name, "#name".
var slackChannelNamed = regexp.MustCompile(`#([A-Za-z0-9][A-Za-z0-9._-]*)`)

// slackFieldWords name the webhook channel's field: a click that opens the
// picker or chooses in it.
var slackFieldWords = regexp.MustCompile(`(?i)channel|webhook|picker`)

// slackKeys are the keys pressed in the channel picker: moving through its
// options, choosing one, closing it.
var slackKeys = strings.Fields("return enter tab down up arrowdown arrowup escape esc backspace")

// slackAllowWords name the page's Allow.
var slackAllowWords = regexp.MustCompile(`(?i)\ballow\b`)

// SlackChannel is a webhook channel's name as a record keeps it: lower case,
// without its "#"; "" for a name Slack gives no channel.
func SlackChannel(name string) string {
	c := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	if !slackChannelName.MatchString(c) {
		return ""
	}
	return c
}

// slackChannelName is a Slack channel's name: lower-case letters, digits,
// hyphens, underscores and dots, at most 80.
var slackChannelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)

// slackHost reports whether host is a Slack workspace's or Slack's own.
func slackHost(host string) bool {
	return host == "slack.com" || strings.HasSuffix(host, slackDomain)
}

// SlackConsentURL reports whether rawURL is Slack's install consent page.
func SlackConsentURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && slackConsentPage(u)
}

// slackConsentPage reports whether u is Slack's install consent page.
func slackConsentPage(u *url.URL) bool {
	return slackHost(strings.ToLower(u.Hostname())) && slices.Contains(slackConsentPaths, strings.TrimSuffix(strings.ToLower(u.Path), "/"))
}

// slackClick decides a call in words on page, at u on a Slack host: Slack's
// install consent page and a Slack app's Install App page; no decision for
// any other page there, which elsewhere decides.
func slackClick(apps []App, u *url.URL, page Page, words string) (Decision, bool) {
	if slackConsentPage(u) {
		return slackConsent(apps, u, page, words), true
	}
	a := slackInstallRecord(apps, u)
	if a == nil || !slackInstallWords.MatchString(words) {
		return Decision{}, false
	}
	if slackInstallOther.MatchString(words) {
		return Decision{Deny, fmt.Sprintf("Refused: on the Install App page of %s only Install or Reinstall is on record; the call %q names "+
			"another control there (a token, its revoke or copy, an uninstall).", slackWords(*a), short(words))}, true
	}
	return Decision{Allow, "beekeeper allows the click: the Install App page of " + slackWords(*a)}, true
}

// slackInstallRecord is the Slack app on record whose Install App page u
// is, nil for none.
func slackInstallRecord(apps []App, u *url.URL) *App {
	host := strings.ToLower(u.Hostname())
	m := slackInstallPage.FindStringSubmatch(u.Path)
	if m == nil || (host != "api.slack.com" && host != "app.slack.com") {
		return nil
	}
	for i := range apps {
		if a := &apps[i]; a.Slack != "" && a.SlackApp == m[1] {
			return a
		}
	}
	return nil
}

// slackConsent decides a call in words on Slack's install consent page u,
// the tab's page.
func slackConsent(apps []App, u *url.URL, page Page, words string) Decision {
	host := strings.ToLower(u.Hostname())
	q := u.Query()
	clientID := q.Get("client_id")
	a := slackRecord(apps, host, clientID)
	if a == nil {
		which := "of client id " + clientID
		if clientID == "" || clientID == chromeRedacted {
			which = strconv.Quote(page.Title) + " (its client id redacted or absent: the hook answers only the client id the URL carries)"
		}
		return Decision{Deny, fmt.Sprintf("Refused: Slack's install consent page %s on %s names a Slack app beekeeper has no record of on that workspace. "+
			"A consent is a grant of the person's identity, answered only for an app the person owns and asked for, which %s "+
			"records on their word; every other grant stays refused.", which, host, SlackForm)}
	}
	scopes := slackScopes(q)
	if a.Channel == "" && (len(scopes) == 0 || slices.Contains(scopes, "incoming-webhook")) {
		return Decision{Deny, fmt.Sprintf("Refused: Slack's install consent page of %s asks for a channel for its webhook (incoming-webhook), or its URL "+
			"names no scopes to tell, and the record names no channel to keep: record it with --webhook-channel <#channel>, the channel the app's "+
			"webhook posts to now (%s).", slackWords(*a), SlackForm)}
	}
	for _, m := range slackChannelNamed.FindAllStringSubmatch(words, -1) {
		if c := strings.ToLower(m[1]); c != a.Channel {
			return Decision{Deny, fmt.Sprintf("Refused: on Slack's install consent page of %s the call names the channel #%s, and the record keeps the webhook "+
				"channel #%s: choose only that one there.", slackWords(*a), c, a.Channel)}
		}
	}
	picking := !slackAllowWords.MatchString(words) && (slackFieldWords.MatchString(words) || slackChannelOf(words) == a.Channel)
	if a.Channel == "" || page.Picked == a.Channel || picking {
		return Decision{Allow, "beekeeper allows it: Slack's install consent page of " + slackWords(*a)}
	}
	return Decision{Deny, fmt.Sprintf("Refused: on Slack's install consent page of %s the webhook channel on record, #%s, is not chosen yet: choose it "+
		"in the \"Channel for webhook\" picker first, naming #%s in that call's summary, in a call of its own (not in a batch with Allow), then click Allow.",
		slackWords(*a), a.Channel, a.Channel)}
}

// slackRecord is the Slack app on record on workspace host whose client id
// is clientID; nil for none, a redacted id too.
func slackRecord(apps []App, host, clientID string) *App {
	for i := range apps {
		a := &apps[i]
		if a.Slack != "" && strings.EqualFold(a.Slack, host) && clientID != "" && clientID != chromeRedacted && clientID == a.ClientID {
			return a
		}
	}
	return nil
}

// slackScopes are the scopes an install URL asks for, bot and user, each
// token trimmed: split on commas, spaces and pluses.
func slackScopes(q url.Values) []string {
	var out []string
	for _, k := range []string{"scope", "user_scope"} {
		for _, v := range q[k] {
			out = append(out, strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '+' })...)
		}
	}
	return out
}

// typedSlack decides text typed or set on page, which d allowed: on Slack's
// install consent page only a part of the recorded webhook channel's name is
// typed, or a key of its picker; d stands for any other page.
func typedSlack(apps []App, page Page, text string, d Decision) Decision {
	u, err := url.Parse(strings.TrimSpace(page.URL))
	if err != nil {
		return d
	}
	if a := slackInstallRecord(apps, u); a != nil && d.Decision == Allow {
		return Decision{Deny, fmt.Sprintf("Refused: on the Install App page of %s only the Install or Reinstall click is on record; "+
			"nothing is typed or pressed there.", slackWords(*a))}
	}
	if !slackConsentPage(u) {
		return d
	}
	a := slackRecord(apps, strings.ToLower(u.Hostname()), u.Query().Get("client_id"))
	if a == nil {
		return d
	}
	t := strings.ToLower(strings.TrimSpace(text))
	if slices.Contains(slackKeys, t) {
		return d
	}
	if c := strings.TrimPrefix(t, "#"); a.Channel != "" && c != "" && strings.Contains(a.Channel, c) {
		return d
	}
	return Decision{Deny, fmt.Sprintf("Refused: on Slack's install consent page of %s only the webhook channel on record, #%s, is typed (a part of its name) "+
		"or a key of its picker, and this input of %d characters is neither: no credential or other text is entered there.", slackWords(*a), a.Channel, len(text))}
}

// slackChannelOf is the last channel a call's words name, lower case; ""
// for none.
func slackChannelOf(words string) string {
	m := slackChannelNamed.FindAllStringSubmatch(words, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.ToLower(m[len(m)-1][1])
}

// slackWords name the record of a Slack app: the app, its workspace, the
// webhook channel kept, and the person's word.
func slackWords(a App) string {
	channel := ""
	if a.Channel != "" {
		channel = ", webhook channel #" + a.Channel
	}
	return fmt.Sprintf("the person's own Slack app %s (%s on %s, client id %s%s), %s, on record since %s by %s: %s",
		a.Name, a.SlackApp, a.Slack, a.ClientID, channel, a.Declared, a.At.UTC().Format("2006-01-02"), a.By, a.Word)
}
