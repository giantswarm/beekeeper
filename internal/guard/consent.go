package guard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A consent page is an OAuth grant: GitHub's green Authorize button, an
// identity provider's Allow. The hook answers a Chrome call that acts on one
// from the Apps on record (Hook.Apps), the person's word: on GitHub's consent
// page of a recorded App, with its callback host, the click is allowed; on
// GitHub's consent page of any other App or callback host it is refused; on
// the lab's own identity provider, a loopback host, a fixture user's sign-in
// and consent are allowed. Any other page gets no decision, as does a call
// whose page the transcript does not show.

// App is an App the person owns and asked a consent for.
type App struct {
	// Name is the App's name as GitHub's consent page titles it.
	Name string
	// ClientID is the OAuth client id the consent URL carries.
	ClientID string
	// Callback is the host the consent page's redirect must go to.
	Callback string
	// Word is the person's words that asked for the consent, By whom and
	// At when they were recorded.
	Word, By string
	At       time.Time
}

// Page is a tab's page: its URL and title.
type Page struct {
	URL, Title string
}

// Decision is the hook's answer for a call acting on a page: Allow, Deny or
// "" for no decision, and why.
type Decision struct {
	Decision, Reason string
}

// Allow and Deny are the decisions.
const (
	Allow = decisionAllow
	Deny  = decisionDeny
)

// GitHub's consent page: its host and path.
const (
	githubHost      = "github.com"
	githubAuthorize = "/login/oauth/authorize"
)

// signInSegments are the path segments of an identity provider's sign-in
// and consent pages (Dex's auth and approval, a portal's sign-in).
var signInSegments = strings.Fields("dex auth approval login signin connect oauth")

// chromeRedacted is what the Claude in Chrome tools put in place of an
// auth-like value of a URL they report (a client id, a state); authorizeTitle
// opens GitHub's consent page's title.
const (
	chromeRedacted = "REDACTED"
	authorizeTitle = "Authorize "
)

// The Chrome tools that act on a page (their names after the server's
// prefix), and the keys of their inputs: the tab a call acts on, a batch
// action's name and input.
const (
	chromeComputer = "computer"
	chromeBatch    = "browser_batch"
	chromeForm     = "form_input"
	tabKey         = "tabId"
	nameKey        = "name"
	inputKey       = "input"
)

// AllowForm is how an App the person owns and asked for is recorded.
const AllowForm = "`beekeeper app allow <name> --client-id <id> --callback <host> --word \"<the person's words>\"`"

// Consent decides a call acting on page from the Apps on record.
func Consent(apps []App, page Page) Decision {
	u, err := url.Parse(strings.TrimSpace(page.URL))
	if err != nil {
		return Decision{}
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == githubHost && u.Path == githubAuthorize:
		return githubConsent(apps, u, page.Title)
	case loopback(host) && signIn(u.Path):
		return Decision{Allow, fmt.Sprintf("beekeeper allows it: %s is the lab's own identity provider on a loopback host, "+
			"whose fixture users' sign-in and consent are the person's own lab's", u.Host)}
	}
	return Decision{}
}

