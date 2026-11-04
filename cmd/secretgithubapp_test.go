//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	dryAppSkeleton = "kind: Secret\nmetadata: {name: app}\n"
	codeFlag       = "--code"
	conversionCode = "a1b2c3d4e5f6"
	// a key's lines without PEM armour: the command writes what GitHub
	// answers and parses no key
	plantedPEM    = "key line one\n" + secretValue + "-pem\nkey line three\n"
	plantedClient = secretValue + "-client"
)

// fakeConversion is GitHub's API answering POST
// /app-manifests/<code>/conversions for one code, unauthenticated as GitHub
// documents it, counting its calls; any other code, and a request that
// carries a token, is GitHub's 404. No gh call is made.
func fakeConversion(t *testing.T, webhook string) *int {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/"+conversionCode+"/conversions" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message": "Not Found", "documentation_url": "https://docs.github.com/rest/apps/apps#create-a-github-app-from-a-manifest"}`)
			return
		}
		hook := "null"
		if webhook != "" {
			hook = fmt.Sprintf("%q", webhook)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id": 123456, "slug": "example-agents", "name": "example-agents", "client_id": "Iv23liExample",
			"client_secret": %q, "pem": %q, "webhook_secret": %s, "html_url": "https://github.com/apps/example-agents"}`, plantedClient, plantedPEM, hook)
	}))
	t.Cleanup(srv.Close)
	prevAPI, prevGH := githubAPI, appsGH
	t.Cleanup(func() { githubAPI, appsGH = prevAPI, prevGH })
	githubAPI = srv.URL
	appsGH = func(_ context.Context, args ...string) ([]byte, error) {
		t.Errorf("secret github-app ran gh %s", strings.Join(args, " "))
		return nil, errors.New("no gh")
	}
	return &calls
}

// githubAppConfig names the installation example's Secret in repo.
func githubAppConfig(t *testing.T, a *app, repo string) string {
	file := filepath.Join(repo, "installations", fixtureOrg, "secrets", "github-app-secret.yaml")
	_ = os.MkdirAll(filepath.Dir(file), 0o750)
	a.cfg.Apps.Secrets = map[string]config.AppSecret{fixtureOrg: {File: file, Name: githubAppOp, Namespace: "team-agents"}}
	appGit(t, repo, "init", "-b", "main")
	appGit(t, repo, "config", "user.name", "Test")
	appGit(t, repo, "config", "user.email", "test")
	appCommit(t, repo)
	remote := filepath.Join(t.TempDir(), "origin.git")
	appGit(t, repo, "init", "--bare", remote)
	appGit(t, repo, "remote", "add", "origin", remote)
	appGit(t, repo, "push", "-u", "origin", "main")
	return file
}

func appGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", repo}, args...)...) //nolint:gosec // test-owned local repositories
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func appCommit(t *testing.T, repo string) {
	t.Helper()
	appGit(t, repo, "add", ".")
	appGit(t, repo, "commit", "--allow-empty", "-m", "fixture")
}

