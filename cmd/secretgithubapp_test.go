//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	codeFlag       = "--code"
	conversionCode = "a1b2c3d4e5f6"
	plantedPEM     = "-----BEGIN RSA PRIVATE KEY-----\n" + secretValue + "-pem\n-----END RSA PRIVATE KEY-----\n"
	plantedClient  = secretValue + "-client"
)

// fakeConversion is gh api's POST /app-manifests/<code>/conversions for one
// code, counting its calls; any other code is GitHub's 404.
func fakeConversion(t *testing.T, webhook string) *int {
	t.Helper()
	calls := 0
	prev := appsGH
	t.Cleanup(func() { appsGH = prev })
	appsGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if strings.Join(args, " ") != "api -X POST app-manifests/"+conversionCode+"/conversions" {
			return nil, errors.New("gh api: exit status 1: gh: Not Found (HTTP 404)")
		}
		hook := "null"
		if webhook != "" {
			hook = fmt.Sprintf("%q", webhook)
		}
		return fmt.Appendf(nil, `{"id": 123456, "slug": "example-agents", "name": "example-agents", "client_id": "Iv23liExample",
			"client_secret": %q, "pem": %q, "webhook_secret": %s, "html_url": "https://github.com/apps/example-agents"}`, plantedClient, plantedPEM, hook), nil
	}
	return &calls
}

// githubAppConfig names the installation example's Secret in repo.
func githubAppConfig(a *app, repo string) string {
	file := filepath.Join(repo, "installations", fixtureOrg, "secrets", "github-app-secret.yaml")
	_ = os.MkdirAll(filepath.Dir(file), 0o750)
	a.cfg.Apps.Secrets = map[string]config.AppSecret{fixtureOrg: {File: file, Name: "github-app", Namespace: "team-agents"}}
	return file
}

