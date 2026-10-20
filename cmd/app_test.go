package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The consent URL of the refused click, as the Chrome tools reported it,
// the record that covers it, and the App command's words.
const (
	recordedApp      = "example-lab-workspaces"
	recordedClientID = "Iv1.0123456789abcdef"
	recordedCallback = "workspace-manager.127.0.0.1.nip.io"
	recordedTitle    = "Authorize " + recordedApp
	recordedConsent  = "https://github.com/login/oauth/authorize?client_id=REDACTED&code_challenge=REDACTED&code_challenge_method=S256" +
		"&redirect_uri=https%3A%2F%2Fworkspace-manager.127.0.0.1.nip.io%2Fcallback%2Fgithub&response_type=code&state=REDACTED"
	personsWord    = "your agent has to do this"
	appAllow       = "allow"
	appCheck       = "check"
	titleFlag      = "--title"
	clientIDFlag   = "--client-id"
	wordFlag       = "--word"
	permissionFlag = "--permission"
	wordsFlag      = "--words"
	navigateFlag   = "--navigate"
	googleFlag     = "--google"
	callbackFlag   = "--callback"
	accountFlag    = "--account"
)

// app check's answers, and the calls and the page of an App's life it
// decides.
const (
	checkAllow   = "allow: "
	checkRefuse  = "refuse: "
	checkNone    = "no decision: "
	clickInstall = "Clicks Install"
	clickDelete  = "Clicks Delete GitHub App"
	halfAdvanced = "https://github.com/settings/apps/example-lab-half/advanced"
)

// The device-flow App on record for one permission change: its manifest
// carries no callback URL; the pages the hook decides for it.
const (
	deviceApp        = "example-lab-devctl"
	deviceChange     = "Members: Read-only"
	deviceManifest   = `{"name": "` + deviceApp + `", "url": "https://example.org", "default_permissions": {"members": "read"}}`
	deviceDeclared   = declaringRepo + "@0123456789ab:apps/" + deviceApp + "/manifest.json"
	permissionsPage  = "https://github.com/organizations/example/settings/apps/" + deviceApp + "/permissions"
	permissionsTitle = "GitHub Apps - " + deviceApp
	selectsReadOnly  = "Selects Read-only for Members access"
)

// The organisation's declaring repository, the commit its default branch
// is at, and where it declares the recorded App.
const (
	declaringRepo    = "example/github"
	declaringCommit  = "0123456789abcdef0123"
	recordedDeclared = declaringRepo + "@0123456789ab:apps/" + recordedApp + "/manifest.json"
)

// allowArgs are `app allow`'s arguments for the recorded App with the
// person's words.
func allowArgs(word string) []string {
	return []string{appAllow, recordedApp, clientIDFlag, recordedClientID, wordFlag, word}
}

// declareApps has a's configuration name the declaring repository and its
// gh read the manifests there: path to content, any other path missing.
func declareApps(t *testing.T, a *app, manifests map[string]string) {
	t.Helper()
	a.cfg.Apps = config.Apps{Repo: declaringRepo, Manifest: "apps/" + config.AppName + "/manifest.json"}
	orig := appsGH
	t.Cleanup(func() { appsGH = orig })
	appsGH = func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		if strings.Contains(call, "repos/"+declaringRepo+"/commits/HEAD") {
			return []byte(declaringCommit + "\n"), nil
		}
		for path, m := range manifests {
			if strings.Contains(call, "repos/"+declaringRepo+"/contents/"+path+"?ref="+declaringCommit) {
				return []byte(m), nil
			}
		}
		return nil, errors.New("gh: Not Found (HTTP 404)")
	}
}

// recordedManifest declares the recorded App with its two callback URLs.
const recordedManifest = `{"name": "` + recordedApp + `", "url": "https://example.org", "callback_urls": ["https://` + recordedCallback +
	`/callback/github", "https://Workspace-Manager.127.0.0.1.nip.io:8443/callback/github", "https://` + recordedCallback + `/callback/other"]}`

