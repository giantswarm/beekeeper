package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	ownPage     = "https://github.com/login/oauth/authorize?client_id=" + ownClientID + "&code_challenge=x&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2F" + ownCallback + "%2Fcallback%2Fgithub&response_type=code&state=s"
	redactedPage = "https://github.com/login/oauth/authorize?client_id=REDACTED&code_challenge=REDACTED&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2F" + ownCallback + "%2Fcallback%2Fgithub&response_type=code&state=REDACTED"
	otherAppPage      = "https://github.com/login/oauth/authorize?client_id=Iv1.ffffffffffffffff&redirect_uri=https%3A%2F%2F" + ownCallback + "%2Fcallback%2Fgithub&response_type=code"
	otherCallbackPage = "https://github.com/login/oauth/authorize?client_id=" + ownClientID + "&redirect_uri=https%3A%2F%2Fexample.org%2Fcallback%2Fgithub&response_type=code"
	labDexPage        = "https://dex.127.0.0.1.nip.io/dex/auth/local?req=abc"
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
		"own App and host":              {Page{ownPage, "Authorize " + ownApp}, recorded, Allow, "the person's own App " + ownApp + " (callback " + ownCallback + "), on record since 2026-10-10 by the guide: your agent has to do this"},
		"own App, the id redacted":      {Page{redactedPage, "Authorize " + ownApp}, recorded, Allow, "the person's own App " + ownApp},
		"own App by id, title unknown":  {Page{ownPage, ""}, recorded, Allow, ownApp},
		"other App":                     {Page{otherAppPage, "Authorize other-app"}, recorded, Deny, `GitHub's consent page of client id Iv1.ffffffffffffffff (callback ` + ownCallback + `) names an App beekeeper has no record of`},
		"other App, the id redacted":    {Page{redactedPage, "Authorize other-app"}, recorded, Deny, `GitHub's consent page "Authorize other-app" (callback ` + ownCallback + `)`},
		"other callback host":           {Page{otherCallbackPage, "Authorize " + ownApp}, recorded, Deny, "(callback example.org)"},
		"no record at all":              {Page{ownPage, "Authorize " + ownApp}, []App{}, Deny, AllowForm},
		"other page":                    {Page{"https://github.com/settings/applications", "Applications"}, recorded, "", ""},
		"the portal":                    {Page{"https://portal.example.org/agents", "Agents"}, recorded, "", ""},
		"lab Dex sign-in":               {Page{labDexPage, "Log in to agentlab"}, recorded, Allow, "dex.127.0.0.1.nip.io is the lab's own identity provider on a loopback host"},
		"lab Dex approval on localhost": {Page{"https://localhost:32000/dex/approval?req=abc", "Grant Access"}, nil, Allow, "localhost:32000 is the lab's own identity provider"},
		"lab portal sign-in":            {Page{"https://workspace-manager.127.0.0.1.nip.io/connect/github", "Connect"}, nil, Allow, "loopback host"},
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