func TestSecretGitHubAppWritesTheSecretAndPrintsOnlyIDs(t *testing.T) {
	a, tools, repo := secretApp(t)
	file := githubAppConfig(a, repo)
	calls := fakeConversion(t, "")
	var stderr bytes.Buffer
	c := a.secretCmd()
	c.SetArgs([]string{githubAppOp, fixtureOrg, codeFlag, "https://github.com/example/redirect?code=" + conversionCode + "&state=x"})
	c.SetOut(a.out)
	c.SetErr(&stderr)
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := a.out.(*bytes.Buffer).String(); out != "app-id 123456\nclient-id Iv23liExample\n" {
		t.Errorf("stdout = %q, want the App's id and client id alone", out)
	}
	for _, key := range []string{"stringData.private-key", "stringData.client-secret"} {
		if !strings.Contains(stderr.String(), "wrote "+file+"#"+key+": ") {
			t.Errorf("stderr names no %s: %q", key, stderr.String())
		}
	}
	noSecret(t, "stderr", stderr.String())
	if *calls != 1 {
		t.Errorf("%d conversions, want 1", *calls)
	}
	plain, err := tools.Run(context.Background(), "", nil, nil, "sops", "decrypt", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Secret", "name: github-app", "namespace: team-agents", "private-key: |", secretValue + "-pem", "client-secret: " + plantedClient} {
		if !strings.Contains(string(plain), want) {
			t.Errorf("the Secret lacks %q:\n%s", want, plain)
		}
	}
	if strings.Contains(string(plain), "webhook-secret") {
		t.Errorf("an App without a webhook got a webhook-secret:\n%s", plain)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret."+githubAppOp })
	if err != nil || len(evs) != 1 {
		t.Fatalf("%d events, %v", len(evs), err)
	}
	noSecret(t, "the log", evs[0].Detail)
	for _, want := range []string{"example (" + file + ")", "app example-agents, app-id 123456, client-id Iv23liExample", "#stringData.private-key ", "hmac:"} {
		if !strings.Contains(evs[0].Detail, want) {
			t.Errorf("the log lacks %q: %s", want, evs[0].Detail)
		}
	}
	if strings.Contains(evs[0].Detail, conversionCode) {
		t.Errorf("the log carries the code: %s", evs[0].Detail)
	}
}

func TestSecretGitHubAppWebhookSecretAndJSON(t *testing.T) {
	a, tools, repo := secretApp(t)
	file := githubAppConfig(a, repo)
	fakeConversion(t, secretValue+"-hook")
	a.json = true
	out, err := runSecret(a, githubAppOp, fixtureOrg, codeFlag, conversionCode)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"appID": 123456`) || !strings.Contains(out, `"clientID": "Iv23liExample"`) || !strings.Contains(out, "#stringData.webhook-secret") {
		t.Errorf("--json = %s", out)
	}
	noSecret(t, "--json", out)
	plain, err := tools.Run(context.Background(), "", nil, nil, "sops", "decrypt", file)
	if err != nil || !strings.Contains(string(plain), "webhook-secret: "+secretValue+"-hook") {
		t.Errorf("the Secret lacks the webhook secret: %v\n%s", err, plain)
	}
}

func TestSecretGitHubAppRefusesBeforeSpendingTheCode(t *testing.T) {
	a, _, repo := secretApp(t)
	githubAppConfig(a, repo)
	calls := fakeConversion(t, "")
	if _, err := runSecret(a, githubAppOp, fixtureOrg, codeFlag, conversionCode); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args  []string
		code  int
		calls int
		want  string
	}{
		// the Secret holds the keys now: a second App is refused unspent
		{[]string{fixtureOrg, codeFlag, conversionCode}, ExitRefused, 1, "holds private-key already"},
		{[]string{"elsewhere", codeFlag, conversionCode}, ExitUsage, 1, "names no Secret for \"elsewhere\""},
		{[]string{fixtureOrg, codeFlag, "../../user"}, ExitUsage, 1, "no manifest code"},
		{[]string{fixtureOrg, codeFlag, ""}, ExitUsage, 1, "no manifest code"},
		// spent or older than its hour: GitHub's 404, said plainly
		{[]string{fixtureOrg, "--replace", codeFlag, "expired0code"}, ExitRefused, 2, "converts once, within an hour"},
	} {
		a.out = &bytes.Buffer{}
		out, err := runSecret(a, append([]string{githubAppOp}, c.args...)...)
		if Code(err) != c.code || !strings.Contains(fmt.Sprint(err), c.want) {
			t.Errorf("github-app %s = %v, want exit %d with %q", strings.Join(c.args, " "), err, c.code, c.want)
		}
		if *calls != c.calls {
			t.Errorf("github-app %s: %d conversions, want %d", strings.Join(c.args, " "), *calls, c.calls)
		}
		noSecret(t, "a refusal", fmt.Sprint(out, err))
	}
	// --replace writes over the keys of the App before
	a.out = &bytes.Buffer{}
	if _, err := runSecret(a, githubAppOp, fixtureOrg, "--replace", codeFlag, conversionCode); err != nil {
		t.Errorf("--replace = %v", err)
	}
}

func TestSecretGitHubAppRefusesAFileThatIsNoSecret(t *testing.T) {
	a, _, repo := secretApp(t)
	calls := fakeConversion(t, "")
	a.cfg.Apps.Secrets = map[string]config.AppSecret{"absent": {File: filepath.Join(repo, "absent-secret.yaml")}}
	if _, err := runSecret(a, githubAppOp, "absent", codeFlag, conversionCode); Code(err) != ExitRefused || !strings.Contains(err.Error(), "is absent") {
		t.Errorf("an absent file without a Secret's name = %v", err)
	}
	a.cfg.Apps.Secrets["nodir"] = config.AppSecret{File: filepath.Join(repo, "nowhere", "secret.yaml"), Name: "github-app", Namespace: "team-agents"}
	if _, err := runSecret(a, githubAppOp, "nodir", codeFlag, conversionCode); Code(err) != ExitRefused || !strings.Contains(err.Error(), "no directory") {
		t.Errorf("a file in an absent directory = %v", err)
	}
	if *calls != 0 {
		t.Errorf("%d conversions, want none", *calls)
	}
}

func TestParseManifestCode(t *testing.T) {
	for in, want := range map[string]string{
		conversionCode:                                  conversionCode,
		" " + conversionCode + "\n":                     conversionCode,
		"https://github.com/o/r?code=" + conversionCode: conversionCode,
		"https://github.com/o/r?state=s&code=a_b-c":     "a_b-c",
		"https://github.com/o/r":                        "",
		"abc/../def":                                    "",
		"abc?x=1":                                       "",
		strings.Repeat("a", 129):                        "",
	} {
		got, err := parseManifestCode(in)
		if (err != nil) != (want == "") || got != want && want != "" {
			t.Errorf("parseManifestCode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
