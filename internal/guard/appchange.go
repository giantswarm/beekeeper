package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// A change of an App's permissions is a grant too: on the App's permissions
// page under the organisation's settings the person selects the level and
// clicks Save changes, and on the organisation's installation of the App
// they click Review request and Accept new permissions. The hook answers
// those calls from the record of the one change the person asked for
// (App.Permission): the navigation to the recorded App's permissions page
// and the calls of that change there are allowed, the permissions page of
// any other App and any other call on the recorded one are refused; on the
// installation's pages a Review request or Accept new permissions that
// names the recorded App, in the page's title or the call's own words, is
// allowed, and every other call there is left to the classifier.

// PermissionForm is how one permission change of an App the person owns and
// asked for is recorded.
const PermissionForm = "`beekeeper app allow <name> --permission \"<scope>: <level>\" --word \"<the person's words>\"`"

// The organisation settings pages of an App on github.com: its permissions
// page, and its installation (the installation's page and the page that
// accepts its new permissions).
var (
	permissionsPath  = regexp.MustCompile(`^/organizations/([^/]+)/settings/apps/([^/]+)/permissions/?$`)
	installationPath = regexp.MustCompile(`^/organizations/([^/]+)/settings/installations/\d+(?:/permissions/update)?/?$`)
)

// levels are the access levels GitHub's permissions page offers, squashed.
var levels = []string{"noaccess", "readonly", "readandwrite"}

// reviewWords are the installation's two clicks that take an App's new
// permissions.
var reviewWords = regexp.MustCompile(`(?i)review request|accept new permissions`)

// notSlug matches what GitHub turns into a hyphen in an App's slug.
var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// AppSlug is the slug GitHub gives an App's name: lower case, every run of
// other characters one hyphen, none at the ends.
func AppSlug(name string) string {
	return strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// Navigation decides a navigation to rawURL from the Apps on record: the
// permissions page of an App on record for a permission change is allowed,
// the permissions page of any other App refused; the Advanced page of an App
// whose deletion is answered (applife.go) and the settings page of an App on
// record for a callback URL (appcallback.go) are allowed; every other URL
// gets no decision.
func Navigation(apps []App, rawURL string) Decision {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Hostname(), githubHost) {
		return Decision{}
	}
	if m := advancedPath.FindStringSubmatch(u.Path); m != nil {
		if a := deletable(apps, m[1], m[2]); a != nil {
			return Decision{Allow, "beekeeper allows it: the Advanced page of " + deleteWords(a)}
		}
		return Decision{}
	}
	if d, ok := callbackNavigation(apps, u.Path); ok {
		return d
	}
	m := permissionsPath.FindStringSubmatch(u.Path)
	if m == nil {
		return Decision{}
	}
	if recs := permissionRecords(apps, m[1], m[2]); len(recs) > 0 {
		return Decision{Allow, "beekeeper allows it: the permissions page of " + changeWords(recs[0])}
	}
	return Decision{Deny, otherPermissionsPage(m[1], m[2])}
}

// permissionClick decides a call that acts on GitHub's page u, in the
// call's own words, where the page is an App's permissions page or an
// installation's; no decision for any other page of github.com.
func permissionClick(apps []App, u *url.URL, title, words string) Decision {
	if m := permissionsPath.FindStringSubmatch(u.Path); m != nil {
		recs := permissionRecords(apps, m[1], m[2])
		if len(recs) == 0 {
			return Decision{Deny, otherPermissionsPage(m[1], m[2])}
		}
		changes := make([]string, 0, len(recs))
		for _, a := range recs {
			if savesChanges(words) || namesChange(a, words) {
				return Decision{Allow, "beekeeper allows the call: the permissions page of " + changeWords(a)}
			}
			changes = append(changes, a.Permission)
		}
		return Decision{Deny, fmt.Sprintf("Refused: on the permissions page of the person's own App %s the call %q is neither a permission change "+
			"on record (%s) nor Save changes; every other change there stays refused.", recs[0].Name, short(words), strings.Join(changes, "; "))}
	}
	if m := installationPath.FindStringSubmatch(u.Path); m != nil && reviewWords.MatchString(words) {
		for i := range apps {
			if a := &apps[i]; a.Permission != "" && strings.EqualFold(a.Org, m[1]) && (containsFold(title, a.Name) || containsFold(words, a.Name)) {
				return Decision{Allow, "beekeeper allows the click: the installation of " + changeWords(a)}
			}
		}
	}
	return Decision{}
}

// permissionRecords are the records of permission changes of the App
// whose permissions page under org's settings slug names, none for an App
// on no such record.
func permissionRecords(apps []App, org, slug string) []*App {
	var out []*App
	for i := range apps {
		if a := &apps[i]; a.Permission != "" && strings.EqualFold(a.Org, org) && strings.EqualFold(AppSlug(a.Name), slug) {
			out = append(out, a)
		}
	}
	return out
}

