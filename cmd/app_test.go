package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
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
	personsWord  = "your agent has to do this"
	appAllow     = "allow"
	appCheck     = "check"
	titleFlag    = "--title"
	clientIDFlag = "--client-id"
	wordFlag     = "--word"
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
		{[]string{appAllow, recordedApp}, clientIDFlag},
		{[]string{appAllow, recordedApp, clientIDFlag, recordedClientID}, wordFlag},
		{[]string{appAllow, "undeclared", clientIDFlag, recordedClientID, wordFlag, "w"}, "App undeclared is not declared: " + declaringRepo + " declares no App at apps/undeclared/manifest.json"},
		{[]string{appAllow, "renamed", clientIDFlag, recordedClientID, wordFlag, "w"}, "App renamed is not declared: " + declaringRepo + "@0123456789ab:apps/renamed/manifest.json names the App another-app"},
		{[]string{appAllow, "no-callback", clientIDFlag, recordedClientID, wordFlag, "w"}, "declares no App name and callback URL"},
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
		"another App":              {[]string{appCheck, recordedConsent, titleFlag, "Authorize other"}, "refuse: "},
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
	if out, _ := runApp(t, a, appCheck, recordedConsent, titleFlag, recordedTitle); !strings.HasPrefix(out, "refuse: ") {
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
	if apps := a.allowedApps(); len(apps) != 1 || apps[0].Name != recordedApp || apps[0].ClientID != recordedClientID || apps[0].Callbacks[0] != recordedCallback || apps[0].Declared != recordedDeclared || apps[0].By != agentOne {
		t.Errorf("allowedApps = %+v", apps)
	}
}
