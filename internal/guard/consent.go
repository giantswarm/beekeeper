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
// from the Apps on record (Hook.Apps): Apps the organisation declares, on the
// person's word. On GitHub's consent page of a recorded App, with one of its
// declared callback hosts, the click is allowed; on
// GitHub's consent page of any other App or callback host it is refused; on
// the lab's own identity provider, a loopback host, a fixture user's sign-in
// and consent are allowed; on Google's sign-in, the consent of a Google App
// on record, by the callback host of the tab's flow, is allowed and any
// other's refused (google.go); a consent page on any other host is refused, so
// no grant elsewhere is left to the browse turn's classifier. A change of an
// App's permissions is decided the same way from the record of the one
// change the person asked for (appchange.go), an App's creation,
// installation and deletion from the running app create, beekeeper's ledger
// and the person's record (applife.go). Any other page gets no decision, as
// does a call whose page the transcript does not show.

// App is an App the person owns and asked a consent, or one permission
// change, for.
type App struct {
	// Name is the App's name as GitHub's consent page titles it.
	Name string
	// ClientID is the OAuth client id the consent URL carries, "" for a
	// record without a consent.
	ClientID string
	// Callbacks are the hosts the consent page's redirect may go to, none
	// for a record without a consent.
	Callbacks []string
	// Org is the organisation whose settings pages the record covers, the
	// owner of the declaring repository.
	Org string
	// Permission is the one change of the App's permissions the person
	// asked for, "<scope>: <level>"; "" for a record without one.
	Permission string
	// Delete says the person asked for the App's deletion (applife.go).
	Delete bool
	// Google says the App is a Google OAuth client whose consent is on
	// Google's sign-in (google.go), not a GitHub App.
	Google bool
	// Run is the running `beekeeper app create` of the App whose listener
	// answers, nil for none; Created is beekeeper's ledger entry of the App
	// it created, nil for none (applife.go).
	Run     *CreateRun
	Created *Created
	// Declared is where the organisation declares the App.
	Declared string
	// Word is the person's words that asked for the grant, By whom and At
	// when they were recorded.
	Word, By string
	At       time.Time
}

