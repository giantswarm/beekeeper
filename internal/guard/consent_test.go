package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The App on record and the pages the decision is tested on: GitHub's
// consent page of the App with its callback host, the same with the client
// id redacted as the Chrome tools report it, the page of another App, the
// App's page with another callback, a page that is no consent page, and
// the lab's own identity provider on a loopback host.
const (
	ownApp      = "example-lab-workspaces"
	ownClientID = "Iv1.0123456789abcdef"
	ownCallback = "workspace-manager.127.0.0.1.nip.io"
	ownTitle    = authorizeTitle + ownApp
	otherTitle  = authorizeTitle + "other-app"
	ownPage     = "https://github.com/login/oauth/authorize?client_id=" + ownClientID + "&code_challenge=x&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2F" + ownCallback + "%2Fcallback%2Fgithub&response_type=code&state=s"
	redactedPage = "https://github.com/login/oauth/authorize?client_id=REDACTED&code_challenge=REDACTED&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2F" + ownCallback + "%2Fcallback%2Fgithub&response_type=code&state=REDACTED"
	otherAppPage      = "https://github.com/login/oauth/authorize?client_id=Iv1.ffffffffffffffff&redirect_uri=https%3A%2F%2F" + ownCallback + "%2Fcallback%2Fgithub&response_type=code"
	otherCallbackPage = "https://github.com/login/oauth/authorize?client_id=" + ownClientID + "&redirect_uri=https%3A%2F%2Fexample.org%2Fcallback%2Fgithub&response_type=code"
	labDexPage        = "https://dex.127.0.0.1.nip.io/dex/auth/local?req=abc"
	lab2AuthorizePage = "https://github-bearer.lab2.127.0.0.2.nip.io/login/oauth/authorize?client_id=lab&response_type=code"
	applicationsPage  = "https://github.com/settings/applications"
	lanConsentPage    = "http://192.168.1.20:5556/dex/approval?req=abc&hmac=x"
	authorizePage     = "https://login.example.org/common/oauth2/v2.0/authorize?client_id=x&response_type=code"
	dexAuthPage       = "https://dex.example.org/dex/auth?client_id=x&redirect_uri=https%3A%2F%2Fportal.example.org%2Fcallback&response_type=code"
)

// The titles of a consent page, an IdP's sign-in and Dex's sign-in.
const (
	grantAccess = "Grant Access"
	idpSignIn   = "Sign in to your account"
	dexSignIn   = "Log in to Dex"
)

// ownDeclared is where the organisation declares the App.
const ownDeclared = "example/github@0123456789ab:apps/" + ownApp + "/manifest.json"

var recorded = []App{{Name: ownApp, ClientID: ownClientID, Callbacks: []string{ownCallback, ownCallback + ":8443"}, Declared: ownDeclared,
	Word: "your agent has to do this", By: byGuide, At: time.Date(2026, 10, 10, 8, 16, 0, 0, time.UTC)}}