func TestSecretGitHubAppWritesTheSecretAndPrintsOnlyIDs(t *testing.T) {
	a, tools, repo := secretApp(t)
	file := githubAppConfig(t, a, repo)
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
	file := githubAppConfig(t, a, repo)
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
	githubAppConfig(t, a, repo)
	calls := fakeConversion(t, "")
	if _, err := runSecret(a, githubAppOp, fixtureOrg, codeFlag, conversionCode); err != nil {
		t.Fatal(err)
	}
	appCommit(t, repo)
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
		// spent or older than its hour: GitHub's status and message
		{[]string{fixtureOrg, "--replace", codeFlag, "expired0code"}, ExitRefused, 2, "the conversion: GitHub answered HTTP 404: Not Found"},
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
	githubAppConfig(t, a, repo)
	a.cfg.Apps.Secrets = map[string]config.AppSecret{"absent": {File: filepath.Join(repo, "absent-secret.yaml")}}
	if _, err := runSecret(a, githubAppOp, "absent", codeFlag, conversionCode); Code(err) != ExitRefused || !strings.Contains(err.Error(), "is absent") {
		t.Errorf("an absent file without a Secret's name = %v", err)
	}
	a.cfg.Apps.Secrets["nodir"] = config.AppSecret{File: filepath.Join(repo, "nowhere", "secret.yaml"), Name: githubAppOp, Namespace: "team-agents"}
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

// A refused conversion names GitHub's status and message, and no error
// carries the code: not GitHub's answer, not an unreachable API.
func TestConvertManifestNamesGitHubsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message": "Validation Failed"}`)
	}))
	prev := githubAPI
	t.Cleanup(func() { githubAPI = prev })
	githubAPI = srv.URL
	if _, err := convertManifest(context.Background(), conversionCode); err == nil || err.Error() != "the conversion: GitHub answered HTTP 422: Validation Failed" {
		t.Errorf("a 422 = %v", err)
	}
	srv.Close()
	_, err := convertManifest(context.Background(), conversionCode)
	if err == nil || !strings.HasPrefix(err.Error(), "the conversion: ") || strings.Contains(err.Error(), conversionCode) {
		t.Errorf("an unreachable API = %v, want the cause without the code", err)
	}
}

// The dry form must work with an inaccessible vault and never touch a
// credential tool, the conversion endpoint, the target or the audit log.
func TestSecretGitHubAppDryRun(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprint(jsonOutput), func(t *testing.T) {
			a, tools, repo := secretApp(t)
			file := githubAppConfig(t, a, repo)
			id, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			recipient := id.Recipient().String()
			rules := "creation_rules:\n  - age: " + recipient + "\n    encrypted_regex: ^(data|stringData)$\n"
			if err := os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte(rules), 0o600); err != nil {
				t.Fatal(err)
			}
			// Public metadata and opaque ciphertext: no decryptable identity exists.
			body := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: github-app\n  namespace: team-agents\nsops:\n  age:\n    - recipient: " + recipient + "\n  mac: ENC[unread]\n"
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			a.cfg.Secret.TokenFile = filepath.Join(repo, "unavailable-token")
			a.cfg.Secret.Session = true
			a.cfg.Secret.AgeIdentities = []config.AgeIdentity{{Recipient: recipient, Ref: "op://Shared/missing/identity"}}
			calls := fakeConversion(t, "")
			a.json = jsonOutput
			before, err := a.store.Events(0, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Even a supplied code is ignored and never consumed.
			out, err := runSecret(a, githubAppOp, testDryRunFlag, fixtureOrg, codeFlag, conversionCode)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{file, githubAppOp, a.cfg.Apps.Secrets[fixtureOrg].Namespace, recipient, "stringData.private-key", "stringData.client-secret", "stringData.webhook-secret"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q: %s", want, out)
				}
			}
			noSecret(t, "dry run", out)
			if strings.Contains(out, conversionCode) {
				t.Fatal("output contains manifest code")
			}
			after, err := a.store.Events(0, nil)
			if err != nil || len(before) != len(after) {
				t.Fatalf("dry run wrote events: %v", err)
			}
			raw, err := os.ReadFile(file) //nolint:gosec // the test’s scratch target
			if err != nil || string(raw) != body {
				t.Fatalf("dry run changed target: %v", err)
			}
			if len(tools.Calls) != 0 || *calls != 0 {
				t.Fatalf("dry run called tools %v or GitHub %d times", tools.Calls, *calls)
			}
			if _, err := os.Stat(filepath.Join(a.cfg.StateDir, "scan", "key")); !os.IsNotExist(err) {
				t.Fatalf("dry run created fingerprint key: %v", err)
			}
			a.out = &bytes.Buffer{}
			if _, err := runSecret(a, githubAppOp, fixtureOrg, testDryRunFlag); err != nil {
				t.Fatalf("dry run without code: %v", err)
			}
		})
	}
}

