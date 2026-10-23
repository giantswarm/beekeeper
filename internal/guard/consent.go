package guard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
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
// other's refused (google.go); on a third party's OAuth client's consent
// page the hook knows and its device-code page, the consent as the account
// on record (thirdparty.go); on Slack's install consent page of a Slack app
// on record, the Allow that keeps its recorded webhook channel (slack.go);
// a consent page on any other host is refused, so
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
	// CallbackURL is the one callback URL the person asked to add to the
	// App's settings, on a callback host its manifest declares; "" for a
	// record without one (appcallback.go).
	CallbackURL string
	// Delete says the person asked for the App's deletion (applife.go).
	Delete bool
	// Google says the App is a Google OAuth client whose consent is on
	// Google's sign-in (google.go), not a GitHub App.
	Google bool
	// Account is the Google account a Google App's sign-in chooses on
	// Google's account chooser, "" for any.
	Account string
	// GitLab is the host of the self-hosted GitLab whose OAuth application
	// this is, its consent on that host's authorize page (gitlab.go); "" for
	// any other App.
	GitLab string
	// OAuth is the sign-in host of a third party's OAuth client a consent
	// is recorded for as the person's Account there (thirdparty.go); "" for
	// any other App.
	OAuth string
	// Slack is the workspace host (<workspace>.slack.com) of a Slack app of
	// the person's whose install consent is on record there, SlackApp its
	// app id and Channel the webhook channel its install keeps, lower case
	// without "#" ("" for an app without one) (slack.go); Slack is "" for
	// any other App.
	Slack, SlackApp, Channel string
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

// Page is a tab's page: its URL and title, on Google's sign-in the callback
// host of the OAuth flow the tab's earlier pages there named, and on a
// GitLab authorize page the client id its consent form carries, as
// GitLabFormScript read it while the tab was at that URL; on Slack's install
// consent page the channel the last call on the tab that named one named
// there (slack.go).
type Page struct {
	URL, Title string
	Flow       string
	Form       string
	Picked     string
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
	chromePageText   = "get_page_text"
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
		if d, ok := callbackClick(apps, u, words); ok {
			return d
		}
		return permissionClick(apps, u, page.Title, words)
	case host == googleHost:
		return googleClick(apps, u, page, words)
	case thirdPartyHost(host):
		return thirdPartyClick(apps, u, page, words)
	case slackHost(host):
		if d, ok := slackClick(apps, u, page, words); ok {
			return d
		}
		return elsewhere(u, page.Title, words)
	case u.Path == gitlabAuthorize && gitlabHost(apps, host):
		return gitlabConsent(apps, u, page)
	case Loopback(host) && signIn(u.Path):
		return Decision{Allow, fmt.Sprintf("beekeeper allows it: %s is the lab's own identity provider on a loopback host, "+
			"whose fixture users' sign-in and consent are the person's own lab's", u.Host)}
	case !Loopback(host):
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
		"which %s (a GitHub App), %s (a Google OAuth client), %s (a self-hosted GitLab's OAuth application), %s (a Slack app) or %s (a third "+
		"party's OAuth client whose consent page beekeeper knows) records on their word; every other grant stays refused.",
		consentWhich(u, title), u.Host, AllowForm, GoogleForm, GitLabForm, SlackForm, OAuthForm)}
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
		if a.Google || a.GitLab != "" || a.OAuth != "" || !slices.Contains(a.Callbacks, callback) {
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

// Loopback reports whether host is the machine itself: localhost and its
// subdomains, a loopback address (127.0.0.0/8, ::1), and a nip.io name whose
// address is one, which every agentlab binds its own of
// (*.127.0.0.1.nip.io, *.lab2.127.0.0.2.nip.io).
func Loopback(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return ip.IsLoopback()
	}
	m := nipAddr.FindStringSubmatch(h)
	if m == nil {
		return false
	}
	ip, err := netip.ParseAddr(strings.ReplaceAll(m[1]+m[2], "-", "."))
	return err == nil && ip.IsLoopback()
}

