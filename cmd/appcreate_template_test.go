package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The fixture purpose whose Apps exist once per installation, its manifest
// template, and the installation it is rendered for.
const (
	portalPurpose  = "giantswarm-portal"
	portalTemplate = `{"name": "giantswarm-portal-{installation}", "url": "https://portal.{baseDomain}", "public": false, ` +
		`"callback_urls": ["https://portal.{baseDomain}/api/auth/github/handler/frame"], "hook_attributes": {"url": "https://portal.{baseDomain}/hook", "active": false}, ` +
		`"default_permissions": {"metadata": "read"}}`
	portalInstallation = "graveler"
	portalDomain       = "graveler.example.org"
	portalApp          = "giantswarm-portal-graveler"
	installationFlag   = "--installation"
	baseDomainFlag     = "--base-domain"
	replaceFlag        = "--replace"
)

// templateApp is createApp with the portal's template declared and the
// Apps GitHub knows by slug answered as the organisation's.
func templateApp(t *testing.T, known ...string) (*app, *fakeGitHub, map[string]string) {
	t.Helper()
	a, g, tools := createApp(t, map[string]string{
		"apps/" + portalPurpose + "/manifest.template.json": portalTemplate,
		"apps/fixed/manifest.template.json":                 `{"name": "fixed-name", "url": "https://{baseDomain}"}`,
		"apps/bare/manifest.template.json":                  `{"name": "bare-{installation}", "url": "https://example.org"}`,
	})
	inner := appsGH
	appsGH = func(ctx context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		for i, slug := range known {
			if call == "api apps/"+slug {
				return []byte(`{"id": ` + string(rune('1'+i)) + `, "slug": "` + slug + `", "html_url": "https://github.com/apps/` + slug +
					`", "owner": {"login": "` + fixtureOrg + `", "type": "Organization"}}`), nil
			}
		}
		return inner(ctx, args...)
	}
	return a, g, tools.Vault
}

// A purpose's template renders, with the installation's name and base
// domain in every string, into the App of the rendered name, created as a
// declared one into the vault item of that name.
func TestAppCreateRendersTheInstallationsApp(t *testing.T) {
	a, g, vault := templateApp(t)
	out, err := runApp(t, a, appCreate, portalPurpose, installationFlag, portalInstallation, baseDomainFlag, portalDomain, timeoutFlag, tenSeconds)
	if err != nil {
		t.Fatalf("app create --installation: %v\n%s", err, out)
	}
	callbacks, _ := g.manifest["callback_urls"].([]any)
	hook, _ := g.manifest["hook_attributes"].(map[string]any)
	if g.manifest["name"] != portalApp || g.manifest["url"] != "https://portal."+portalDomain || len(callbacks) != 1 ||
		callbacks[0] != "https://portal."+portalDomain+"/api/auth/github/handler/frame" || hook["url"] != "https://portal."+portalDomain+"/hook" {
		t.Errorf("GitHub received the manifest %v", g.manifest)
	}
	item := "op://Shared/" + portalApp
	for _, field := range []string{appIDField, appClientIDField, appSlugField, githubClientSecret, githubPrivateKey, appInstallationID} {
		if vault[item+"/"+field] == "" {
			t.Errorf("the vault item %s lacks %s: %q", item, field, vault)
		}
	}
	for _, want := range []string{
		"App " + portalApp + ", declared in " + declaringRepo + "@0123456789ab:apps/" + portalPurpose + "/manifest.template.json",
		"wrote " + item + ": app-id 4242, client-id " + fixtureClientID + ", slug " + portalApp + " (configuration)",
		"installed: installation-id 9001",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stay untouched") {
		t.Errorf("a first App names a predecessor:\n%s", out)
	}
	noFixtureSecret(t, "the answer", out)
	if evs := createEvents(t, a); len(evs) != 1 || !strings.Contains(evs[0].Detail, portalPurpose+": "+item+": app-id 4242") {
		t.Errorf("the log: %+v", evs)
	}
}