func TestSecretGitHubAppDryRunMissingParts(t *testing.T) {
	for _, tc := range []struct{ name, body, rules, want string }{
		{name: "missing-installation", want: "names no Secret"},
		{name: "file", want: "target Secret:"},
		{name: "rules", body: dryAppSkeleton, want: "no .sops.yaml"},
		{name: "unmatched", body: dryAppSkeleton, rules: "creation_rules:\n  - path_regex: nowhere\n", want: "no creation rule"},
		{name: "recipient", body: dryAppSkeleton, rules: "creation_rules:\n  - age: invalid\n", want: "no valid age recipient"},
		{name: "wrong-kind", body: "kind: ConfigMap\n", want: "no Kubernetes Secret"},
		{name: "missing-name", body: "kind: Secret\n", want: "no metadata.name"},
		{name: handoverMalformedMode, body: "[" + secretValue, want: "no YAML mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, tools, repo := secretApp(t)
			file := githubAppConfig(t, a, repo)
			if tc.name == "missing-installation" {
				a.cfg.Apps.Secrets = nil
			}
			if tc.body != "" {
				if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(filepath.Join(repo, ".sops.yaml")); err != nil {
				t.Fatal(err)
			}
			if tc.rules != "" {
				if err := os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte(tc.rules), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := fakeConversion(t, "")
			out, err := runSecret(a, githubAppOp, testDryRunFlag, fixtureOrg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			noSecret(t, "dry run refusal", fmt.Sprint(out, err))
			if len(tools.Calls) != 0 || *calls != 0 {
				t.Fatal("dry run invoked a credential tool or GitHub")
			}
			if tc.body == "" {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatal("dry run created target")
				}
			}
		})
	}
}

func TestGitHubAppDryRunVaultRouting(t *testing.T) {
	a := &app{}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{githubAppOp, testDryRunFlag, fixtureOrg}, true},
		{[]string{githubAppOp, fixtureOrg, testDryRunFlag + "=true"}, true},
		{[]string{githubAppOp, fixtureOrg, "--dry-run=false"}, false},
		{[]string{githubAppOp, fixtureOrg, testDryRunFlag, "--dry-run=false"}, false},
		{[]string{githubAppOp, codeFlag, testDryRunFlag, fixtureOrg}, false},
		{[]string{githubAppOp, "--", testDryRunFlag}, false},
	} {
		if got := a.githubAppDryRun(tc.args); got != tc.want {
			t.Errorf("%v: dry run %v, want %v", tc.args, got, tc.want)
		}
		if tc.want && (a.secretNeedsVault("", tc.args) || a.secretNeedsBroker(tc.args)) {
			t.Errorf("%v needs credentials", tc.args)
		}
	}
}