// nipAddr is the IPv4 address a nip.io name resolves to, dotted or dashed.
var nipAddr = regexp.MustCompile(`(?:^|[.-])(?:(\d{1,3}(?:\.\d{1,3}){3})|(\d{1,3}(?:-\d{1,3}){3}))\.nip\.io$`)

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
	var checks map[string]pageCheck
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
				if c.typed && d.Decision == Allow {
					d = typedCallback(apps, page, c.text, d)
					d = typedDeviceCode(page, c.text, d)
					d = typedSlack(apps, page, c.text, d)
				}
				// Text typed or set on Google's sign-in is no grant the
				// record covers: the classifier decides it.
				if c.typed && d.Decision == Allow && onGoogle(page) {
					d = Decision{}
				}
				// A click or key the record allows lands only on a page
				// Chrome draws.
				if c.pointer && d.Decision == Allow {
					if checks == nil {
						checks = pageChecks(ev.TranscriptPath)
					}
					if r := drawRefusal(page, tabFor(pages, c.tab), checks); r != "" {
						d = Decision{Deny, r}
					}
				}
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
var acting = []string{actionLeftClick, "right_click", "double_click", "triple_click", "left_click_drag", actionKeyPress, "type"}

// chromeCall is one call of a Chrome tool the hook decides: a navigation to
// a URL, or an action on a tab (its tabId, "" when the call names none) in
// the call's own words (the summary the model wrote, a typed text, a form
// value).
type chromeCall struct {
	navigate, tab, words string
	// text is the input alone: the typed text, the key, the form value.
	text string
	// typed says the call enters input: a typed text, a key, a drag, a
	// form value; anything but a click.
	typed bool
	// pointer says the call is an input event on the page (the computer
	// tool's), which lands only where Chrome draws it; a form value is set
	// on the element.
	pointer bool
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
		return []chromeCall{{tab: tabOf(input[tabKey]), words: wordsOf(input, valueKey), text: valueOf(input, valueKey), typed: true}}
	case chromeComputer:
		if action, _ := input[actionKey].(string); slices.Contains(acting, action) {
			return []chromeCall{{tab: tabOf(input[tabKey]), words: wordsOf(input, textKey), text: valueOf(input, textKey), typed: !strings.HasSuffix(action, "click"), pointer: true}}
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
	return strings.TrimSpace(summary + " " + valueOf(input, key))
}

// valueOf is the value under key of a call's input (a typed text, a key, a
// form value) as text, "" for none.
func valueOf(input map[string]any, key string) string {
	switch v := input[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64, bool:
		return fmt.Sprint(v)
	}
	return ""
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
// named since it came to that host; a page carries the client id
// GitLabFormScript, a call of its own, read from its consent form, unless a
// result of a call that can change the tab's page (changesPage: a click, a
// key, a navigation, another script, such a batch) or a change of its URL
// followed.
func lastPages(transcript string) map[string]Page {
	pages, flows, forms, picks := map[string]Page{}, map[string]googleFlow{}, map[string]pageForm{}, map[string]pagePick{}
	seen := func(tab string, p Page) {
		if forms[tab].url != p.URL {
			delete(forms, tab)
		}
		if picks[tab].url != p.URL {
			delete(picks, tab)
		}
		pages[tab] = p
		u, err := url.Parse(p.URL)
		if err != nil || !strings.EqualFold(u.Hostname(), googleHost) {
			delete(flows, tab)
			return
		}
		f := flows[tab].next(u)
		flows[tab] = f
		p.Flow = f.callback
		pages[tab] = p
	}
	scanJoined(transcript, func(call chromeAction, text string) {
		// The tab context is the tool's trailer of the result: the page's
		// own text before it names no tab.
		if trimmed := strings.TrimSpace(text); strings.HasPrefix(trimmed, tabsJSONOpen) {
			for _, m := range tabJSON.FindAllStringSubmatch(trimmed, -1) {
				seen(m[1], Page{URL: unquote(m[3]), Title: unquote(m[2])})
			}
			return
		}
		// The marker opens a line, which no page title does (a title holds
		// no line break); the last one is the tool's.
		at := contextLine.FindAllStringIndex(text, -1)
		if len(at) == 0 {
			return
		}
		trailer := text[at[len(at)-1][0]:]
		for _, m := range trailerTab.FindAllStringSubmatch(trailer, -1) {
			seen(m[1], Page{URL: m[3], Title: m[2]})
		}
		// The result's own text, before the trailer, is what GitLabFormScript
		// read from the consent form of the tab it executed on, at the URL the
		// trailer names; a call that can change the tab's page (a click, a key,
		// a navigation, another script) leaves no client id the page is known
		// by, a wait, a page read or a tab listing keeps it.
		if m := executedTab.FindStringSubmatch(trailer); m != nil {
			p, ok := pages[m[1]]
			code, _ := call.input[textKey].(string)
			switch {
			case ok && chromeTool(call.name) == chromeScript && isGitLabForm(code):
				if id := gitlabFormID(text[:at[len(at)-1][0]]); id != "" {
					forms[m[1]] = pageForm{p.URL, id}
				} else {
					delete(forms, m[1])
				}
			case !ok || changesPage(call.name, call.input, m[1]):
				delete(forms, m[1])
			}
			// On Slack's install consent page the channel the call's own
			// words name is the one it chose in the webhook picker; a later
			// call that acts on the tab and names none may have changed it.
			if ok && SlackConsentURL(p.URL) {
				for _, c := range chromeCalls(call.name, call.input) {
					if c.tab != "" && c.tab != m[1] {
						continue
					}
					if ch := slackChannelOf(c.words); ch != "" {
						picks[m[1]] = pagePick{p.URL, ch}
					} else {
						delete(picks, m[1])
					}
				}
			}
		}
	})
	for tab, f := range forms {
		p := pages[tab]
		p.Form = f.clientID
		pages[tab] = p
	}
	for tab, f := range picks {
		p := pages[tab]
		p.Picked = f.channel
		pages[tab] = p
	}
	return pages
}

// pageForm is the client id GitLabFormScript read from a tab's consent form
// while it was at url.
type pageForm struct {
	url, clientID string
}

// pagePick is the channel a call on a tab named while it was at url, on
// Slack's install consent page.
type pagePick struct {
	url, channel string
}

// executedTab is the tab a Chrome tool result's call executed on, as its
// tab context names it.
var executedTab = regexp.MustCompile(`(?m)^- Executed on tabId: (\d+)\s*$`)

// trailerTab is one tab of a tab context, one per line: the line's first
// bullet names the tab, its title runs to the line's last `" ("`, so a
// title that names another tab names none.
var trailerTab = regexp.MustCompile(`(?m)^[^\n•]*•\s*tabId (\d+): "(.*)" \("([^"\n]*)"\)[^\S\n]*$`)

// contextLine is the line that opens the tab context a Chrome tool
// appends to its result; tabsJSONOpen opens a tabs_context result.
var contextLine = regexp.MustCompile(`(?m)^Tab Context:`)

const tabsJSONOpen = `{"availableTabs"`

// scanJoined calls fn with the call and the text of each result of a
// Chrome tool call in the transcript's tail, its blocks joined, in order:
// another tool's result, a shell's output say, names no tab.
func scanJoined(transcript string, fn func(call chromeAction, text string)) {
	browser := map[string]chromeAction{}
	scanLines(transcript, func(line []byte) {
		for _, c := range toolCalls(line) {
			if isBrowserTool(c.name) {
				browser[c.id] = c.chromeAction
			}
		}
		for _, r := range resultGroups(line) {
			if call, ok := browser[r.id]; ok {
				fn(call, strings.Join(r.texts, "\n"))
			}
		}
	})
}

// scanResults calls fn with the text of each tool result in the
// transcript's tail, in order; nothing for a transcript that cannot be read.
func scanResults(transcript string, fn func(text string)) {
	scanLines(transcript, func(line []byte) {
		for _, text := range resultTexts(line) {
			fn(text)
		}
	})
}

// scanLines calls fn with each line of the transcript's tail, in order;
// nothing for a transcript that cannot be read.
func scanLines(transcript string, fn func(line []byte)) {
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
		fn(sc.Bytes())
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

// tabFor is tab, or, for a call that names no tab, the one tab of pages.
func tabFor(pages map[string]Page, tab string) string {
	if tab == "" && len(pages) == 1 {
		for t := range pages {
			return t
		}
	}
	return tab
}

// resultTexts are the texts of the tool results on one transcript line: a
// string, or the text blocks of an array.
func resultTexts(line []byte) []string {
	var out []string
	for _, g := range resultGroups(line) {
		out = append(out, g.texts...)
	}
	return out
}

// toolResult is one tool result: the call it answers and its texts.
type toolResult struct {
	id    string
	texts []string
}

// resultGroups are the tool results on one transcript line.
func resultGroups(line []byte) []toolResult {
	var entry struct {
		Message struct {
			Content []struct {
				Type      string          `json:"type"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return nil
	}
	var out []toolResult
	for _, b := range entry.Message.Content {
		if b.Type != "tool_result" {
			continue
		}
		var s string
		if json.Unmarshal(b.Content, &s) == nil {
			out = append(out, toolResult{b.ToolUseID, []string{s}})
			continue
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(b.Content, &blocks) == nil {
			var texts []string
			for _, t := range blocks {
				texts = append(texts, t.Text)
			}
			out = append(out, toolResult{b.ToolUseID, texts})
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
