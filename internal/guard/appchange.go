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
// whose deletion is answered (applife.go) is allowed; every other URL gets
// no decision.
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
	m := permissionsPath.FindStringSubmatch(u.Path)
	if m == nil {
		return Decision{}
	}
	if a := permissionRecord(apps, m[1], m[2]); a != nil {
		return Decision{Allow, "beekeeper allows it: the permissions page of " + changeWords(a)}
	}
	return Decision{Deny, otherPermissionsPage(m[1], m[2])}
}

// permissionClick decides a call that acts on GitHub's page u, in the
// call's own words, where the page is an App's permissions page or an
// installation's; no decision for any other page of github.com.
func permissionClick(apps []App, u *url.URL, title, words string) Decision {
	if m := permissionsPath.FindStringSubmatch(u.Path); m != nil {
		a := permissionRecord(apps, m[1], m[2])
		if a == nil {
			return Decision{Deny, otherPermissionsPage(m[1], m[2])}
		}
		if savesChanges(words) || namesChange(a, words) {
			return Decision{Allow, "beekeeper allows the call: the permissions page of " + changeWords(a)}
		}
		return Decision{Deny, fmt.Sprintf("Refused: on the permissions page of the person's own App %s the call %q is neither the one permission change "+
			"on record (%s) nor Save changes; every other change there stays refused.", a.Name, short(words), a.Permission)}
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

// permissionRecord is the App on record for a permission change whose
// permissions page under org's settings slug names, nil for none.
func permissionRecord(apps []App, org, slug string) *App {
	for i := range apps {
		if a := &apps[i]; a.Permission != "" && strings.EqualFold(a.Org, org) && strings.EqualFold(AppSlug(a.Name), slug) {
			return a
		}
	}
	return nil
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
