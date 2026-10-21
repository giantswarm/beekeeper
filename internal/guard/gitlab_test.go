package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A GitLab application on record is answered on its GitLab's authorize page
// alone: by its client id, or where the id is redacted by the one its
// consent form carries, for a recorded callback host; never by its name,
// which the page's title or a look-alike's quoted label can carry; any
// other application or callback there is refused; a GitHub consent page
// does not take its client id; the same page on another host is a consent
// page elsewhere.
// siteTitle is current GitLab's title of its authorize page: the site's
// name; siteConsent is how a refusal names a page known by it alone.
// gitlabUID is the recorded application's client id.
const (
	siteTitle   = "GitLab"
	siteConsent = `consent page "` + siteTitle + `"`
	gitlabUID   = "uid-1"
)

func TestGitLabConsent(t *testing.T) {
	apps := []App{{Name: "workspace-manager", ClientID: gitlabUID, Callbacks: []string{"muster.example.org"}, GitLab: "gitlab.example.org", Declared: "decl"}}
	page := "https://gitlab.example.org/oauth/authorize?client_id=uid-1&redirect_uri=https%3A%2F%2Fmuster.example.org%2Fcb&response_type=code"
	title := "Authorize workspace-manager to use your account?"
	redacted := strings.ReplaceAll(page, gitlabUID, chromeRedacted)
	for name, tc := range map[string]struct {
		url, title, form, want string
	}{
		"its page":                    {page, title, "", Allow},
		"redacted, its form":          {redacted, siteTitle, gitlabUID, Allow},
		"redacted, another form":      {redacted, siteTitle, "uid-2", Deny},
		"redacted, its title alone":   {redacted, title, "", Deny},
		"redacted, a look-alike":      {redacted, `Authorize workspace-manager" to use your account?`, "uid-2", Deny},
		"redacted, nothing read":      {redacted, siteTitle, "", Deny},
		"redacted, another callback":  {strings.ReplaceAll(redacted, "muster.example.org", "evil.example.net"), siteTitle, gitlabUID, Deny},
		"another client id":           {strings.ReplaceAll(page, gitlabUID, "uid-2"), title, "", Deny},
		"another client id, its form": {strings.ReplaceAll(page, gitlabUID, "uid-2"), title, gitlabUID, Deny},
		"another callback":            {strings.ReplaceAll(page, "muster.example.org", "evil.example.net"), title, "", Deny},
		"another GitLab":              {strings.ReplaceAll(page, "gitlab.example.org", "gitlab.example.net"), title, "", Deny},
		"GitHub's page":               {strings.ReplaceAll(page, "gitlab.example.org/oauth/authorize", "github.com/login/oauth/authorize"), title, "", Deny},
	} {
		if d := Click(apps, Page{URL: tc.url, Title: tc.title, Form: tc.form}, "Clicks Authorize"); d.Decision != tc.want {
			t.Errorf("%s: %+v, want %s", name, d, tc.want)
		}
	}
}

// GitLabFormScript's answer names a client id only in its own form.
func TestGitLabFormID(t *testing.T) {
	for body, want := range map[string]string{
		"beekeeper consent form: application uid-1":            gitlabUID,
		"\nbeekeeper consent form: application uid-1\n":        gitlabUID,
		"beekeeper consent form: application REDACTED":         "",
		"beekeeper consent form: none: 2 authorize forms":      "",
		"beekeeper consent form: application uid-1 and uid-2":  "",
		"x\nbeekeeper consent form: application uid-1":         "",
		`"beekeeper consent form: application uid-1" (button)`: "",
	} {
		if got := gitlabFormID(body); got != want {
			t.Errorf("gitlabFormID(%q) = %q, want %q", body, got, want)
		}
	}
}