// transcriptWith writes a browse turn's transcript whose Chrome tool
// results end with the tab context, the last naming page for tab 7 in the
// bullet form and, when asJSON, a tabs_context result in the JSON form.
func transcriptWith(t *testing.T, page Page, asJSON bool) string {
	t.Helper()
	ctx := func(p Page) string {
		return `Tab Context: - Available tabs:   • tabId 7: "` + p.Title + `" ("` + p.URL + `")`
	}
	line := func(content any) string {
		b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t", "content": content}}}})
		return string(b)
	}
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"mcp__claude-in-chrome__navigate","input":{"url":"https://example.org/"}}]}}`,
		line("[navigate] Navigated to https://example.org/ " + ctx(Page{"https://example.org/", "Example"})),
		`{"type":"user","message":{"content":"plain text"}}`,
		"not json",
		line([]any{map[string]any{"type": "text", "text": "Successfully captured screenshot " + ctx(page)}, map[string]any{"type": "image"}}),
	}
	if asJSON {
		b, _ := json.Marshal(map[string]any{"availableTabs": []any{map[string]any{"tabId": 7, "title": page.Title, "url": page.URL}}, "selectedTabId": 7})
		lines = append(lines, line(string(b)))
	}
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The hook decides a Chrome call that acts on a page (a click, a key, a
// typed text, a batch with one of them) from the tab's last known page in
// the transcript: allowed on the recorded App's consent page, refused on
// another App's, not decided on a page that is no consent page, for a call
// that only reads, for a tab the transcript does not show, and without a
// record to decide from.
func TestConsentDecision(t *testing.T) {
	apps := func() []App { return recorded }
	own := transcriptWith(t, Page{redactedPage, "Authorize " + ownApp}, false)
	ownJSON := transcriptWith(t, Page{redactedPage, "Authorize " + ownApp}, true)
	other := transcriptWith(t, Page{redactedPage, "Authorize other-app"}, false)
	plain := transcriptWith(t, Page{"https://github.com/settings/applications", "Applications"}, false)
	click := map[string]any{"action": "left_click", "ref": "ref_45", "tabId": json.Number("7"), "action_summary": "Clicks Authorize"}
	batch := map[string]any{"actions": []any{
		map[string]any{"name": "computer", "input": map[string]any{"action": "screenshot", "tabId": json.Number("7")}},
		map[string]any{"name": "computer", "input": map[string]any{"action": "left_click", "tabId": json.Number("7"), "action_summary": "Clicks Authorize"}},
	}}
	for name, tc := range map[string]struct {
		tool       string
		input      map[string]any
		transcript string
		apps       func() []App
		decision   string
		reason     string
	}{
		"a click on the own App's page":       {"mcp__claude-in-chrome__computer", click, own, apps, decisionAllow, "the person's own App " + ownApp},
		"the tab context in JSON":             {"mcp__claude-in-chrome__computer", click, ownJSON, apps, decisionAllow, ownApp},
		"a batch with the click":              {"mcp__claude-in-chrome__browser_batch", batch, own, apps, decisionAllow, ownApp},
		"a form value on the page":            {"mcp__claude-in-chrome__form_input", map[string]any{"ref": "ref_1", "value": "x", "tabId": json.Number("7")}, own, apps, decisionAllow, ownApp},
		"the desktop's spelling of the tools": {"mcp__Claude_in_Chrome__computer", click, own, apps, decisionAllow, ownApp},
		"a click on another App's page":       {"mcp__claude-in-chrome__computer", click, other, apps, decisionDeny, "Authorize other-app"},
		"a click on no consent page":          {"mcp__claude-in-chrome__computer", click, plain, apps, "", ""},
		"a screenshot of the own App's page":  {"mcp__claude-in-chrome__computer", map[string]any{"action": "screenshot", "tabId": json.Number("7")}, own, apps, "", ""},
		"a batch that only reads":             {"mcp__claude-in-chrome__browser_batch", map[string]any{"actions": []any{map[string]any{"name": "find", "input": map[string]any{"query": "Authorize", "tabId": json.Number("7")}}}}, own, apps, "", ""},
		"a tab the transcript does not show":  {"mcp__claude-in-chrome__computer", map[string]any{"action": "left_click", "tabId": json.Number("8")}, own, apps, "", ""},
		"a click without a tab, one known":    {"mcp__claude-in-chrome__computer", map[string]any{"action": "left_click", "ref": "ref_45"}, own, apps, decisionAllow, ownApp},
		"no transcript":                       {"mcp__claude-in-chrome__computer", click, "", apps, "", ""},
		"a transcript that is missing":        {"mcp__claude-in-chrome__computer", click, filepath.Join(t.TempDir(), "none.jsonl"), apps, "", ""},
		"no record to decide from":            {"mcp__claude-in-chrome__computer", click, own, nil, "", ""},
		"another tool":                        {"mcp__github__get_issue", click, own, apps, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(tc.tool, tc.input)
			ev["permission_mode"] = "dontAsk"
			ev["session_id"] = "browse-turn"
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
	path := transcriptWith(t, Page{redactedPage, "Authorize " + ownApp}, false)
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	pad := `{"type":"user","message":{"content":"` + strings.Repeat("x", transcriptTail) + `"}}` + "\n"
	if err := os.WriteFile(path, append([]byte(pad), b...), 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok := lastPage(path, "7")
	if !ok || p.URL != redactedPage || p.Title != "Authorize "+ownApp {
		t.Errorf("lastPage of a long transcript = %+v, %v", p, ok)
	}
}
