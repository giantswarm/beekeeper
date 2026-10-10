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
	applicationsPage  = "https://github.com/settings/applications"
)

var recorded = []App{{Name: ownApp, ClientID: ownClientID, Callback: ownCallback, Word: "your agent has to do this", By: "the guide",
	At: time.Date(2026, 10, 10, 8, 16, 0, 0, time.UTC)}}

// The App on record is allowed on its consent page with its callback host,
// by client id or, where the tool redacted it, by GitHub's title; another
// App, another callback host and a title that names another App are
// refused naming the record's form; a page that is no consent page gets no
// decision; the lab's identity provider on a loopback host is allowed, on
// another host not decided.
func TestConsent(t *testing.T) {
	for name, tc := range map[string]struct {
		page     Page
		apps     []App
		decision string
		reason   string
	}{
		"own App and host":              {Page{ownPage, ownTitle}, recorded, Allow, "the person's own App " + ownApp + " (callback " + ownCallback + "), on record since 2026-10-10 by the guide: your agent has to do this"},
		"own App, the id redacted":      {Page{redactedPage, ownTitle}, recorded, Allow, "the person's own App " + ownApp},
		"own App by id, title unknown":  {Page{ownPage, ""}, recorded, Allow, ownApp},
		"other App":                     {Page{otherAppPage, otherTitle}, recorded, Deny, `GitHub's consent page of client id Iv1.ffffffffffffffff (callback ` + ownCallback + `) names an App beekeeper has no record of`},
		"other App, the id redacted":    {Page{redactedPage, otherTitle}, recorded, Deny, `GitHub's consent page "` + otherTitle + `" (callback ` + ownCallback + `)`},
		"other callback host":           {Page{otherCallbackPage, ownTitle}, recorded, Deny, "(callback example.org)"},
		"no record at all":              {Page{ownPage, ownTitle}, []App{}, Deny, AllowForm},
		"other page":                    {Page{applicationsPage, "Applications"}, recorded, "", ""},
		"the portal":                    {Page{"https://portal.example.org/agents", "Agents"}, recorded, "", ""},
		"lab Dex sign-in":               {Page{labDexPage, "Log in to agentlab"}, recorded, Allow, "dex.127.0.0.1.nip.io is the lab's own identity provider on a loopback host"},
		"lab Dex approval on localhost": {Page{"https://localhost:32000/dex/approval?req=abc", "Grant Access"}, nil, Allow, "localhost:32000 is the lab's own identity provider"},
		"lab portal sign-in":            {Page{"https://" + ownCallback + "/connect/github", "Connect"}, nil, Allow, "loopback host"},
		"Dex on another host":           {Page{"https://dex.example.org/dex/approval?req=abc", "Grant Access"}, recorded, "", ""},
		"a lab page that is no sign-in": {Page{"https://backstage.127.0.0.1.nip.io/catalog", "Catalog"}, recorded, "", ""},
		"no URL":                        {Page{"", ""}, recorded, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := Consent(tc.apps, tc.page)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Consent = %+v, want %q with %q", d, tc.decision, tc.reason)
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
		return `Tab Context: - Available tabs:   • tabId 7: "` + p.Title + `" ("` + p.URL + `")`
	}
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__claude-in-chrome__navigate","input":{"url":"https://example.org/"}}]}}`,
		resultLine(strconv.Quote("[navigate] Navigated to https://example.org/ " + ctx(Page{"https://example.org/", "Example"}))),
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
	own := transcriptWith(t, Page{redactedPage, ownTitle}, false)
	ownJSON := transcriptWith(t, Page{redactedPage, ownTitle}, true)
	other := transcriptWith(t, Page{redactedPage, otherTitle}, false)
	plain := transcriptWith(t, Page{applicationsPage, "Applications"}, false)
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
			ev["permission_mode"] = "dontAsk"
			ev[sessionKey] = "browse-turn"
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
	path := transcriptWith(t, Page{redactedPage, ownTitle}, false)
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