// changeWords name the record of a permission change: the App, where it is
// declared, the change, and the person's word.
func changeWords(a *App) string {
	return fmt.Sprintf("the person's own App %s, declared in %s, on record for the change %s since %s by %s: %s",
		a.Name, a.Declared, a.Permission, a.At.UTC().Format("2006-01-02"), a.By, a.Word)
}

// otherPermissionsPage is the refusal of the permissions page of an App not
// on record for a change.
func otherPermissionsPage(org, slug string) string {
	return fmt.Sprintf("Refused: the permissions page of the App %s of %s is not on record for a permission change. A change of an App's "+
		"permissions is a grant, answered only for the one change the person asked for, which %s records on their word; every other "+
		"grant stays refused.", slug, org, PermissionForm)
}

// namesChange reports whether a call's words name the recorded change: the
// permission's scope, and no other level than the recorded one (a click
// that opens the row's levels names the scope alone; one that selects a
// level names both).
func namesChange(a *App, words string) bool {
	scope, level, _ := strings.Cut(a.Permission, ":")
	w, scope, level := squash(words), squash(scope), squash(level)
	if scope == "" || !strings.Contains(w, scope) {
		return false
	}
	for _, l := range levels {
		if l != level && strings.Contains(w, l) {
			return false
		}
	}
	return true
}

// savesChanges reports whether a call's words name the page's Save.
func savesChanges(words string) bool { return strings.Contains(strings.ToLower(words), "save") }

// squash is s in lower case with everything but letters and digits dropped,
// so that "Read-only", "read only" and "Read-Only" compare equal.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// containsFold reports whether s contains sub, case-insensitively; never
// for an empty sub.
func containsFold(s, sub string) bool {
	return sub != "" && strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// permissionDisclosure requires both a UI-only description and a page read
// identifying the actual ref as that disclosure's button or tab. A summary
// alone cannot turn a denied grant into an allowed click.
var permissionDisclosureWords = regexp.MustCompile(`(?i)^(expand|expands|collapse|collapses|switch to|switches to) (?:the )?((?:organization|repository|account) permissions) (section|tab)\.?$`)

var permissionControl = regexp.MustCompile(`(?i)^\s*(?:-\s*)?(?:(button|tab) "((?:organization|repository|account) permissions)" \[(ref_\d+)\]|(ref_\d+): (button|tab) "((?:organization|repository|account) permissions)")(?:\s.*)?$`)

func permissionDisclosure(page Page, words, target, ref string) bool {
	u, err := url.Parse(page.URL)
	if err != nil || !strings.EqualFold(u.Hostname(), githubHost) || !permissionsPath.MatchString(u.Path) {
		return false
	}
	step := permissionDisclosureWords.FindStringSubmatch(strings.TrimSpace(words))
	control := permissionControl.FindStringSubmatch(target)
	if step == nil || control == nil || control[3]+control[4] != ref {
		return false
	}
	role, label := control[1]+control[5], control[2]+control[6]
	switching := strings.HasPrefix(strings.ToLower(step[1]), "switch")
	return strings.EqualFold(label, step[2]) &&
		((switching && strings.EqualFold(step[3], "tab") && strings.EqualFold(role, "tab")) ||
			(!switching && strings.EqualFold(step[3], "section") && strings.EqualFold(role, "button")))
}

// permissionRef returns the last page read's line for ref, only on the
// clicked tab at its current URL. A navigation, input or other page-changing
// call invalidates the evidence even when its result has not arrived yet.
// This uses the same ref-line format as the subscription guard, scoped to
// a matched Chrome read_page/find call rather than unrelated tool output.
func permissionRef(transcript, tab, rawURL, ref string) string {
	if tab == "" || ref == "" {
		return ""
	}
	var target string
	var revision int
	type readCall struct {
		chromeAction
		revision int
	}
	calls := map[string]readCall{}
	scanLines(transcript, func(line []byte) {
		for _, c := range toolCalls(line) {
			if !isBrowserTool(c.name) {
				continue
			}
			if changesPage(c.name, c.input, tab) {
				revision++
				target = ""
			}
			calls[c.id] = readCall{c.chromeAction, revision}
		}
		for _, r := range resultGroups(line) {
			c, ok := calls[r.id]
			if !ok || c.revision != revision || tabOf(c.input[tabKey]) != tab || (chromeTool(c.name) != chromeReadPage && chromeTool(c.name) != chromeFind) {
				continue
			}
			target = ""
			text := strings.Join(r.texts, "\n")
			at := contextLine.FindAllStringIndex(text, -1)
			if len(at) == 0 {
				continue
			}
			cut := at[len(at)-1][0]
			matched := false
			for _, m := range trailerTab.FindAllStringSubmatch(text[cut:], -1) {
				if m[1] == tab && m[3] == rawURL {
					matched = true
				}
			}
			if !matched {
				continue
			}
			for l := range strings.SplitSeq(text[:cut], "\n") {
				for _, found := range refPattern.FindAllString(l, -1) {
					if found == ref {
						target = l
					}
				}
			}
		}
	})
	return target
}
