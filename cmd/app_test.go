package cmd

import (
	"path/filepath"
	"strings"
	"testing"

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
	personsWord = "your agent has to do this"
	appAllow    = "allow"
	appCheck    = "check"
	titleFlag   = "--title"
)

// allowArgs are `app allow`'s arguments for the recorded App with callback
// and the person's words.
func allowArgs(callback, word string) []string {
	return []string{appAllow, recordedApp, "--client-id", recordedClientID, "--callback", callback, "--word", word}
}

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

// An App is recorded with its name, client id, callback host and the
// person's words, by the caller; a record of the same name is replaced;
// the list and the log name it; a revoke takes it off; check prints the
// hook's decision for a page, dry: the allow with the record on the
// recorded consent URL, the refusal on another callback host, no decision
// on a page that is no consent page.
func TestAppRecord(t *testing.T) {
	a, _ := stubApp(t)
	for _, args := range [][]string{
		{appAllow, recordedApp},
		{appAllow, recordedApp, "--client-id", recordedClientID},
		allowArgs("https://"+recordedCallback+"/callback", personsWord),
		allowArgs(recordedCallback, ""),
		{"revoke", recordedApp},
	} {
		if _, err := runApp(t, a, args...); err == nil {
			t.Errorf("app %q passed, want a refusal", args)
		}
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, "no App on record") {
		t.Errorf("list without a record printed %q", out)
	}
	out, err := runApp(t, a, allowArgs(recordedCallback, personsWord)...)
	if err != nil || !strings.Contains(out, "App "+recordedApp+" on record") {
		t.Fatalf("app allow = %q, %v", out, err)
	}
	if out, _ := runApp(t, a, allowArgs("Workspace-Manager.127.0.0.1.nip.io:8443", "and again")...); !strings.Contains(out, "callback workspace-manager.127.0.0.1.nip.io:8443") {
		t.Errorf("app allow again = %q", out)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Apps) != 1 || st.Apps[0].Callback != "workspace-manager.127.0.0.1.nip.io:8443" || st.Apps[0].By.Name != agentOne || st.Apps[0].Word != "and again" {
		t.Fatalf("the record = %+v, %v", st.Apps, err)
	}
	if out, _ := runApp(t, a, allowArgs(recordedCallback, personsWord)...); !strings.Contains(out, "on record") {
		t.Fatalf("app allow = %q", out)
	}
	if out, _ := runApp(t, a, "list"); !strings.Contains(out, recordedApp+"  client id "+recordedClientID+"  callback "+recordedCallback) || !strings.Contains(out, "by "+agentOne+": "+personsWord) {
		t.Errorf("list = %q", out)
	}
	evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == appAllowed })
	if len(evs) != 3 || !strings.Contains(evs[2].Detail, recordedApp+": client id "+recordedClientID+", callback "+recordedCallback) {
		t.Errorf("app.allow events = %+v", evs)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the recorded consent URL": {[]string{appCheck, recordedConsent, titleFlag, recordedTitle}, "allow: beekeeper allows the click: the consent page of the person's own App " + recordedApp + " (callback " + recordedCallback + "), on record since "},
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
	if _, err := runApp(t, a, allowArgs(recordedCallback, "w")...); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", filepath.Join(a.cfg.StateDir, "config.yaml"))
	if apps := a.allowedApps(); len(apps) != 1 || apps[0].Name != recordedApp || apps[0].ClientID != recordedClientID || apps[0].Callback != recordedCallback || apps[0].By != agentOne {
		t.Errorf("allowedApps = %+v", apps)
	}
}