// --replace creates the successor of the installation's latest App, the
// next generation's name into an item of its own, leaves the old App and
// item as they are, and names the old App's deletion as the person's step.
func TestAppCreateReplaceCreatesTheSuccessor(t *testing.T) {
	a, g, vault := templateApp(t, portalApp, portalApp+"-2")
	oldItem := "op://Shared/" + portalApp + "-2/" + githubPrivateKey
	vault[oldItem] = "the old key"
	a.json = true
	progress := &bytes.Buffer{}
	c := a.appCmd()
	c.SetArgs([]string{appCreate, portalPurpose, installationFlag, portalInstallation, baseDomainFlag, portalDomain, replaceFlag, timeoutFlag, tenSeconds})
	c.SetErr(progress)
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("app create --replace: %v\n%s", err, progress)
	}
	successor := portalApp + "-3"
	if g.manifest["name"] != successor {
		t.Errorf("GitHub received the App %v, want %s", g.manifest["name"], successor)
	}
	if vault[oldItem] != "the old key" || vault["op://Shared/"+successor+"/"+appIDField] != "4242" {
		t.Errorf("the vault after the successor: %q", vault)
	}
	if want := "the App " + portalApp + "-2 (id 2) and its vault item stay untouched until its consumers have switched to op://Shared/" + successor +
		"; then deleting it is the person's step: " + githubWeb + "/organizations/" + fixtureOrg + "/settings/apps/" + portalApp + "-2/advanced"; !strings.Contains(progress.String(), want) {
		t.Errorf("the progress lacks %q:\n%s", want, progress)
	}
	out := a.out.(*bytes.Buffer).String()
	if !strings.Contains(out, `"replaces": "`+portalApp+`-2"`) || !strings.Contains(out, `"item": "op://Shared/`+successor+`"`) {
		t.Errorf("--json:\n%s", out)
	}
	if evs := createEvents(t, a); len(evs) != 1 || !strings.Contains(evs[0].Detail, "succeeding "+portalApp+"-2") {
		t.Errorf("the log: %+v", evs)
	}
}

// Refused before any click, nothing written: an undeclared purpose, a
// rendered name without the installation, a name past GitHub's length, a
// template with {baseDomain} and no --base-domain, a malformed installation
// or domain, --replace with no App to succeed, and an App GitHub knows by
// the rendered name; --replace and --base-domain go with --installation.
func TestAppCreateFromATemplateRefusesBeforeAnyClick(t *testing.T) {
	a, _, vault := templateApp(t, "bare-taken")
	opened := 0
	openBrowser = func(context.Context, string) error { opened++; return nil }
	for name, tc := range map[string]struct {
		args []string
		code int
		want string
	}{
		"undeclared purpose": {[]string{appCreate, "no-purpose", installationFlag, portalInstallation}, ExitRefused, "no template declares the Apps of no-purpose: " + declaringRepo + " declares no App at apps/no-purpose/manifest.template.json"},
		"no installation in the name": {[]string{appCreate, "fixed", installationFlag, portalInstallation, baseDomainFlag, portalDomain}, ExitRefused,
			"renders the App name fixed-name, which lacks the installation graveler"},
		"too long":         {[]string{appCreate, portalPurpose, installationFlag, "a-very-long-installation", baseDomainFlag, portalDomain}, ExitRefused, "has 42 characters, GitHub takes at most 34"},
		"no base domain":   {[]string{appCreate, portalPurpose, installationFlag, portalInstallation}, ExitUsage, "--base-domain: " + declaringRepo + "@0123456789ab:apps/" + portalPurpose + "/manifest.template.json carries {baseDomain}"},
		"bad installation": {[]string{appCreate, portalPurpose, installationFlag, "Graveler", baseDomainFlag, portalDomain}, ExitUsage, "a lower-case DNS label"},
		"bad domain":       {[]string{appCreate, portalPurpose, installationFlag, portalInstallation, baseDomainFlag, "localhost"}, ExitUsage, "a lower-case DNS name"},
		"nothing to replace": {[]string{appCreate, portalPurpose, installationFlag, portalInstallation, baseDomainFlag, portalDomain, replaceFlag}, ExitRefused,
			"GitHub knows no App giantswarm-portal-graveler for --replace to succeed"},
		"known name":    {[]string{appCreate, "bare", installationFlag, "taken"}, ExitRefused, "GitHub already knows an App named bare-taken"},
		"replace alone": {[]string{appCreate, fixtureApp, replaceFlag}, ExitUsage, "--replace and --base-domain go with --installation"},
		"domain alone":  {[]string{appCreate, fixtureApp, baseDomainFlag, portalDomain}, ExitUsage, "--replace and --base-domain go with --installation"},
		"with manifest": {[]string{appCreate, portalInstallation, "--manifest", "m.json", installationFlag, portalInstallation}, ExitUsage, "not --manifest's file"},
	} {
		out, err := runApp(t, a, tc.args...)
		if Code(err) != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: app %s = %v (exit %d), want exit %d with %q\n%s", name, strings.Join(tc.args, " "), err, Code(err), tc.code, tc.want, out)
		}
	}
	if opened != 0 || len(vault) != 1 {
		t.Errorf("a refused creation opened %d pages and wrote the vault: %q", opened, vault)
	}
}
