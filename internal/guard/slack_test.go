package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The Slack app, its workspace and its install pages the tests use.
const (
	slackName     = "Swarmgeist"
	slackWS       = "gigantic.slack.com"
	slackAppID    = "A08A95JBNH1"
	slackClient   = "2171500629.8349188396579"
	slackChannel  = "team-bumblebee"
	slackTitle    = "Allow Swarmgeist to access Slack | Giant Swarm Slack"
	slackScopes   = "&scope=usergroups%3Aread%2Cchat%3Awrite%2Cincoming-webhook&user_scope=&redirect_uri=&state=&granular_bot_scope=1&install_redirect=install-on-team&original_team=T0251EQJH&team=1"
	slackInstall  = "https://" + slackWS + "/oauth?client_id=" + slackClient + slackScopes
	slackSettings = "https://api.slack.com/apps/" + slackAppID + "/install-on-team"
	slackAllow    = "Clicks Allow"
	notChosen     = "is not chosen yet"
	reinstall     = "Clicks Reinstall to Giant Swarm"
	otherChannel  = "general"
)

func slackRec() App {
	return App{Name: slackName, Slack: slackWS, SlackApp: slackAppID, ClientID: slackClient, Channel: slackChannel, Declared: "the person's word (a Slack app)", Word: "note: reinstall it"}
}

// On Slack's install page of a Slack app on record the webhook channel's
// picker and the recorded channel chosen there are allowed, and Allow once
// that channel is chosen; another channel, Allow before the choice, a page
// asking for a webhook of a record without a channel, another client id,
// another workspace and no record are refused, each naming why. The app's
// Install App page answers its Install or Reinstall.
func TestSlackConsent(t *testing.T) {
	rec := slackRec()
	bare := rec
	bare.Channel = ""
	const keep = "webhook channel #" + slackChannel
	for name, tc := range map[string]struct {
		apps         []App
		url, title   string
		picked       string
		words        string
		want, naming string
	}{
		"opens the picker":                   {[]App{rec}, slackInstall, slackTitle, "", "Opens the Channel for webhook picker", Allow, keep},
		"chooses its channel":                {[]App{rec}, slackInstall, slackTitle, "", "Chooses #team-bumblebee", Allow, keep},
		"Allow once chosen":                  {[]App{rec}, slackInstall, slackTitle, slackChannel, "Clicks Allow to reinstall Swarmgeist", Allow, "own Slack app Swarmgeist"},
		"Allow before the choice":            {[]App{rec}, slackInstall, slackTitle, "", "Clicks Allow to reinstall Swarmgeist", Deny, notChosen},
		"Allow naming the channel, unpicked": {[]App{rec}, slackInstall, slackTitle, "", "Clicks Allow keeping #team-bumblebee", Deny, notChosen},
		"another click before the choice":    {[]App{rec}, slackInstall, slackTitle, "", "Confirms the re-install", Deny, notChosen},
		"Allow after another pick":           {[]App{rec}, slackInstall, slackTitle, otherChannel, slackAllow, Deny, notChosen},
		"another channel":                    {[]App{rec}, slackInstall, slackTitle, "", "Chooses #general in the channel picker", Deny, "#general"},
		"Allow naming another channel":       {[]App{rec}, slackInstall, slackTitle, slackChannel, "Clicks Allow with #general", Deny, "#general"},
		"a webhook, no channel on record":    {[]App{bare}, slackInstall, slackTitle, "", slackAllow, Deny, "--webhook-channel"},
		"no webhook, no channel on record":   {[]App{bare}, strings.Replace(slackInstall, "%2Cincoming-webhook", "", 1), slackTitle, "", slackAllow, Allow, slackName},
		"a redacted client id, its title":    {[]App{rec}, strings.Replace(slackInstall, slackClient, chromeRedacted, 1), slackTitle, slackChannel, slackAllow, Allow, slackName},
		"a redacted client id, other title":  {[]App{rec}, strings.Replace(slackInstall, slackClient, chromeRedacted, 1), "Allow Other to access Slack", slackChannel, slackAllow, Deny, SlackForm},
		"another client id":                  {[]App{rec}, strings.Replace(slackInstall, slackClient, "1.2", 1), slackTitle, slackChannel, slackAllow, Deny, "of client id 1.2"},
		"another workspace":                  {[]App{rec}, strings.Replace(slackInstall, slackWS, "other.slack.com", 1), slackTitle, slackChannel, slackAllow, Deny, SlackForm},
		"no record":                          {nil, slackInstall, slackTitle, slackChannel, slackAllow, Deny, SlackForm},
		"the v2 authorize path":              {[]App{rec}, strings.Replace(slackInstall, "/oauth?", "/oauth/v2/authorize?", 1), slackTitle, slackChannel, slackAllow, Allow, slackName},
		"its Reinstall":                      {[]App{rec}, slackSettings, "Slack API: Applications", "", reinstall, Allow, "Install App page"},
		"its Reinstall under app-settings":   {[]App{rec}, "https://app.slack.com/app-settings/T0251EQJH/" + slackAppID + "/install-on-team", "", "", reinstall, Allow, "Install App page"},
		"another app's Reinstall":            {[]App{rec}, "https://api.slack.com/apps/A0OTHER123/install-on-team", "", "", reinstall, "", ""},
		"another click on its settings":      {[]App{rec}, slackSettings, "", "", "Clicks Revoke All OAuth Tokens", "", ""},
	} {
		d := Click(tc.apps, Page{URL: tc.url, Title: tc.title, Picked: tc.picked}, tc.words)
		if d.Decision != tc.want || !strings.Contains(d.Reason, tc.naming) {
			t.Errorf("%s: %+v, want %q naming %q", name, d, tc.want, tc.naming)
		}
	}
}