// The App on record is allowed on its consent page with one of its declared
// callback hosts,
// by client id or, where the tool redacted it, by GitHub's title; another
// App, another callback host and a title that names another App are
// refused naming the record's form; a page that is no consent page gets no
// decision; the lab's identity provider on a loopback host is allowed, a
// consent page on any other host refused.
func TestConsent(t *testing.T) {
	for name, tc := range map[string]struct {
		page     Page
		apps     []App
		decision string
		reason   string
	}{
		"own App and host":                   {Page{URL: ownPage, Title: ownTitle}, recorded, Allow, "the person's own App " + ownApp + " (callback " + ownCallback + "), declared in " + ownDeclared + ", on record since 2026-10-10 by the guide: your agent has to do this"},
		"own App, its other declared host":   {Page{URL: strings.Replace(ownPage, ownCallback, ownCallback+"%3A8443", 1), Title: ownTitle}, recorded, Allow, "(callback " + ownCallback + ":8443), declared in"},
		"own App, the id redacted":           {Page{URL: redactedPage, Title: ownTitle}, recorded, Allow, "the person's own App " + ownApp},
		"own App by id, title unknown":       {Page{URL: ownPage, Title: ""}, recorded, Allow, ownApp},
		"other App":                          {Page{URL: otherAppPage, Title: otherTitle}, recorded, Deny, `GitHub's consent page of client id Iv1.ffffffffffffffff (callback ` + ownCallback + `) names an App beekeeper has no record of`},
		"other App, the id redacted":         {Page{URL: redactedPage, Title: otherTitle}, recorded, Deny, `GitHub's consent page "` + otherTitle + `" (callback ` + ownCallback + `)`},
		"other callback host":                {Page{URL: otherCallbackPage, Title: ownTitle}, recorded, Deny, "(callback example.org)"},
		"no record at all":                   {Page{URL: ownPage, Title: ownTitle}, []App{}, Deny, AllowForm},
		"other page":                         {Page{URL: applicationsPage, Title: applicationsTitle}, recorded, "", ""},
		"the portal":                         {Page{URL: "https://portal.example.org/agents", Title: "Agents"}, recorded, "", ""},
		"lab Dex sign-in":                    {Page{URL: labDexPage, Title: "Log in to agentlab"}, recorded, Allow, "dex.127.0.0.1.nip.io is the lab's own identity provider on a loopback host"},
		"lab Dex approval on localhost":      {Page{URL: "https://localhost:32000/dex/approval?req=abc", Title: grantAccess}, nil, Allow, "localhost:32000 is the lab's own identity provider"},
		"lab portal sign-in":                 {Page{URL: "https://" + ownCallback + "/connect/github", Title: "Connect"}, nil, Allow, "loopback host"},
		"second lab's authorize page":        {Page{URL: lab2AuthorizePage, Title: "Authorize agentlab"}, nil, Allow, "github-bearer.lab2.127.0.0.2.nip.io is the lab's own identity provider on a loopback host"},
		"a consent on a non-loopback nip.io": {Page{URL: "https://dex.10.0.0.2.nip.io/dex/approval?req=abc", Title: grantAccess}, recorded, Deny, "on dex.10.0.0.2.nip.io, neither the consent page of an App on record"},
		"Dex on another host":                {Page{URL: "https://dex.example.org/dex/approval?req=abc", Title: grantAccess}, recorded, Deny, `"` + grantAccess + `" is a consent page (an OAuth grant) on dex.example.org`},
		"a consent on a LAN host":            {Page{URL: lanConsentPage, Title: grantAccess}, recorded, Deny, "on 192.168.1.20:5556, neither the consent page of an App on record"},
		"a consent path, no title":           {Page{URL: "https://idp.example.org/oauth2/consent?challenge=c", Title: ""}, recorded, Deny, "/oauth2/consent is a consent page"},
		"a grant title on any path":          {Page{URL: "https://apps.example.org/connect", Title: "Authorize example-tool"}, recorded, Deny, AllowForm},
		"another GitHub's consent page":      {Page{URL: "https://github.example.org/login/oauth/authorize?client_id=x&redirect_uri=https%3A%2F%2Fexample.org", Title: ""}, recorded, Deny, "on github.example.org"},
		"an IdP's authorization page":        {Page{URL: authorizePage, Title: idpSignIn}, recorded, Deny, "on login.example.org"},
		"Dex's sign-in on another host":      {Page{URL: dexAuthPage, Title: dexSignIn}, recorded, Deny, "on dex.example.org"},
		"a lab page that is no sign-in":      {Page{URL: "https://backstage.127.0.0.1.nip.io/catalog", Title: "Catalog"}, recorded, "", ""},
		"no URL":                             {Page{URL: "", Title: ""}, recorded, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := Click(tc.apps, tc.page, clicksAuthorize)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Click = %+v, want %q with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// On an OAuth authorization page elsewhere, which also signs in, a call whose
// own words name a grant is refused and any other gets no decision; on a
// consent page every call acting on it is refused, whatever its words.
func TestConsentElsewhereByWords(t *testing.T) {
	for name, tc := range map[string]struct {
		page     Page
		words    string
		decision string
	}{
		"the SSO button on Dex's sign-in":     {Page{URL: dexAuthPage, Title: dexSignIn}, "Clicks Log in with GitHub", ""},
		"an account on an IdP's sign-in":      {Page{URL: authorizePage, Title: idpSignIn}, "Clicks the account tile", ""},
		"a click without words on a sign-in":  {Page{URL: authorizePage, Title: idpSignIn}, "", ""},
		"Accept on an IdP's authorization":    {Page{URL: authorizePage, Title: idpSignIn}, "Clicks Accept", Deny},
		"Allow on Dex's sign-in":              {Page{URL: dexAuthPage, Title: dexSignIn}, "Clicks Allow", Deny},
		"a click without words on a consent":  {Page{URL: lanConsentPage, Title: grantAccess}, "", Deny},
		"Cancel on a consent":                 {Page{URL: lanConsentPage, Title: grantAccess}, "Clicks Cancel", Deny},
		"a portal page with a client_id only": {Page{URL: "https://portal.example.org/settings?client_id=x", Title: "Settings"}, "Clicks Allow", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if d := Click(recorded, tc.page, tc.words); d.Decision != tc.decision {
				t.Errorf("Click = %+v, want %q", d, tc.decision)
			}
		})
	}
}

// resultLine is a transcript line with one tool result whose content is
// the JSON document content (a quoted string, or an array of blocks).
func resultLine(content string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":` + content + `}]}}`
}

// transcriptWith writes a browse turn's transcript whose Chrome tool
// results end with the tab context, the last naming page for tab 7 in the
// bullet form and, when asJSON, a tabs_context result in the JSON form.
func transcriptWith(t *testing.T, page Page, asJSON bool) string {
	t.Helper()
	ctx := func(p Page) string {
		return "\n\nTab Context:\n- Available tabs:\n  • tabId 7: \"" + p.Title + `" ("` + p.URL + `")`
	}
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__claude-in-chrome__navigate","input":{"url":"https://example.org/"}}]}}`,
		resultLine(strconv.Quote("[navigate] Navigated to https://example.org/ " + ctx(Page{URL: "https://example.org/", Title: "Example"}))),
		`{"type":"user","message":{"content":"plain text"}}`,
		"no json here",
		resultLine(`[{"type":"text","text":` + strconv.Quote("Successfully captured screenshot "+ctx(page)) + `},{"type":"image"}]`),
	}
	if asJSON {
		lines = append(lines, resultLine(strconv.Quote(`{"availableTabs":[{"tabId":7,"title":`+strconv.Quote(page.Title)+`,"url":`+strconv.Quote(page.URL)+`}],"selectedTabId":7}`)))
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The Chrome calls the decision is tested with: a click on tab 7, a
// screenshot of it, and a batch action of the computer tool.
const (
	leftClick       = "left_click"
	clicksAuthorize = "Clicks Authorize"
	tab7            = "7"
)

func tabInput(action string, more map[string]any) map[string]any {
	in := map[string]any{actionKey: action, tabKey: json.Number(tab7)}
	for k, v := range more {
		in[k] = v
	}
	return in
}

func batchAction(name string, in map[string]any) map[string]any {
	return map[string]any{nameKey: name, inputKey: in}
}

// The hook decides a Chrome call that acts on a page (a click, a key, a
// typed text, a batch with one of them) from the tab's last known page in
// the transcript: allowed on the recorded App's consent page, refused on
// another App's, not decided on a page that is no consent page, for a call
// that only reads, for a tab the transcript does not show, and without a
// record to decide from.
func TestConsentDecision(t *testing.T) {
	apps := func() []App { return recorded }
	own := transcriptWith(t, Page{URL: redactedPage, Title: ownTitle}, false)
	ownJSON := transcriptWith(t, Page{URL: redactedPage, Title: ownTitle}, true)
	other := transcriptWith(t, Page{URL: redactedPage, Title: otherTitle}, false)
	plain := transcriptWith(t, Page{URL: applicationsPage, Title: applicationsTitle}, false)
	lan := transcriptWith(t, Page{URL: lanConsentPage, Title: grantAccess}, false)
	click := tabInput(leftClick, map[string]any{"ref": "ref_45", "action_summary": clicksAuthorize})
	screenshot := tabInput("screenshot", nil)
	batch := map[string]any{"actions": []any{batchAction(chromeComputer, screenshot), batchAction(chromeComputer, tabInput(leftClick, nil))}}
	reads := map[string]any{"actions": []any{batchAction("find", map[string]any{"query": clicksAuthorize, tabKey: json.Number(tab7)})}}
	for name, tc := range map[string]struct {
		tool       string
		input      map[string]any
		transcript string
		apps       func() []App
		decision   string
		reason     string
	}{
		"a click on the own App's page":       {browserTools + chromeComputer, click, own, apps, decisionAllow, "the person's own App " + ownApp},
		"the tab context in JSON":             {browserTools + chromeComputer, click, ownJSON, apps, decisionAllow, ownApp},
		"a batch with the click":              {browserTools + chromeBatch, batch, own, apps, decisionAllow, ownApp},
		"a form value on the page":            {browserTools + chromeForm, map[string]any{"value": "x", tabKey: json.Number(tab7)}, own, apps, decisionAllow, ownApp},
		"the desktop's spelling of the tools": {"mcp__Claude_in_Chrome__" + chromeComputer, click, own, apps, decisionAllow, ownApp},
		"a click on another App's page":       {browserTools + chromeComputer, click, other, apps, decisionDeny, otherTitle},
		"a click on no consent page":          {browserTools + chromeComputer, click, plain, apps, "", ""},
		"a click on a consent page elsewhere": {browserTools + chromeComputer, click, lan, apps, decisionDeny, "on 192.168.1.20:5556"},
		"a batch on a consent page elsewhere": {browserTools + chromeBatch, batch, lan, apps, decisionDeny, `"` + grantAccess + `" is a consent page`},
		"a screenshot of the own App's page":  {browserTools + chromeComputer, screenshot, own, apps, "", ""},
		"a batch that only reads":             {browserTools + chromeBatch, reads, own, apps, "", ""},
		"a tab the transcript does not show":  {browserTools + chromeComputer, map[string]any{actionKey: leftClick, tabKey: json.Number("8")}, own, apps, "", ""},
		"a click without a tab, one known":    {browserTools + chromeComputer, map[string]any{actionKey: leftClick}, own, apps, decisionAllow, ownApp},
		"no transcript":                       {browserTools + chromeComputer, click, "", apps, "", ""},
		"a transcript that is missing":        {browserTools + chromeComputer, click, filepath.Join(t.TempDir(), "none.jsonl"), apps, "", ""},
		"no record to decide from":            {browserTools + chromeComputer, click, own, nil, "", ""},
		"another tool":                        {"mcp__github__get_issue", click, own, apps, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(tc.tool, tc.input)
			ev["permission_mode"] = dontAsk
			ev[sessionKey] = browseTurn
			ev["transcript_path"] = tc.transcript
			d := decideEvent(t, Hook{Apps: tc.apps}, ev)
			switch {
			case tc.decision == "":
				if d != nil && d.PermissionDecision != "" {
					t.Errorf("decided %+v, want no decision", d)
				}
			case d == nil || d.PermissionDecision != tc.decision || !strings.Contains(d.Reason, tc.reason):
				t.Errorf("decided %+v, want %s with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// A transcript longer than the tail read keeps the latest page: the tail's
// cut first line decodes as nothing, the rest reads as usual.
func TestLastPageReadsTheTail(t *testing.T) {
	path := transcriptWith(t, Page{URL: redactedPage, Title: ownTitle}, false)
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	pad := `{"type":"user","message":{"content":"` + strings.Repeat("x", transcriptTail) + `"}}` + "\n"
	if err := os.WriteFile(path, append([]byte(pad), b...), 0o600); err != nil { //nolint:gosec // the test's own file
		t.Fatal(err)
	}
	p, ok := lastPage(path, tab7)
	if !ok || p.URL != redactedPage || p.Title != ownTitle {
		t.Errorf("lastPage of a long transcript = %+v, %v", p, ok)
	}
}

// Every agentlab binds a loopback address of its own, so a host is the
// machine's by the address it names, not by a list of labs.
func TestLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":                           true,
		"dex.localhost":                       true,
		"127.0.0.1":                           true,
		"127.0.0.2":                           true,
		"::1":                                 true,
		"[::1]":                               true,
		"dex.127.0.0.1.nip.io":                true,
		"github-bearer.lab2.127.0.0.2.nip.io": true,
		"Dex.LAB3.127.0.0.3.nip.io.":          true,
		"backstage-127-0-0-2.nip.io":          true,
		"127.0.0.2.nip.io":                    true,
		"dex.10.0.0.2.nip.io":                 false,
		"dex.192.168.1.20.nip.io":             false,
		"dex.127.0.0.1.nip.io.example.org":    false,
		"dex.127.0.0.1.example.org":           false,
		"127.0.0.1.example.org":               false,
		"my-localhost.example.org":            false,
		"10.0.0.1":                            false,
		"":                                    false,
	} {
		if got := Loopback(host); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", host, got, want)
		}
	}
}
