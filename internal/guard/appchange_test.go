package guard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The App on record for one permission change, a device-flow App without
// a consent, and the pages of the organisation's settings the decision is
// tested on: its permissions page (titled as GitHub does, and as the
// sudo prompt served there is), another App's, the installation's page and
// the page that accepts its new permissions.
const (
	deviceApp         = "Example Lab Devctl"
	deviceSlug        = "example-lab-devctl"
	deviceOrg         = "example"
	deviceChange      = "Members: Read-only"
	deviceDeclared    = "example/github@0123456789ab:apps/" + deviceSlug + "/manifest.json"
	permissionsPage   = "https://github.com/organizations/" + deviceOrg + "/settings/apps/" + deviceSlug + "/permissions"
	permissionsTitle  = "GitHub Apps - " + deviceApp
	sudoTitle         = "Confirm access"
	otherAppsPage     = "https://github.com/organizations/" + deviceOrg + "/settings/apps/other-app/permissions"
	otherOrgPage      = "https://github.com/organizations/other/settings/apps/" + deviceSlug + "/permissions"
	installPage       = "https://github.com/organizations/" + deviceOrg + "/settings/installations/12345"
	acceptPage        = installPage + "/permissions/update"
	selectsReadOnly   = "Selects Read-only for Members access"
	opensMembers      = "Opens the Members access dropdown"
	savesChangesWord  = "Clicks Save changes"
	reviewRequest     = "Clicks Review request for " + deviceApp
	acceptsNew        = "Clicks Accept new permissions"
	refKey            = "ref"
	actionsKey        = "actions"
	refTwelve         = "ref_12"
	applicationsTitle = "Applications"
	notTheChange      = "neither the one permission change"
	readLevel         = "read"
)

var changeRecorded = []App{{Name: deviceApp, Org: deviceOrg, Permission: deviceChange, Declared: deviceDeclared,
	Word: "make sure beekeeper handles such permission grants with the hook", By: "the guide", At: time.Date(2026, 10, 10, 10, 20, 0, 0, time.UTC)}}