// githubConsent decides a call on GitHub's consent page u: allowed for the
// App on record whose callback host the page's redirect names and whose
// client id the URL carries, or whose name GitHub's title carries where the
// tool redacted the id; refused for any other App or callback.
func githubConsent(apps []App, u *url.URL, title string) Decision {
	q := u.Query()
	clientID := q.Get("client_id")
	callback := ""
	if r, err := url.Parse(q.Get("redirect_uri")); err == nil {
		callback = strings.ToLower(r.Host)
	}
	for _, a := range apps {
		if !strings.EqualFold(a.Callback, callback) {
			continue
		}
		if clientID == a.ClientID || (clientID == chromeRedacted && strings.EqualFold(title, authorizeTitle+a.Name)) {
			return Decision{Allow, fmt.Sprintf("beekeeper allows the click: the consent page of the person's own App %s (callback %s), "+
				"on record since %s by %s: %s", a.Name, a.Callback, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
		}
	}
	which := "of client id " + clientID
	if clientID == "" || clientID == chromeRedacted {
		which = strconv.Quote(title)
	}
	return Decision{Deny, fmt.Sprintf("Refused: GitHub's consent page %s (callback %s) names an App beekeeper has no record of. "+
		"A consent is a grant of the person's identity, answered only for an App the person owns and asked for, which %s "+
		"records on their word; every other grant stays refused.", which, callback, AllowForm)}
}

// loopback reports whether host is the machine itself: localhost, the
// loopback addresses, and the lab domain nip.io resolves to loopback.
func loopback(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".127.0.0.1.nip.io")
}

// signIn reports whether path is an identity provider's sign-in or consent
// page.
func signIn(path string) bool {
	for s := range strings.SplitSeq(path, "/") {
		if slices.Contains(signInSegments, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// consent decides a Chrome call of the event that acts on a page the
// transcript shows, nil when the hook has no record to decide from, the
// call only reads, or the page is unknown.
func (h Hook) consent(ev event) []byte {
	if h.Apps == nil || !isBrowserTool(ev.ToolName) {
		return nil
	}
	tab, acts := actsOn(ev.ToolName, ev.ToolInput)
	if !acts {
		return nil
	}
	page, ok := lastPage(ev.TranscriptPath, tab)
	if !ok {
		return nil
	}
	d := Consent(h.Apps(), page)
	if d.Decision == "" {
		return nil
	}
	return answer(hookOutput{PermissionDecision: d.Decision, Reason: d.Reason})
}

// acting are the computer actions that act on a page rather than read it.
var acting = []string{"left_click", "right_click", "double_click", "triple_click", "left_click_drag", "key", "type"}

// actsOn reports whether the Chrome call acts on a page (a click, a key, a
// typed text, a form value), and on which tab (its tabId, "" when the call
// names none): a batch acts when one of its actions does, on that action's
// tab.
func actsOn(tool string, input map[string]any) (string, bool) {
	t := strings.ToLower(tool)
	if i := strings.LastIndex(t, "__"); i >= 0 {
		t = t[i+len("__"):]
	}
	switch t {
	case chromeForm:
		return tabOf(input[tabKey]), true
	case chromeComputer:
		action, _ := input[actionKey].(string)
		return tabOf(input[tabKey]), slices.Contains(acting, action)
	case chromeBatch:
		actions, _ := input["actions"].([]any)
		for _, a := range actions {
			m, _ := a.(map[string]any)
			name, _ := m[nameKey].(string)
			in, _ := m[inputKey].(map[string]any)
			if tab, acts := actsOn(name, in); acts {
				return tab, true
			}
		}
	}
	return "", false
}

// tabOf is a tabId as the tool wrote it, "" for none.
func tabOf(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	}
	return ""
}

// Every Chrome tool result ends with the tab context, each tab as
// `tabId N: "title" ("url")`, and a tabs_context result lists the same as
// JSON: tabLine and tabJSON read both.
var (
	tabLine = regexp.MustCompile(`tabId (\d+): "(.*?)" \("([^"]*)"\)`)
	tabJSON = regexp.MustCompile(`"tabId":\s*(\d+),\s*"title":\s*"((?:[^"\\]|\\.)*)",\s*"url":\s*"((?:[^"\\]|\\.)*)"`)
)

// transcriptTail bounds how much of a transcript lastPage reads: the tab
// context of the latest results is at its end.
const transcriptTail = 8 << 20

// lastPage is the page of tab as the transcript last showed it: the tab's
// last known URL and title in the tool results, or, for a call that names
// no tab, the one tab the results show.
func lastPage(transcript, tab string) (Page, bool) {
	if transcript == "" {
		return Page{}, false
	}
	f, err := os.Open(transcript) //nolint:gosec // the session's own transcript
	if err != nil {
		return Page{}, false
	}
	defer func() { _ = f.Close() }()
	// The tail's first line is cut: it decodes as no result.
	if fi, err := f.Stat(); err == nil && fi.Size() > transcriptTail {
		if _, err := f.Seek(fi.Size()-transcriptTail, io.SeekStart); err != nil {
			return Page{}, false
		}
	}
	pages := map[string]Page{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		for _, text := range resultTexts(sc.Bytes()) {
			for _, m := range tabLine.FindAllStringSubmatch(text, -1) {
				pages[m[1]] = Page{URL: m[3], Title: m[2]}
			}
			for _, m := range tabJSON.FindAllStringSubmatch(text, -1) {
				pages[m[1]] = Page{URL: unquote(m[3]), Title: unquote(m[2])}
			}
		}
	}
	if tab != "" {
		p, ok := pages[tab]
		return p, ok
	}
	if len(pages) == 1 {
		for _, p := range pages {
			return p, true
		}
	}
	return Page{}, false
}

// resultTexts are the texts of the tool results on one transcript line: a
// string, or the text blocks of an array.
func resultTexts(line []byte) []string {
	var entry struct {
		Message struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return nil
	}
	var out []string
	for _, b := range entry.Message.Content {
		if b.Type != "tool_result" {
			continue
		}
		var s string
		if json.Unmarshal(b.Content, &s) == nil {
			out = append(out, s)
			continue
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(b.Content, &blocks) == nil {
			for _, t := range blocks {
				out = append(out, t.Text)
			}
		}
	}
	return out
}

// unquote decodes the JSON escapes of s, a string's content.
func unquote(s string) string {
	var out string
	if json.Unmarshal([]byte(`"`+s+`"`), &out) != nil {
		return s
	}
	return out
}