func TestSecretGitHubAppDryRunEncryptionAndReplace(t *testing.T) {
	a, tools, repo := secretApp(t)
	file := githubAppConfig(t, a, repo)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	rulesFile := filepath.Join(repo, ".sops.yaml")
	rules := "creation_rules:\n  - age: " + id.Recipient().String() + "\n    encrypted_regex: "
	body := "kind: Secret\nmetadata: {name: app}\ndata:\n  private-key: ENC[unread]\nsops:\n  mac: ENC[unread]\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rule    string
		replace bool
		want    string
	}{
		{"^data$", true, "would stay plaintext"},
		{"^(data|stringData)$", false, "holds private-key already"},
		{"^(data|stringData)$", true, ""},
	} {
		if err := os.WriteFile(rulesFile, []byte(rules+tc.rule+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{githubAppOp, fixtureOrg, testDryRunFlag}
		if tc.replace {
			args = append(args, "--replace")
		}
		a.out = &bytes.Buffer{}
		out, err := runSecret(a, args...)
		if tc.want == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("got %v, want %q", err, tc.want)
		}
		noSecret(t, "dry run", fmt.Sprint(out, err))
	}
	if len(tools.Calls) != 0 {
		t.Fatalf("dry run invoked tools: %v", tools.Calls)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the test’s scratch target
	if err != nil || string(raw) != body {
		t.Fatalf("dry run changed target: %v", err)
	}
}

func TestSecretGitHubAppDryRunCreatesNoStateDirectory(t *testing.T) {
	a, tools, repo := secretApp(t)
	file := githubAppConfig(t, a, repo)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte("creation_rules:\n  - age: "+id.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(dryAppSkeleton), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(repo, "no-state")
	cfgPath := filepath.Join(repo, "config.yaml")
	cfg := "stateDir: " + stateDir + "\napps:\n  secrets:\n    example:\n      file: " + file + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New()
	c.SetArgs([]string{"--" + configFlag, cfgPath, "secret", githubAppOp, testDryRunFlag, fixtureOrg})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("dry run created state directory: %v", err)
	}
	if len(tools.Calls) != 0 {
		t.Fatalf("dry run invoked tools: %v", tools.Calls)
	}
}

func TestSecretGitHubAppCheckoutRefusals(t *testing.T) {
	for _, kind := range []string{"behind", litDiverged, "tracked", "staged", "untracked", "fetch-failure", "missing-main", "not-a-clone"} {
		t.Run(kind, func(t *testing.T) {
			a, tools, repo := secretApp(t)
			file := githubAppConfig(t, a, repo)
			calls := fakeConversion(t, "")
			want := "local changes"
			switch kind {
			case "behind", "diverged":
				peer := filepath.Join(t.TempDir(), "peer")
				remote := appGit(t, repo, "remote", "get-url", "origin")
				appGit(t, repo, "clone", "-b", "main", remote, peer)
				appGit(t, peer, "config", "user.name", "Test")
				appGit(t, peer, "config", "user.email", "test")
				appCommit(t, peer)
				appGit(t, peer, "push", "origin", "main")
				if kind == "diverged" {
					if err := os.WriteFile(filepath.Join(repo, "local"), []byte("local"), 0o600); err != nil {
						t.Fatal(err)
					}
					appCommit(t, repo)
				}
				want = "behind or diverged"
			case "tracked", "staged":
				if err := os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
				if kind == "staged" {
					appGit(t, repo, "add", ".sops.yaml")
				}
			case "untracked":
				if err := os.WriteFile(filepath.Join(repo, "untracked"), []byte("local"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "fetch-failure":
				appGit(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
				want = "fetch failed"
			case "not-a-clone":
				if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
				want = "not in a Git clone"
			case "missing-main":
				appGit(t, repo, "push", "origin", ":main")
				want = "fetch failed"
			}
			_, err := runSecret(a, githubAppOp, fixtureOrg, codeFlag, conversionCode)
			if Code(err) != ExitRefused || !strings.Contains(fmt.Sprint(err), want) || !strings.Contains(fmt.Sprint(err), repo) || !(strings.Contains(fmt.Sprint(err), "git -C") || strings.Contains(fmt.Sprint(err), "configure apps.secrets")) { //nolint:staticcheck // reads as the condition it checks
				t.Fatalf("got %v; want refusal naming clone, remedy and %q", err, want)
			}
			if *calls != 0 || len(tools.Calls) != 0 {
				t.Fatalf("refusal accessed credentials or converted code: %d, %v", *calls, tools.Calls)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("refusal created target: %v", err)
			}
		})
	}
}

func TestGitHubAppCheckoutAllowsAhead(t *testing.T) {
	a, _, repo := secretApp(t)
	githubAppConfig(t, a, repo)
	appCommit(t, repo)
	fakeConversion(t, "")
	if _, err := runSecret(a, githubAppOp, fixtureOrg, codeFlag, conversionCode); err != nil {
		t.Fatal(err)
	}
}