// On Slack's install page of an app on record only a part of the recorded
// channel's name, with or without its "#", and the picker's keys are typed;
// other text is refused, and other pages keep their decision.
func TestTypedSlack(t *testing.T) {
	apps := []App{slackRec()}
	page := Page{URL: slackInstall, Title: slackTitle}
	ok := Decision{Allow, "ok"}
	for text, want := range map[string]string{
		"team-bumb": Allow, "#team-bumblebee": Allow, keyEnter: Allow, "ArrowDown": Allow,
		otherChannel: Deny, "hunter2": Deny, "": Deny,
	} {
		if d := typedSlack(apps, page, text, ok); d.Decision != want {
			t.Errorf("typed %q: %+v, want %s", text, d, want)
		}
	}
	if d := typedSlack(apps, Page{URL: slackSettings}, "general", ok); d != ok {
		t.Errorf("typed on its settings page: %+v, want the decision kept", d)
	}
}

// The channel a call on the tab chose on Slack's install page is the last
// one a call executed there named; a call on another URL of the tab names
// none on it.
func TestSlackPickedFromTheTranscript(t *testing.T) {
	use := func(id string, input map[string]any) string {
		in, _ := json.Marshal(input)
		return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"mcp__claude-in-chrome__computer","input":` + string(in) + `}]}}`
	}
	result := func(id, url string) string {
		r := resultLine(`[{"type":"text","text":"Clicked"},{"type":"text","text":` +
			strconv.Quote("\n\nTab Context:\n- Executed on tabId: 7\n- Available tabs:\n  • tabId 7: \""+slackTitle+"\" (\""+url+"\")") + `}]`)
		return strings.Replace(r, `"tool_use_id":"t"`, `"tool_use_id":"`+id+`"`, 1)
	}
	click := func(words string) map[string]any {
		return map[string]any{actionKey: actionLeftClick, tabKey: 7, summaryKey: words}
	}
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"its channel chosen": {[]string{use("a", click("Opens the channel picker")), result("a", slackInstall),
			use("b", click("Chooses #team-bumblebee")), result("b", slackInstall)}, slackChannel},
		"a later choice": {[]string{use("a", click("Chooses #team-bumblebee")), result("a", slackInstall), use("b", click("Chooses #General")), result("b", slackInstall)}, "general"},
		"on another URL": {[]string{use("a", click("Chooses #team-bumblebee")), result("a", slackSettings), use("b", click("Reinstalls")), result("b", slackInstall)}, ""},
		"no result":      {[]string{use("a", click("Chooses #team-bumblebee"))}, ""},
		"none named":     {[]string{use("a", click("Opens the channel picker")), result("a", slackInstall)}, ""},
	} {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := lastPages(path)["7"].Picked; got != tc.want {
			t.Errorf("%s: picked %q, want %q", name, got, tc.want)
		}
	}
}

// A webhook channel is kept by its name, lower case without "#"; a name
// Slack gives no channel is none.
func TestSlackChannel(t *testing.T) {
	for in, want := range map[string]string{"#Team-Bumblebee": "team-bumblebee", " alerts.prod ": "alerts.prod", "#": "", "two words": "", "-x": ""} {
		if got := SlackChannel(in); got != want {
			t.Errorf("SlackChannel(%q) = %q, want %q", in, got, want)
		}
	}
}
