package guard

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A GitLab application on record is answered on its GitLab's authorize page
// alone: by its client id, or where the id is redacted by the name alone its
// title or its text carries (the title "GitLab" names none), for a recorded
// callback host; any other application or callback there is
// refused; a GitHub consent page does not take its client id; the same page
// on another host is a consent page elsewhere.
// siteTitle is current GitLab's title of its authorize page: the site's name.
const siteTitle = "GitLab"

func TestGitLabConsent(t *testing.T) {
	apps := []App{{Name: "workspace-manager", ClientID: "uid-1", Callbacks: []string{"muster.example.org"}, GitLab: "gitlab.example.org", Declared: "decl"}}
	page := "https://gitlab.example.org/oauth/authorize?client_id=uid-1&redirect_uri=https%3A%2F%2Fmuster.example.org%2Fcb&response_type=code"
	title := "Authorize workspace-manager to use your account?"
	redacted := strings.ReplaceAll(page, "uid-1", chromeRedacted)
	heading := "Authorize workspace-manager to use your account?\nAn application called workspace-manager is requesting access to your GitLab account."
	button := `- ref_7: button ""Authorize workspace-manager"" (submit) - Exact match - the button with the text "Authorize workspace-manager"`
	for name, tc := range map[string]struct {
		url, title, text, want string
	}{
		"its page":                         {page, title, "", Allow},
		"redacted, its title":              {redacted, title, "", Allow},
		"redacted, another":                {redacted, "Authorize other", "", Deny},
		"redacted, site name":              {redacted, "Authorize workspace-manager · GitLab", "", Allow},
		"redacted, a prefix":               {redacted, "Authorize workspace-manager-other to use your account?", "", Deny},
		"redacted, a suffix":               {redacted, "Authorize workspace-manager evil to use your account?", "", Deny},
		"GitLab's title, its heading":      {redacted, siteTitle, heading, Allow},
		"GitLab's title, its button":       {redacted, siteTitle, button, Allow},
		"GitLab's title, nothing read":     {redacted, siteTitle, "", Deny},
		"GitLab's title, another heading":  {redacted, siteTitle, strings.ReplaceAll(heading, "workspace-manager", "other"), Deny},
		"GitLab's title, a prefix":         {redacted, siteTitle, strings.ReplaceAll(heading, "workspace-manager", "workspace-manager-other"), Deny},
		"GitLab's title, two names":        {redacted, siteTitle, heading + "\nAuthorize other", Deny},
		"GitLab's title, another callback": {strings.ReplaceAll(redacted, "muster.example.org", "evil.example.net"), siteTitle, heading, Deny},
		"another client id":                {strings.ReplaceAll(page, "uid-1", "uid-2"), title, "", Deny},
		"another callback":                 {strings.ReplaceAll(page, "muster.example.org", "evil.example.net"), title, "", Deny},
		"another GitLab":                   {strings.ReplaceAll(page, "gitlab.example.org", "gitlab.example.net"), title, "", Deny},
		"GitHub's page":                    {strings.ReplaceAll(page, "gitlab.example.org/oauth/authorize", "github.com/login/oauth/authorize"), title, "", Deny},
	} {
		if d := Click(apps, Page{URL: tc.url, Title: tc.title, Text: tc.text}, "Clicks Authorize"); d.Decision != tc.want {
			t.Errorf("%s: %+v, want %s", name, d, tc.want)
		}
	}
}

// The hook answers a click on a GitLab application's authorize page, titled
// "GitLab" with the client id redacted, from what the Chrome tools read on
// that tab at that URL: a find's match of the "Authorize <name>" button;
// text read at the tab's earlier URL names nothing on this one, and another
// application's button is refused naming the record's form.
func TestGitLabConsentFromTheTranscript(t *testing.T) {
	apps := func() []App {
		return []App{{Name: "workspace-manager", ClientID: "uid-1", Callbacks: []string{"muster.example.org"}, GitLab: "gitlab.example.org", Declared: "decl"}}
	}
	authorize := "https://gitlab.example.org/oauth/authorize?client_id=REDACTED&code_challenge=REDACTED&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2Fmuster.example.org%2Foauth%2Fproxy%2Fcallback&response_type=code&scope=api+read_user&state=REDACTED"
	found := func(name string) string {
		return `Found 1 matching element` + "\n\n" + `- ref_7: button ""Authorize ` + name + `"" (submit) - Exact match`
	}
	result := func(body, url string) string {
		return resultLine(`[{"type":"text","text":` + strconv.Quote(body) + `},{"type":"text","text":` +
			strconv.Quote("\n\nTab Context:\n- Executed on tabId: 7\n- Available tabs:\n  • tabId 7: \"GitLab\" (\""+url+"\")") + `}]`)
	}
	transcript := func(lines ...string) string {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__claude-in-chrome__find","input":{}}]}}`
		if err := os.WriteFile(path, []byte(use+"\n"+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	click := tabInput(leftClick, map[string]any{refKey: "ref_7", summaryKey: "Clicks GitLab's 'Authorize workspace-manager' button"})
	for name, tc := range map[string]struct {
		transcript, decision, reason string
	}{
		"its button found":           {transcript(result(found("workspace-manager"), authorize)), decisionAllow, "own GitLab application workspace-manager"},
		"read at the earlier URL":    {transcript(result(found("workspace-manager"), "https://gitlab.example.org/users/sign_in"), result("Clicked", authorize)), decisionDeny, `consent page "GitLab"`},
		"another application's page": {transcript(result(found("other"), authorize)), decisionDeny, `consent page "Authorize other" (callback muster.example.org)`},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(browserTools+chromeComputer, click)
			ev["permission_mode"] = dontAsk
			ev[sessionKey] = browseTurn
			ev["transcript_path"] = tc.transcript
			d := decideEvent(t, Hook{Apps: apps}, ev)
			if d == nil || d.PermissionDecision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("decided %+v, want %s with %q", d, tc.decision, tc.reason)
			}
			if tc.decision == decisionDeny && !strings.Contains(d.Reason, GitLabForm) {
				t.Errorf("refusal %q names no record's form", d.Reason)
			}
		})
	}
}