// Page is a tab's page: its URL and title, and on Google's sign-in the
// callback host of the OAuth flow the tab's earlier pages there named.
type Page struct {
	URL, Title string
	Flow       string
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

// A consent page elsewhere is one whose path names a consent (consentSegments)
// or whose title names a grant (grantTitle), where any call acting on it is a
// grant or its refusal; or an OAuth authorization page (authorizeSegments, or
// an authorization request's client_id with its response_type or
// redirect_uri), which also signs in, where only a call whose own words name a
// grant (grantWords) is one.
var (
	consentSegments   = strings.Fields("approval approve consent consents grant")
	authorizeSegments = strings.Fields("authorize authorization oauth oauth2")
	grantTitle        = regexp.MustCompile(`(?i)^authori[sz]e\b|grant access|consent|permissions requested`)
	grantWords        = regexp.MustCompile(`(?i)\b(authori[sz]e|allow|grant|approve|accept|consent)`)
)

// chromeRedacted is what the Claude in Chrome tools put in place of an
// auth-like value of a URL they report (a client id, a state); authorizeTitle
// opens GitHub's consent page's title.
const (
	chromeRedacted = "REDACTED"
	authorizeTitle = "Authorize "
)

// The Chrome tools that act on a page or open one (their names after the
// server's prefix), and the keys of their inputs: the tab a call acts on,
// the URL a navigate opens, the summary the model wrote for a call, a batch
// action's name and input.
const (
	chromeComputer   = "computer"
	chromeBatch      = "browser_batch"
	chromeForm       = "form_input"
	chromeNavigation = "navigate"
	tabKey           = "tabId"
	urlKey           = "url"
	summaryKey       = "action_summary"
	textKey          = "text"
	valueKey         = "value"
	nameKey          = "name"
	inputKey         = "input"
)

// AllowForm is how a consent for an App the person owns and asked for is
// recorded.
const AllowForm = "`beekeeper app allow <name> --client-id <id> --word \"<the person's words>\"`"

// Click decides a call that acts on page (a click, a key, a typed text, a
// form value), in the call's own words, from the Apps on record.
func Click(apps []App, page Page, words string) Decision {
	u, err := url.Parse(strings.TrimSpace(page.URL))
	if err != nil {
		return Decision{}
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == githubHost && u.Path == githubAuthorize:
		return githubConsent(apps, u, page.Title)
	case host == githubHost:
		if d, ok := lifecycleClick(apps, u, page.Title, words); ok {
			return d
		}
		return permissionClick(apps, u, page.Title, words)
	case host == googleHost:
		return googleClick(apps, u, page, words)
	case loopback(host) && signIn(u.Path):
		return Decision{Allow, fmt.Sprintf("beekeeper allows it: %s is the lab's own identity provider on a loopback host, "+
			"whose fixture users' sign-in and consent are the person's own lab's", u.Host)}
	case !loopback(host):
		return elsewhere(u, page.Title, words)
	}
	return Decision{}
}

// elsewhere decides a call in words on a page at u on a host no record
// covers: refused on a consent page, no decision on any other.
func elsewhere(u *url.URL, title, words string) Decision {
	if !consentElsewhere(u, title, words) {
		return Decision{}
	}
	return Decision{Deny, fmt.Sprintf("Refused: %s is a consent page (an OAuth grant) on %s, neither the consent page of an App on record "+
		"nor the lab's own identity provider on a loopback host. A consent is answered only for an App the person owns and asked for, "+
		"which %s (a GitHub App) or %s (a Google OAuth client) records on their word; every other grant stays refused.",
		consentWhich(u, title), u.Host, AllowForm, GoogleForm)}
}

// consentElsewhere reports whether a call in words acts on a consent page:
// any call on a page whose path or title names a consent, a call whose words
// name a grant on an OAuth authorization page.
func consentElsewhere(u *url.URL, title, words string) bool {
	if hasSegment(u.Path, consentSegments) || grantTitle.MatchString(strings.TrimSpace(title)) {
		return true
	}
	q := u.Query()
	authorizing := hasSegment(u.Path, authorizeSegments) || (q.Get("client_id") != "" && (q.Get("response_type") != "" || q.Get("redirect_uri") != ""))
	return authorizing && grantWords.MatchString(words)
}

// consentWhich names a consent page: its title, else its path.
func consentWhich(u *url.URL, title string) string {
	if t := strings.TrimSpace(title); t != "" {
		return strconv.Quote(t)
	}
	return u.Path
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
		if a.Google || !slices.Contains(a.Callbacks, callback) {
			continue
		}
		if (clientID != "" && clientID == a.ClientID) || (clientID == chromeRedacted && strings.EqualFold(title, authorizeTitle+a.Name)) {
			return Decision{Allow, fmt.Sprintf("beekeeper allows the click: the consent page of the person's own App %s (callback %s), "+
				"declared in %s, on record since %s by %s: %s", a.Name, callback, a.Declared, a.At.UTC().Format("2006-01-02"), a.By, a.Word)}
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
	return hasSegment(path, signInSegments)
}

// hasSegment reports whether one of path's segments is among segments.
func hasSegment(path string, segments []string) bool {
	for s := range strings.SplitSeq(path, "/") {
		if slices.Contains(segments, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// consent decides a Chrome call of the event from the Apps on record: each
// navigation it makes and each call that acts on a page the transcript
// shows. A refused one refuses the call; the call is allowed when every one
// of them is allowed; nil when the hook has no record to decide from, the
// call only reads, a page is unknown, or one of them is not the hook's to
// decide.
func (h Hook) consent(ev event) []byte {
	if h.Apps == nil || !isBrowserTool(ev.ToolName) {
		return nil
	}
	calls := chromeCalls(ev.ToolName, ev.ToolInput)
	if len(calls) == 0 {
		return nil
	}
	apps := h.Apps()
	var pages map[string]Page
	allowed := ""
	for _, c := range calls {
		var d Decision
		if c.navigate != "" {
			d = Navigation(apps, c.navigate)
		} else {
			if pages == nil {
				pages = lastPages(ev.TranscriptPath)
			}
			if page, ok := pageOf(pages, c.tab); ok {
				d = Click(apps, page, c.words)
			}
		}
		switch d.Decision {
		case Deny:
			return answer(hookOutput{PermissionDecision: Deny, Reason: d.Reason})
		case Allow:
			if allowed == "" {
				allowed = d.Reason
			}
		default:
			return nil
		}
	}
	return answer(hookOutput{PermissionDecision: Allow, Reason: allowed})
}

// acting are the computer actions that act on a page rather than read it.
var acting = []string{"left_click", "right_click", "double_click", "triple_click", "left_click_drag", "key", "type"}

// chromeCall is one call of a Chrome tool the hook decides: a navigation to
// a URL, or an action on a tab (its tabId, "" when the call names none) in
// the call's own words (the summary the model wrote, a typed text, a form
// value).
type chromeCall struct {
	navigate, tab, words string
}

// chromeCalls are the calls of a Chrome tool call the hook decides: a
// navigate, a click, a key, a typed text, a form value, and each of a
// batch's; none for a call that only reads.
func chromeCalls(tool string, input map[string]any) []chromeCall {
	switch chromeTool(tool) {
	case chromeNavigation:
		if u, _ := input[urlKey].(string); u != "" {
			return []chromeCall{{navigate: u}}
		}
	case chromeForm:
		return []chromeCall{{tab: tabOf(input[tabKey]), words: wordsOf(input, valueKey)}}
	case chromeComputer:
		if action, _ := input[actionKey].(string); slices.Contains(acting, action) {
			return []chromeCall{{tab: tabOf(input[tabKey]), words: wordsOf(input, textKey)}}
		}
	case chromeBatch:
		actions, _ := input["actions"].([]any)
		var calls []chromeCall
		for _, a := range actions {
			m, _ := a.(map[string]any)
			name, _ := m[nameKey].(string)
			in, _ := m[inputKey].(map[string]any)
			calls = append(calls, chromeCalls(name, in)...)
		}
		return calls
	}
	return nil
}

// wordsOf are a call's own words: the summary the model wrote for it and
// the value under key (a typed text, a form value).
func wordsOf(input map[string]any, key string) string {
	summary, _ := input[summaryKey].(string)
	value := ""
	switch v := input[key].(type) {
	case string:
		value = v
	case json.Number:
		value = v.String()
	case float64, bool:
		value = fmt.Sprint(v)
	}
	return strings.TrimSpace(summary + " " + value)
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
	return pageOf(lastPages(transcript), tab)
}

// lastPages are the pages of the tabs as the transcript last showed them,
// by tabId; none for a transcript that cannot be read. A page on Google's
// sign-in carries the callback host of the flow the tab's pages there
// named since it came to that host.
func lastPages(transcript string) map[string]Page {
	pages, flows := map[string]Page{}, map[string]string{}
	seen := func(tab string, p Page) {
		u, err := url.Parse(p.URL)
		if err != nil || !strings.EqualFold(u.Hostname(), googleHost) {
			delete(flows, tab)
		} else if cb := GoogleCallback(u); cb != "" {
			flows[tab] = cb
		}
		p.Flow = flows[tab]
		pages[tab] = p
	}
	scanResults(transcript, func(text string) {
		for _, m := range tabLine.FindAllStringSubmatch(text, -1) {
			seen(m[1], Page{URL: m[3], Title: m[2]})
		}
		for _, m := range tabJSON.FindAllStringSubmatch(text, -1) {
			seen(m[1], Page{URL: unquote(m[3]), Title: unquote(m[2])})
		}
	})
	return pages
}

// scanResults calls fn with the text of each tool result in the
// transcript's tail, in order; nothing for a transcript that cannot be read.
func scanResults(transcript string, fn func(text string)) {
	if transcript == "" {
		return
	}
	f, err := os.Open(transcript) //nolint:gosec // the session's own transcript
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	// The tail's first line is cut: it decodes as no result.
	if fi, err := f.Stat(); err == nil && fi.Size() > transcriptTail {
		if _, err := f.Seek(fi.Size()-transcriptTail, io.SeekStart); err != nil {
			return
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		for _, text := range resultTexts(sc.Bytes()) {
			fn(text)
		}
	}
}

// pageOf is the page of tab among pages, or, for a call that names no tab,
// the one page there is.
func pageOf(pages map[string]Page, tab string) (Page, bool) {
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