// runApp runs `beekeeper app <args>` on a and returns what it printed.
func runApp(t *testing.T, a *app, args ...string) (string, error) {
	t.Helper()
	out := a.out.(interface{ String() string })
	defer func() { a.out.(interface{ Reset() }).Reset() }()
	c := a.appCmd()
	c.SetArgs(args)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.Execute()
	return out.String(), err
}

// An App is recorded with its name, client id and the person's words, by
// the caller, once the organisation declares it: the callback hosts and the
// declaration are its manifest's; an App no manifest declares under its
// name, and every App without a declaring repository, is refused; a record
// of the same name is replaced;
// the list and the log name it; a revoke takes it off; check prints the
// hook's decision for a page, dry: the allow with the record on the
// recorded consent URL, the refusal on another callback host, no decision
// on a page that is no consent page.
func TestAppRecord(t *testing.T) {
	a, _ := stubApp(t)
	if _, err := runApp(t, a, allowArgs(personsWord)...); err == nil || !strings.Contains(err.Error(), "no repository declares the organisation's Apps") {
		t.Errorf("app allow without a declaring repository = %v", err)
	}
	declareApps(t, a, map[string]string{
		"apps/" + recordedApp + "/manifest.json": recordedManifest,
		"apps/renamed/manifest.json":             `{"name": "another-app", "callback_urls": ["https://example.org/cb"]}`,
		"apps/no-callback/manifest.json":         `{"name": "no-callback", "callback_urls": []}`,
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{appAllow, recordedApp}, wordFlag},
		{[]string{appAllow, recordedApp, clientIDFlag, recordedClientID}, wordFlag},
		{[]string{appAllow, recordedApp, wordFlag, "w"}, clientIDFlag + " <id> for a consent (the OAuth client id the App's consent URL carries), --permission \"<scope>: <level>\" for one permission change, --delete for the App's deletion, --google --callback <host> for a Google OAuth client's consent, or several"},
		{[]string{appAllow, recordedApp, permissionFlag, "Members", wordFlag, "w"}, `--permission names one change as "<scope>: <level>" (e.g. "Members: Read-only"), not "Members"`},
		{[]string{appAllow, "undeclared", clientIDFlag, recordedClientID, wordFlag, "w"}, "App undeclared is not declared: " + declaringRepo + " declares no App at apps/undeclared/manifest.json"},
		{[]string{appAllow, "renamed", clientIDFlag, recordedClientID, wordFlag, "w"}, "App renamed is not declared: " + declaringRepo + "@0123456789ab:apps/renamed/manifest.json names the App another-app"},
		{[]string{appAllow, "no-callback", clientIDFlag, recordedClientID, wordFlag, "w"}, "App no-callback has no consent page to record: " + declaringRepo + "@0123456789ab:apps/no-callback/manifest.json declares no callback URL (an App used through the device flow has none); a change of its permissions is recorded with --permission \"<scope>: <level>\""},
		{[]string{"revoke", recordedApp}, "no App " + recordedApp + " is on record"},
	} {
		if _, err := runApp(t, a, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("app %q = %v, want a refusal with %q", tc.args, err, tc.want)
		}
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, "no App on record") {
		t.Errorf("list without a record printed %q", out)
	}
	callbacks := recordedCallback + " " + recordedCallback + ":8443"
	out, err := runApp(t, a, allowArgs("and first")...)
	if err != nil || out != "App "+recordedApp+" on record, declared in "+recordedDeclared+": its consent page with callback "+
		recordedCallback+" or "+recordedCallback+":8443 is answered by the hook\n" {
		t.Fatalf("app allow = %q, %v", out, err)
	}
	if out, _ := runApp(t, a, allowArgs(personsWord)...); !strings.Contains(out, "on record") {
		t.Fatalf("app allow again = %q", out)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Apps) != 1 || strings.Join(st.Apps[0].Callbacks, " ") != callbacks || st.Apps[0].Declared != recordedDeclared ||
		st.Apps[0].By.Name != agentOne || st.Apps[0].Word != personsWord {
		t.Fatalf("the record = %+v, %v", st.Apps, err)
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, recordedApp+"  client id "+recordedClientID+"  callbacks "+callbacks+"  declared in "+recordedDeclared) ||
		!strings.Contains(out, "by "+agentOne+": "+personsWord) {
		t.Errorf("list = %q", out)
	}
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == appAllowed })
	if len(evs) != 2 || !strings.Contains(evs[1].Detail, recordedApp+": client id "+recordedClientID+", callbacks "+callbacks+", declared in "+recordedDeclared+": "+personsWord) {
		t.Errorf("app.allow events = %+v", evs)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the recorded consent URL": {[]string{appCheck, recordedConsent, titleFlag, recordedTitle}, "allow: beekeeper allows the click: the consent page of the person's own App " + recordedApp + " (callback " + recordedCallback + "), declared in " + recordedDeclared + ", on record since "},
		"another callback host":    {[]string{appCheck, strings.ReplaceAll(recordedConsent, recordedCallback, "example.org"), titleFlag, recordedTitle}, "refuse: Refused: GitHub's consent page \"" + recordedTitle + "\" (callback example.org) names an App beekeeper has no record of"},
		"another App":              {[]string{appCheck, recordedConsent, titleFlag, "Authorize other"}, checkRefuse},
		"no consent page":          {[]string{appCheck, "https://github.com/settings/applications"}, "no decision: not a page the hook decides"},
		"the lab's Dex":            {[]string{appCheck, "https://dex.127.0.0.1.nip.io/dex/auth/local?req=x"}, "allow: beekeeper allows it: dex.127.0.0.1.nip.io is the lab's own identity provider"},
	} {
		if out, err := runApp(t, a, tc.args...); err != nil || !strings.HasPrefix(out, tc.want) {
			t.Errorf("%s: app check = %q, %v; want %q", name, out, err, tc.want)
		}
	}
	if out, err := runApp(t, a, "revoke", recordedApp); err != nil || !strings.Contains(out, "off the record") {
		t.Fatalf("app revoke = %q, %v", out, err)
	}
	if out, _ := runApp(t, a, appCheck, recordedConsent, titleFlag, recordedTitle); !strings.HasPrefix(out, checkRefuse) {
		t.Errorf("check after the revoke = %q", out)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == appRevoked }); len(evs) != 1 || !strings.Contains(evs[0].Detail, recordedApp) {
		t.Errorf("app.revoke events = %+v", evs)
	}
	// The hook reads the record as the guard does, from the configuration's
	// state directory.
	if _, err := runApp(t, a, allowArgs("w")...); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", filepath.Join(a.cfg.StateDir, "config.yaml"))
	if apps := a.allowedApps(); len(apps) != 1 || apps[0].Name != recordedApp || apps[0].ClientID != recordedClientID || apps[0].Callbacks[0] != recordedCallback || apps[0].Declared != recordedDeclared || apps[0].By != agentOne || apps[0].Org != fixtureOrg {
		t.Errorf("allowedApps = %+v", apps)
	}
}