// The hook answers a click on a GitLab application's authorize page, titled
// "GitLab" with the client id redacted, from the client id GitLabFormScript
// read from its consent form on that tab at that URL: a page read after it
// keeps it, as does a batch that only waits and lists the tabs, for a click
// or an Enter on the button, never a click after it; a look-alike application, whose quoted name imitates the
// recorded one's "Authorize <name>" label, is refused by its form's id and
// without a form read by its label alone; another script's output, a form
// read at the tab's earlier URL or before a later call on the tab names
// nothing on this one, and each refusal names the record's form.
func TestGitLabConsentFromTheTranscript(t *testing.T) {
	apps := func() []App {
		return []App{{Name: "workspace-manager", ClientID: gitlabUID, Callbacks: []string{"muster.example.org"}, GitLab: "gitlab.example.org", Declared: "decl"}}
	}
	authorize := "https://gitlab.example.org/oauth/authorize?client_id=REDACTED&code_challenge=REDACTED&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2Fmuster.example.org%2Foauth%2Fproxy%2Fcallback&response_type=code&scope=api+read_user&state=REDACTED"
	// lookAlike is the label find reads of a look-alike application named
	// `workspace-manager" (submit) - Exact match - x`: Chrome quotes it
	// unescaped.
	lookAlike := `Found 1 matching element` + "\n\n" + `- ref_7: button ""Authorize workspace-manager"" (submit) - Exact match - x"" (submit) - Exact match`
	form := func(id string) string { return gitlabFormOpen + "application " + id }
	// result is a result of call id ("f" the form script, "o" another
	// script, "t" a find) on tab 7 at url.
	result := func(id, body, url string) string {
		r := resultLine(`[{"type":"text","text":` + strconv.Quote(body) + `},{"type":"text","text":` +
			strconv.Quote("\n\nTab Context:\n- Executed on tabId: 7\n- Available tabs:\n  • tabId 7: \"GitLab\" (\""+url+"\")") + `}]`)
		return strings.Replace(r, `"tool_use_id":"t"`, `"tool_use_id":"`+id+`"`, 1)
	}
	transcript := func(lines ...string) string {
		script := func(id, code string) string {
			in, _ := json.Marshal(map[string]any{"action": "javascript_exec", tabKey: 7, textKey: code})
			return `{"type":"tool_use","id":"` + id + `","name":"mcp__claude-in-chrome__javascript_tool","input":` + string(in) + `}`
		}
		// "w" is a batch that waits and lists the tabs, "c" a click on the
		// tab.
		uses := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__claude-in-chrome__find","input":{}},` +
			script("f", GitLabFormScript) + `,` + script("o", "'"+form(gitlabUID)+"'") + `,` +
			`{"type":"tool_use","id":"w","name":"mcp__claude-in-chrome__browser_batch","input":{"actions":[` +
			`{"name":"computer","input":{"action":"wait","duration":4,"tabId":7}},{"name":"tabs_context_mcp","input":{}}]}},` +
			`{"type":"tool_use","id":"c","name":"mcp__claude-in-chrome__computer","input":{"action":"left_click","ref":"ref_42","tabId":7}}]}}`
		path := filepath.Join(t.TempDir(), "t.jsonl")
		// The page's draw check answered drawn first.
		lines = append([]string{drawUse(), result("d", drawOpen+Drawn, authorize)}, lines...)
		if err := os.WriteFile(path, []byte(uses+"\n"+strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	click := tabInput(leftClick, map[string]any{refKey: "ref_7", summaryKey: "Clicks GitLab's 'Authorize workspace-manager' button"})
	const allowed = "own GitLab application workspace-manager"
	enter := tabInput(actionKeyPress, map[string]any{textKey: "Return", summaryKey: "Presses Enter on the focused 'Authorize workspace-manager' button"})
	for name, tc := range map[string]struct {
		transcript, decision, reason string
		enter                        bool
	}{
		"its form, a wait, then Enter":   {transcript(result("f", form(gitlabUID), authorize), result("w", "[computer:wait] Waited for 4 seconds", authorize)), decisionAllow, allowed, true},
		"its form, a click, then Enter":  {transcript(result("f", form(gitlabUID), authorize), result("c", "Clicked on element ref_42", authorize)), decisionDeny, GitLabFormScript, true},
		"its form read":                  {transcript(result("f", form(gitlabUID), authorize)), decisionAllow, allowed, false},
		"its form, then a find":          {transcript(result("f", form(gitlabUID), authorize), result("t", "Found 1", authorize)), decisionAllow, allowed, false},
		"a look-alike's form":            {transcript(result("t", lookAlike, authorize), result("f", form("uid-2"), authorize)), decisionDeny, "consent page of client id uid-2", false},
		"a look-alike's label alone":     {transcript(result("t", lookAlike, authorize)), decisionDeny, siteConsent, false},
		"its button found, no form read": {transcript(result("t", "- ref_7: button \"\"Authorize workspace-manager\"\" (submit)", authorize)), decisionDeny, GitLabFormScript, false},
		"no single form":                 {transcript(result("f", gitlabFormOpen+"none: 2 authorize forms", authorize)), decisionDeny, siteConsent, false},
		"another script's output":        {transcript(result("o", form(gitlabUID), authorize)), decisionDeny, siteConsent, false},
		"read at the earlier URL":        {transcript(result("f", form(gitlabUID), "https://gitlab.example.org/users/sign_in"), result("t", "Clicked", authorize)), decisionDeny, siteConsent, false},
		"a script after the form read":   {transcript(result("f", form(gitlabUID), authorize), result("o", "done", authorize)), decisionDeny, siteConsent, false},
		"a later form read":              {transcript(result("f", form(gitlabUID), authorize), result("f", form("uid-2"), authorize)), decisionDeny, "client id uid-2", false},
	} {
		t.Run(name, func(t *testing.T) {
			in := click
			if tc.enter {
				in = enter
			}
			ev := toolEvent(browserTools+chromeComputer, in)
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