// A navigation to the permissions page of the App on record is allowed
// with the record; to the permissions page of another App, or the same
// slug under another organisation, refused naming the record's form; a
// navigation anywhere else, and one without a record, gets no decision.
func TestNavigation(t *testing.T) {
	for name, tc := range map[string]struct {
		url      string
		apps     []App
		decision string
		reason   string
	}{
		"the recorded App's permissions page": {permissionsPage, changeRecorded, Allow, "the permissions page of the person's own App " + deviceApp + ", declared in " + deviceDeclared + ", on record for the change " + deviceChange + " since 2026-10-10 by the guide: make sure"},
		"with a trailing slash":               {permissionsPage + "/", changeRecorded, Allow, deviceApp},
		"another App's":                       {otherAppsPage, changeRecorded, Deny, "the permissions page of the App other-app of " + deviceOrg + " is not on record for a permission change"},
		"another organisation's":              {otherOrgPage, changeRecorded, Deny, PermissionForm},
		"a consent record only":               {permissionsPage, recorded, Deny, PermissionForm},
		"no record at all":                    {permissionsPage, nil, Deny, PermissionForm},
		"the App's settings page":             {"https://github.com/organizations/" + deviceOrg + "/settings/apps/" + deviceSlug, changeRecorded, "", ""},
		"the installations list":              {"https://github.com/organizations/" + deviceOrg + "/settings/installations", changeRecorded, "", ""},
		"the consent page":                    {ownPage, recorded, "", ""},
		"another site":                        {"https://example.org/organizations/" + deviceOrg + "/settings/apps/" + deviceSlug + "/permissions", changeRecorded, "", ""},
		"back":                                {"back", changeRecorded, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := Navigation(tc.apps, tc.url)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Navigation = %+v, want %q with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// On the recorded App's permissions page the calls of the change are
// allowed: one naming the permission (the row opened, the level selected,
// a form value), Save changes; any other call there is refused (another
// level, another permission, a key), as is any call on the permissions
// page of another App. On the installation's pages Review request and
// Accept new permissions are allowed when the page's title or the call
// names the App on record, and left undecided otherwise, as is any other
// call there.
func TestPermissionClick(t *testing.T) {
	for name, tc := range map[string]struct {
		page     Page
		words    string
		apps     []App
		decision string
		reason   string
	}{
		"selects the level":            {Page{permissionsPage, permissionsTitle}, selectsReadOnly, changeRecorded, Allow, "the permissions page of the person's own App " + deviceApp + ", declared in " + deviceDeclared + ", on record for the change " + deviceChange},
		"opens the row":                {Page{permissionsPage, permissionsTitle}, opensMembers, changeRecorded, Allow, deviceApp},
		"a form value":                 {Page{permissionsPage, permissionsTitle}, "Sets Members to Read-only read", changeRecorded, Allow, deviceApp},
		"the level spelt otherwise":    {Page{permissionsPage, permissionsTitle}, "Members: read only", changeRecorded, Allow, deviceApp},
		"saves":                        {Page{permissionsPage, permissionsTitle}, savesChangesWord, changeRecorded, Allow, deviceApp},
		"another level":                {Page{permissionsPage, permissionsTitle}, "Selects Read and write for Members access", changeRecorded, Deny, `the call "Selects Read and write for Members access" is ` + notTheChange + " on record (" + deviceChange + ") nor Save changes"},
		"no access":                    {Page{permissionsPage, permissionsTitle}, "Selects No access for Members", changeRecorded, Deny, notTheChange},
		"another permission":           {Page{permissionsPage, permissionsTitle}, "Selects Read-only for Administration", changeRecorded, Deny, notTheChange},
		"a key":                        {Page{permissionsPage, permissionsTitle}, "Enter", changeRecorded, Deny, notTheChange},
		"nothing said":                 {Page{permissionsPage, permissionsTitle}, "", changeRecorded, Deny, notTheChange},
		"the sudo prompt there":        {Page{permissionsPage, sudoTitle}, "Clicks Confirm", changeRecorded, Deny, notTheChange},
		"another App's page":           {Page{otherAppsPage, "GitHub Apps - other-app"}, selectsReadOnly, changeRecorded, Deny, "the permissions page of the App other-app of " + deviceOrg + " is not on record"},
		"a consent record only":        {Page{permissionsPage, permissionsTitle}, selectsReadOnly, recorded, Deny, PermissionForm},
		"review request, titled":       {Page{installPage, "Installed GitHub Apps - " + deviceApp}, "Clicks Review request", changeRecorded, Allow, "the installation of the person's own App " + deviceApp},
		"review request, named":        {Page{installPage, "Organization settings"}, reviewRequest, changeRecorded, Allow, deviceApp},
		"accept new permissions":       {Page{acceptPage, permissionsTitle}, acceptsNew, changeRecorded, Allow, deviceApp},
		"accept, App not named":        {Page{acceptPage, "Organization settings"}, acceptsNew, changeRecorded, "", ""},
		"accept, another organisation": {Page{strings.Replace(acceptPage, deviceOrg, "other", 1), permissionsTitle}, acceptsNew, changeRecorded, "", ""},
		"another click there":          {Page{installPage, "Installed GitHub Apps - " + deviceApp}, "Clicks Uninstall", changeRecorded, "", ""},
		"the installations list":       {Page{"https://github.com/organizations/" + deviceOrg + "/settings/installations", "Installed GitHub Apps"}, "Clicks Configure for " + deviceApp, changeRecorded, "", ""},
		"another GitHub page":          {Page{applicationsPage, applicationsTitle}, selectsReadOnly, changeRecorded, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := Click(tc.apps, tc.page, tc.words)
			if d.Decision != tc.decision || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("Click = %+v, want %q with %q", d, tc.decision, tc.reason)
			}
		})
	}
}

// The hook decides the Chrome calls of a browse turn from the record: the
// navigation to the recorded App's permissions page is allowed and to
// another App's refused, a click or a form value on the page the
// transcript shows is decided in the call's own words, a batch is allowed
// only when each of its calls is and refused when one is, and a batch with
// a call the hook cannot decide is left to the classifier.
func TestPermissionDecision(t *testing.T) {
	apps := func() []App { return changeRecorded }
	own := transcriptWith(t, Page{permissionsPage, permissionsTitle}, false)
	install := transcriptWith(t, Page{installPage, "Installed GitHub Apps - " + deviceApp}, true)
	navigate := func(u string) map[string]any { return map[string]any{urlKey: u} }
	selects := tabInput(leftClick, map[string]any{refKey: refTwelve, summaryKey: selectsReadOnly})
	writes := tabInput(leftClick, map[string]any{refKey: refTwelve, summaryKey: "Selects Read and write for Members access"})
	saves := tabInput(leftClick, map[string]any{summaryKey: savesChangesWord})
	form := map[string]any{refKey: refTwelve, valueKey: readLevel, summaryKey: "Sets Members access to Read-only", tabKey: json.Number(tab7)}
	batchOf := func(actions ...map[string]any) map[string]any {
		var all []any
		for _, a := range actions {
			all = append(all, a)
		}
		return map[string]any{actionsKey: all}
	}
	for name, tc := range map[string]struct {
		tool       string
		input      map[string]any
		transcript string
		decision   string
		reason     string
	}{
		"navigate to the recorded page":   {browserTools + chromeNavigation, navigate(permissionsPage), own, decisionAllow, "the permissions page of the person's own App " + deviceApp},
		"navigate to another App's":       {browserTools + chromeNavigation, navigate(otherAppsPage), own, decisionDeny, PermissionForm},
		"navigate elsewhere":              {browserTools + chromeNavigation, navigate("https://example.org/"), own, "", ""},
		"a click that selects the level":  {browserTools + chromeComputer, selects, own, decisionAllow, deviceChange},
		"a click that selects another":    {browserTools + chromeComputer, writes, own, decisionDeny, notTheChange},
		"a form value":                    {browserTools + chromeForm, form, own, decisionAllow, deviceApp},
		"a batch of the change":           {browserTools + chromeBatch, batchOf(batchAction(chromeComputer, selects), batchAction(chromeComputer, tabInput("screenshot", nil)), batchAction(chromeComputer, saves)), own, decisionAllow, deviceApp},
		"a batch with another level":      {browserTools + chromeBatch, batchOf(batchAction(chromeComputer, selects), batchAction(chromeComputer, writes)), own, decisionDeny, notTheChange},
		"a batch with an undecided call":  {browserTools + chromeBatch, batchOf(batchAction(chromeNavigation, navigate(permissionsPage)), batchAction(chromeNavigation, navigate("https://example.org/"))), own, "", ""},
		"a batch of navigate and click":   {browserTools + chromeBatch, batchOf(batchAction(chromeNavigation, navigate(permissionsPage)), batchAction(chromeComputer, selects)), own, decisionAllow, deviceApp},
		"review request on the install":   {browserTools + chromeComputer, tabInput(leftClick, map[string]any{summaryKey: "Clicks Review request"}), install, decisionAllow, "the installation of the person's own App " + deviceApp},
		"another click on the install":    {browserTools + chromeComputer, tabInput(leftClick, map[string]any{summaryKey: "Clicks Uninstall"}), install, "", ""},
		"a click on a page not shown":     {browserTools + chromeComputer, selects, "", "", ""},
		"a consent click, record unmoved": {browserTools + chromeComputer, tabInput(leftClick, map[string]any{summaryKey: clicksAuthorize}), transcriptWith(t, Page{redactedPage, ownTitle}, false), decisionDeny, AllowForm},
	} {
		t.Run(name, func(t *testing.T) {
			ev := toolEvent(tc.tool, tc.input)
			ev["permission_mode"] = "dontAsk"
			ev[sessionKey] = "browse-turn"
			ev["transcript_path"] = tc.transcript
			d := decideEvent(t, Hook{Apps: apps}, ev)
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

// GitHub's slug of an App's name.
func TestAppSlug(t *testing.T) {
	for name, want := range map[string]string{deviceApp: deviceSlug, deviceSlug: deviceSlug, "  My_App 2!": "my-app-2"} {
		if got := AppSlug(name); got != want {
			t.Errorf("AppSlug(%q) = %q, want %q", name, got, want)
		}
	}
}