// A Google OAuth client is recorded with --google and its callback hosts,
// on the person's word alone: no declaring repository is read, a URL's host
// is taken, the list and the answer name it; check allows Google's consent
// of its flow and refuses a third party's; a GitHub App's flags and a
// --callback without --google are refused.
func TestAppGoogleRecord(t *testing.T) {
	a, _ := stubApp(t)
	const (
		client   = "example-portal"
		callback = "portal.example.org"
		chooser  = "https://accounts.google.com/v3/signin/accountchooser?client_id=REDACTED&redirect_uri=https%3A%2F%2F" + callback + "%2Fcb&response_type=code"
		account  = "person@example.com"
		scopes   = "https://accounts.google.com/o/oauth2/v2/auth?client_id=REDACTED&redirect_uri=https%3A%2F%2F" + callback + "%2Fcb&response_type=code"
	)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{appAllow, client, googleFlag, wordFlag, "w"}, "--google needs --callback <host>"},
		{[]string{appAllow, client, googleFlag, callbackFlag, callback, wordFlag, "w"}, "--google needs --account <address>"},
		{[]string{appAllow, client, clientIDFlag, "x", accountFlag, account, wordFlag, "w"}, "--account names the account a Google OAuth client's sign-in chooses, with --google"},
		{[]string{appAllow, client, googleFlag, callbackFlag, callback, permissionFlag, deviceChange, wordFlag, "w"}, "--google records a consent on Google's sign-in alone"},
		{[]string{appAllow, client, googleFlag, callbackFlag, "a b", wordFlag, "w"}, `--callback names a callback host (or a URL), not "a b"`},
		{[]string{appAllow, client, callbackFlag, callback, clientIDFlag, "x", wordFlag, "w"}, "--callback names a Google OAuth client's callback hosts, with --google"},
	} {
		if _, err := runApp(t, a, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("app %q = %v, want a refusal with %q", tc.args, err, tc.want)
		}
	}
	out, err := runApp(t, a, appAllow, client, googleFlag, callbackFlag, "https://Portal.example.org/api/auth/google/handler/frame", callbackFlag, callback, accountFlag, " Person@Example.com ", wordFlag, personsWord)
	if err != nil || out != "App "+client+" on record, declared in "+googleDeclared+": its consent on Google's sign-in with callback "+callback+" as "+account+" is answered by the hook\n" {
		t.Fatalf("app allow --google = %q, %v", out, err)
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, client+"  Google  callbacks "+callback+"  account "+account+"  declared in "+googleDeclared+"  since ") {
		t.Errorf("list = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, scopes); !strings.HasPrefix(out, checkAllow+"beekeeper allows it: Google's sign-in to the person's own App "+client) {
		t.Errorf("check of its flow = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, chooser, wordsFlag, "Clicks the account "+account); !strings.HasPrefix(out, checkAllow) {
		t.Errorf("check of its account chooser = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, chooser, wordsFlag, "Clicks the account other@example.com"); !strings.HasPrefix(out, checkRefuse) {
		t.Errorf("check of another account on its chooser = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, strings.ReplaceAll(scopes, callback, "app.example.net")); !strings.HasPrefix(out, checkRefuse) || !strings.Contains(out, guard.GoogleForm) {
		t.Errorf("check of a third party's consent = %q", out)
	}
	t.Setenv("BEEKEEPER_CONFIG", filepath.Join(a.cfg.StateDir, "config.yaml"))
	if apps := a.allowedApps(); len(apps) != 1 || !apps[0].Google || apps[0].Callbacks[0] != callback || apps[0].Account != account {
		t.Errorf("allowedApps = %+v", apps)
	}
}

// One permission change of an App is recorded with --permission, for an
// App without callback URLs too, no client id needed: the record carries
// the organisation, the change and the declaration, the list, the log and
// the answer name it, the hook decides the App's permissions page from it
// (the navigation, a call in the change's words, Save changes; another
// App's page and another call refused), and a record of both a consent and
// a change answers both.
func TestAppPermissionRecord(t *testing.T) {
	a, _ := stubApp(t)
	declareApps(t, a, map[string]string{"apps/" + deviceApp + "/manifest.json": deviceManifest, "apps/" + recordedApp + "/manifest.json": recordedManifest})
	out, err := runApp(t, a, appAllow, deviceApp, permissionFlag, " Members:Read-only ", wordFlag, personsWord)
	if err != nil || out != "App "+deviceApp+" on record, declared in "+deviceDeclared+": the change "+deviceChange+" on its permissions page and its installation's "+
		"Review request and Accept new permissions is answered by the hook\n" {
		t.Fatalf("app allow --permission = %q, %v", out, err)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Apps) != 1 || st.Apps[0].Permission != deviceChange || st.Apps[0].Org != fixtureOrg || st.Apps[0].ClientID != "" || len(st.Apps[0].Callbacks) != 0 ||
		st.Apps[0].Declared != deviceDeclared || st.Apps[0].Word != personsWord {
		t.Fatalf("the record = %+v, %v", st.Apps, err)
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, deviceApp+"  permission "+deviceChange+"  declared in "+deviceDeclared+"  since ") {
		t.Errorf("list = %q", out)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == appAllowed }); len(evs) != 1 || !strings.Contains(evs[0].Detail, deviceApp+": permission "+deviceChange+", declared in "+deviceDeclared+": "+personsWord) {
		t.Errorf("app.allow events = %+v", evs)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the navigation to its permissions page": {[]string{appCheck, permissionsPage, navigateFlag}, "allow: beekeeper allows it: the permissions page of the person's own App " + deviceApp + ", declared in " + deviceDeclared + ", on record for the change " + deviceChange + " since "},
		"the click that selects the level":       {[]string{appCheck, permissionsPage, titleFlag, permissionsTitle, wordsFlag, selectsReadOnly}, "allow: beekeeper allows the call: the permissions page of the person's own App " + deviceApp},
		"Save changes":                           {[]string{appCheck, permissionsPage, wordsFlag, "Clicks Save changes"}, checkAllow},
		"another level":                          {[]string{appCheck, permissionsPage, wordsFlag, "Selects Read and write for Members access"}, "refuse: Refused: on the permissions page of the person's own App " + deviceApp + " the call \"Selects Read and write for Members access\" is neither the one permission change on record (" + deviceChange + ") nor Save changes"},
		"another App's page":                     {[]string{appCheck, strings.Replace(permissionsPage, deviceApp, "other-app", 1), navigateFlag}, "refuse: Refused: the permissions page of the App other-app of example is not on record for a permission change"},
		"the consent page, no consent on record": {[]string{appCheck, recordedConsent, titleFlag, recordedTitle}, checkRefuse},
		"another navigation":                     {[]string{appCheck, "https://github.com/organizations/example/settings/installations", navigateFlag}, checkNone},
	} {
		if out, err := runApp(t, a, tc.args...); err != nil || !strings.HasPrefix(out, tc.want) {
			t.Errorf("%s: app check = %q, %v; want %q", name, out, err, tc.want)
		}
	}
	out, err = runApp(t, a, appAllow, recordedApp, clientIDFlag, recordedClientID, permissionFlag, "Administration: Read and write", wordFlag, personsWord)
	if err != nil || !strings.HasSuffix(out, ": its consent page with callback "+recordedCallback+" or "+recordedCallback+":8443 and the change Administration: Read and write on its "+
		"permissions page and its installation's Review request and Accept new permissions are answered by the hook\n") {
		t.Fatalf("app allow with both = %q, %v", out, err)
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, recordedApp+"  client id "+recordedClientID+"  callbacks "+recordedCallback+" "+recordedCallback+":8443  permission Administration: Read and write  declared in ") {
		t.Errorf("list with both = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, recordedConsent, titleFlag, recordedTitle); !strings.HasPrefix(out, checkAllow) {
		t.Errorf("check of the consent with both = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, strings.Replace(permissionsPage, deviceApp, recordedApp, 1), navigateFlag); !strings.HasPrefix(out, checkAllow) {
		t.Errorf("check of the permissions page with both = %q", out)
	}
	if out, err := runApp(t, a, "revoke", deviceApp); err != nil || !strings.Contains(out, "off the record") {
		t.Fatalf("app revoke = %q, %v", out, err)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == appRevoked }); len(evs) != 1 || !strings.Contains(evs[0].Detail, deviceApp+": permission "+deviceChange+", declared in") {
		t.Errorf("app.revoke events = %+v", evs)
	}
	if out, _ := runApp(t, a, appCheck, permissionsPage, navigateFlag); !strings.HasPrefix(out, checkRefuse) {
		t.Errorf("check after the revoke = %q", out)
	}
}

// An App's life on GitHub, as app check prints the hook's decisions: the
// manifest page's Create GitHub App is allowed while an app create runs
// for the App (its listener answers) and refused while none does; the
// installation page's Install is allowed for the App being created or in
// beekeeper's ledger and refused for any other; the Advanced page's
// deletion is allowed for an App on record with --delete (declared in
// apps.repo or by a local manifest, under the organisation or the person's
// own account) and for a ledger App whose credentials were stored nowhere,
// refused for a ledger App that holds them and for any other.
func TestAppLifecycleCheck(t *testing.T) {
	const (
		creating     = "example-lab-agents"
		lostApp      = "example-lab-lost"
		keptApp      = "example-lab-kept"
		listener     = "127.0.0.1:43123"
		manifestPage = "https://github.com/settings/apps/manifest"
		createWords  = "Clicks Create GitHub App for " + fixtureOrg
		deleteFlag   = "--delete"
	)
	a, _ := stubApp(t)
	declareApps(t, a, map[string]string{"apps/" + recordedApp + "/manifest.json": recordedManifest})
	up := true
	orig := listenerUp
	t.Cleanup(func() { listenerUp = orig })
	listenerUp = func(r state.AppRun) bool { return up && r.Listener == listener }
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AppRuns = []state.AppRun{{Name: creating, Org: fixtureOrg, Declared: "/tmp/glean/manifest.json", Listener: listener, PID: 1, By: state.Party{Name: agentOne}, At: a.now}}
		st.AppsCreated = []state.AppCreated{
			{Name: lostApp, Slug: lostApp, ID: 11, Org: fixtureOrg, Declared: recordedDeclared, At: a.now},
			{Name: keptApp, Slug: keptApp, ID: 12, Org: fixtureOrg, Declared: recordedDeclared, Stored: "op://vault/" + keptApp, At: a.now},
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(local, []byte(`{"name": "example-lab-half"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{appAllow, recordedApp, deleteFlag}, wordFlag},
		{[]string{appAllow, recordedApp, manifestFlag, local, wordFlag, "w"}, "--manifest declares an App for --delete alone"},
		{[]string{appAllow, "another", deleteFlag, manifestFlag, local, wordFlag, "w"}, "App another is not declared: " + local + " names the App example-lab-half"},
	} {
		if _, err := runApp(t, a, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("app %q = %v, want a refusal with %q", tc.args, err, tc.want)
		}
	}
	out, err := runApp(t, a, appAllow, recordedApp, deleteFlag, wordFlag, personsWord)
	if err != nil || out != "App "+recordedApp+" on record, declared in "+recordedDeclared+": its deletion on its Advanced page under the organisation's or the person's own account is answered by the hook\n" {
		t.Fatalf("app allow --delete = %q, %v", out, err)
	}
	if _, err := runApp(t, a, appAllow, "example-lab-half", deleteFlag, manifestFlag, local, wordFlag, personsWord); err != nil {
		t.Fatalf("app allow --delete --manifest: %v", err)
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, recordedApp+"  delete  declared in "+recordedDeclared) || !strings.Contains(out, "example-lab-half  delete  declared in "+local) ||
		!strings.Contains(out, "created "+lostApp+"  slug "+lostApp+"  id 11  of "+fixtureOrg+"  credentials stored nowhere") ||
		!strings.Contains(out, "created "+keptApp+"  slug "+keptApp+"  id 12  of "+fixtureOrg+"  credentials in op://vault/"+keptApp) {
		t.Errorf("list = %q", out)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"Create on the manifest page": {[]string{appCheck, manifestPage, wordsFlag, createWords}, "allow: beekeeper allows the click: GitHub's manifest page of the person's own App " +
			creating + " of " + fixtureOrg + ", declared in /tmp/glean/manifest.json, which the `beekeeper app create` run by " + agentOne + " since "},
		"Create for another organisation": {[]string{appCheck, manifestPage, wordsFlag, "Clicks Create GitHub App for other-org"},
			"refuse: Refused: GitHub's manifest page creates an App, and the page names none of the Apps the running `beekeeper app create` commands create."},
		"Create on the personal account's page": {[]string{appCheck, "https://github.com/settings/apps/new", wordsFlag, createWords}, "allow: beekeeper allows the click: GitHub's manifest page of the person's own App " + creating},
		"the organisation's form submit":        {[]string{appCheck, "https://github.com/organizations/" + fixtureOrg + "/settings/apps/new", wordsFlag, "Submits the form"}, checkAllow},
		"another call on the manifest page":     {[]string{appCheck, manifestPage, wordsFlag, "Scrolls down"}, checkNone},
		"Install of the App being created":      {[]string{appCheck, "https://github.com/apps/" + creating + "/installations/new", wordsFlag, clickInstall}, "allow: beekeeper allows the click: the installation page of the person's own App " + creating},
		"Install of a ledger App": {[]string{appCheck, "https://github.com/apps/" + keptApp + "/installations/new/permissions?suggested_target_id=1", wordsFlag, "Clicks Install & Authorize"},
			"allow: beekeeper allows the click: the installation page of the person's own App " + keptApp + " of " + fixtureOrg + " (id 12), declared in " + recordedDeclared +
				", which `beekeeper app create` created on "},
		"Install of another App": {[]string{appCheck, "https://github.com/apps/other-app/installations/new", wordsFlag, clickInstall},
			"refuse: Refused: the installation page of the App other-app, which beekeeper neither creates nor created."},
		"Delete of an App on record": {[]string{appCheck, "https://github.com/organizations/" + fixtureOrg + "/settings/apps/" + recordedApp + "/advanced", wordsFlag, clickDelete},
			"allow: beekeeper allows the call: the deletion of the person's own App " + recordedApp + ", declared in " + recordedDeclared + ", on record for its deletion since "},
		"its name typed into the confirmation": {[]string{appCheck, "https://github.com/organizations/" + fixtureOrg + "/settings/apps/" + recordedApp + "/advanced", wordsFlag, "Types the App's name " + recordedApp}, checkAllow},
		"Delete under the person's account": {[]string{appCheck, halfAdvanced, wordsFlag, "Clicks I understand, delete this GitHub App"},
			"allow: beekeeper allows the call: the deletion of the person's own App example-lab-half, declared in " + local},
		"the navigation to its Advanced page": {[]string{appCheck, halfAdvanced, navigateFlag}, "allow: beekeeper allows it: the Advanced page of the person's own App example-lab-half"},
		"Delete of a ledger App stored nowhere": {[]string{appCheck, "https://github.com/organizations/" + fixtureOrg + "/settings/apps/" + lostApp + "/advanced", wordsFlag, clickDelete},
			"allow: beekeeper allows the call: the deletion of the person's own App " + lostApp + " of " + fixtureOrg + " (id 11), declared in " + recordedDeclared + ", which `beekeeper app create` created on "},
		"Delete of a ledger App that holds credentials": {[]string{appCheck, "https://github.com/organizations/" + fixtureOrg + "/settings/apps/" + keptApp + "/advanced", wordsFlag, clickDelete},
			"refuse: Refused: the deletion of the App " + keptApp + " is on no record: a deletion is answered only for the person's own App that they asked to delete, which " + guard.DeleteForm},
		"Delete of another App":            {[]string{appCheck, "https://github.com/settings/apps/other-app/advanced", wordsFlag, clickDelete}, "refuse: Refused: the deletion of the App other-app is on no record"},
		"a transfer on an App on record":   {[]string{appCheck, halfAdvanced, wordsFlag, "Clicks Transfer ownership"}, checkNone},
		"the Advanced page of another App": {[]string{appCheck, "https://github.com/settings/apps/other-app/advanced", navigateFlag}, checkNone},
	} {
		if out, err := runApp(t, a, tc.args...); err != nil || !strings.HasPrefix(out, tc.want) {
			t.Errorf("%s: app check = %q, %v; want %q", name, out, err, tc.want)
		}
	}
	up = false
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"Create without a run": {[]string{appCheck, manifestPage, wordsFlag, createWords}, "refuse: Refused: GitHub's manifest page creates an App, and no `beekeeper app create` runs. " +
			"A creation is a grant, answered only for the person's own App while " + guard.CreateForm},
		"Install without a run": {[]string{appCheck, "https://github.com/apps/" + creating + "/installations/new", wordsFlag, clickInstall}, checkRefuse},
	} {
		if out, err := runApp(t, a, tc.args...); err != nil || !strings.HasPrefix(out, tc.want) {
			t.Errorf("%s: app check = %q, %v; want %q", name, out, err, tc.want)
		}
	}
}
