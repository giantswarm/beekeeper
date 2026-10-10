package cmd

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The fixture App the declaring repository (declaringRepo, app_test.go)
// declares, what the fake GitHub answers for it, and the command's words.
// The credentials are obvious fakes.
const (
	fixtureApp       = "example-lab-skills"
	fixtureOrg       = "example"
	fixtureOrgID     = "77"
	fixtureAppID     = int64(4242)
	fixtureAppIDText = "4242"
	fixtureClientID  = "Iv1.fedcba9876543210"
	fixtureSecret    = "planted-Client-Secret-3b9e1d"
	fixtureWebhook   = "planted-Webhook-Secret-5a7c2f"
	fixtureInstall   = int64(9001)
	fixtureCode      = "planted-code-1f2e3d"
	fixtureItem      = "op://Shared/" + fixtureApp
	fixtureManifest  = `{"name": "` + fixtureApp + `", "url": "https://example.org", "public": false, "hook_attributes": {"url": "https://example.org/hook"}, ` +
		`"default_permissions": {"contents": "read", "metadata": "read"}}`
	takenManifest = `{"name": "taken", "url": "https://example.org"}`
	renamedApp    = "renamed"
	appCreate     = "create"
	timeoutFlag   = "--timeout"
	tenSeconds    = "10s"
	selectedRepos = "selected"
	plainType     = "STRING"
	concealedType = "CONCEALED"
	opItemCall    = "op item"
)

// fixtureKey is the App's private key the fake conversion answers, made
// once for the package's tests.
var fixtureKey = sync.OnceValues(func() (*rsa.PrivateKey, string) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
})

// fakeGitHub is GitHub's web site and API as the flow sees them: the
// manifest page's redirect, the conversion, the installation page and the
// App's installations list.
type fakeGitHub struct {
	srv *httptest.Server
	mu  sync.Mutex
	// manifest and state are what the create page received.
	manifest map[string]any
	state    string
	// installQuery is the installation page's query once the browser got
	// there, which is the Install click unless noInstall.
	installQuery url.Values
	installed    bool
	noInstall    bool
	// wrongState redirects with another state than the page received.
	wrongState bool
	// tokens counts the installations listings asked with the App's token.
	tokens int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{}
	g.srv = httptest.NewServer(g)
	t.Cleanup(g.srv.Close)
	prevWeb, prevAPI := githubWeb, githubAPI
	githubWeb, githubAPI = g.srv.URL, g.srv.URL
	t.Cleanup(func() { githubWeb, githubAPI = prevWeb, prevAPI })
	return g
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, keyPEM := fixtureKey()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/organizations/"+fixtureOrg+"/settings/apps/new":
		g.state = r.URL.Query().Get("state")
		if err := r.ParseForm(); err != nil || json.Unmarshal([]byte(r.FormValue(appManifestField)), &g.manifest) != nil {
			http.Error(w, "no manifest", http.StatusBadRequest)
			return
		}
		redirect, _ := g.manifest[appRedirectField].(string)
		state := g.state
		if g.wrongState {
			state = "another-run"
		}
		http.Redirect(w, r, redirect+"?code="+fixtureCode+"&state="+url.QueryEscape(state), http.StatusFound)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app-manifests/"):
		if r.URL.Path != "/app-manifests/"+fixtureCode+"/conversions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message": "Not Found"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fixtureAppID, "slug": fixtureApp, "name": fixtureApp, "client_id": fixtureClientID,
			"client_secret": fixtureSecret, "webhook_secret": fixtureWebhook, "pem": keyPEM, "html_url": "https://github.com/apps/" + fixtureApp,
			"owner": map[string]any{"login": fixtureOrg, "id": 77}})
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		if !g.appToken(r.Header.Get("Authorization")) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message": "A JSON web token could not be decoded"}`)
			return
		}
		g.tokens++
		insts := []any{}
		if g.installed {
			insts = append(insts, map[string]any{"id": fixtureInstall, "repository_selection": selectedRepos, "account": map[string]any{"login": "Example", "id": 77}})
		}
		_ = json.NewEncoder(w).Encode(insts)
	case r.Method == http.MethodGet && r.URL.Path == "/apps/"+fixtureApp+"/installations/new/permissions":
		g.installQuery = r.URL.Query()
		g.installed = !g.noInstall
		_, _ = io.WriteString(w, "the installation page")
	default:
		http.NotFound(w, r)
	}
}

// appToken reports whether auth is a token the App signed: RS256 under the
// fixture key, issued by the App's id.
func (g *fakeGitHub) appToken(auth string) bool {
	key, _ := fixtureKey()
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	return json.Unmarshal(raw, &claims) == nil && claims.Iss == fixtureAppIDText
}

// The manifest page's form: its action and the manifest it submits.
var (
	formAction   = regexp.MustCompile(`<form method="post" action="([^"]+)">`)
	formManifest = regexp.MustCompile(`(?s)<textarea name="manifest"[^>]*>(.*?)</textarea>`)
)

// browse is the person's browser: it opens the loopback page, submits its
// form to GitHub as the page's script would, and follows every redirect to
// the end. It reports what stopped it.
func browse(page string) error {
	resp, err := http.Get(page) //nolint:gosec // this run's loopback page
	if err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	action, manifest := formAction.FindSubmatch(body), formManifest.FindSubmatch(body)
	if action == nil || manifest == nil {
		return errors.New("the page has no form")
	}
	resp, err = http.PostForm(html.UnescapeString(string(action[1])), url.Values{appManifestField: {html.UnescapeString(string(manifest[1]))}}) //nolint:gosec // the page's own action
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the flow ended with HTTP %d", resp.StatusCode)
	}
	return nil
}

// createApp is secretApp with the declaring repository (the fixture App
// and the manifests given), the fake GitHub, a browser that runs the flow
// by itself, and the installations looked for every 10ms; in the person's
// terminal, no agent marker set.
func createApp(t *testing.T, manifests map[string]string) (*app, *fakeGitHub, *secrettest.Tools) {
	t.Helper()
	a, tools, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	if manifests == nil {
		manifests = map[string]string{}
	}
	manifests["apps/"+fixtureApp+"/manifest.json"] = fixtureManifest
	declareApps(t, a, manifests)
	g := newFakeGitHub(t)
	inner := appsGH
	appsGH = func(ctx context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(call, "api orgs/"+fixtureOrg+" --jq .id"):
			return []byte(fixtureOrgID + "\n"), nil
		case strings.HasPrefix(call, "api repos/"+fixtureOrg+"/agentlab --jq .id"):
			return []byte("101\n"), nil
		case strings.HasPrefix(call, "api repos/"+fixtureOrg+"/workspace-manager --jq .id"):
			return []byte("102\n"), nil
		case call == "api apps/taken":
			return []byte(`{"id": 7, "slug": "taken", "html_url": "https://github.com/apps/taken", "owner": {"login": "someone"}}`), nil
		}
		return inner(ctx, args...)
	}
	prevOpen, prevPoll := openBrowser, appInstallPoll
	openBrowser = func(_ context.Context, page string) error {
		// the browser's own outcome is in what GitHub and the vault saw
		go func() { _ = browse(page) }()
		return nil
	}
	appInstallPoll = 10 * time.Millisecond
	t.Cleanup(func() { openBrowser, appInstallPoll = prevOpen, prevPoll })
	for _, k := range agentMarkers {
		t.Setenv(k, "")
	}
	return a, g, tools
}

// noFixtureSecret fails when a fixture credential, or a line of the key,
// is in s.
func noFixtureSecret(t *testing.T, what, s string) {
	t.Helper()
	_, keyPEM := fixtureKey()
	keyLine := strings.Split(keyPEM, "\n")[1]
	for _, v := range []string{fixtureSecret, fixtureWebhook, keyLine, base64.StdEncoding.EncodeToString([]byte(fixtureSecret))} {
		if strings.Contains(s, v) {
			t.Errorf("%s carries a credential:\n%s", what, s)
		}
	}
}

// createEvents are the app.create events logged, each without a value.
func createEvents(t *testing.T, a *app) []state.Event {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == appCreated })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		noFixtureSecret(t, "the log", e.Detail)
	}
	return evs
}

// A creation submits the declared manifest with this run's redirect URL and
// state, converts the code GitHub redirects with, writes the six fields of
// the vault item in one op call (the configuration as strings, the secrets
// concealed), sends the browser to the installation page with the
// organisation and the declared repositories preselected, records the
// installation's id once it exists, and answers and logs ids, the slug,
// lengths and fingerprints only.
func TestAppCreateRunsTheManifestFlowIntoTheVault(t *testing.T) {
	a, g, tools := createApp(t, map[string]string{"apps/" + fixtureApp + "/installation.json": `{"repositories": ["workspace-manager", "agentlab"]}`})
	out, err := runApp(t, a, appCreate, fixtureApp, timeoutFlag, tenSeconds)
	if err != nil {
		t.Fatalf("app create: %v\n%s", err, out)
	}
	_, keyPEM := fixtureKey()
	for field, want := range map[string]string{appIDField: fixtureAppIDText, appClientIDField: fixtureClientID, appSlugField: fixtureApp, githubClientSecret: fixtureSecret,
		githubPrivateKey: keyPEM, appWebhookSecret: fixtureWebhook, appInstallationID: "9001"} {
		if tools.Vault[fixtureItem+"/"+field] != want {
			t.Errorf("the vault's %s = %q", field, tools.Vault[fixtureItem+"/"+field])
		}
	}
	for field, want := range map[string]string{appIDField: plainType, appClientIDField: plainType, appSlugField: plainType, appInstallationID: plainType,
		githubClientSecret: concealedType, githubPrivateKey: concealedType, appWebhookSecret: concealedType} {
		if tools.Types[fixtureItem+"/"+field] != want {
			t.Errorf("the vault's %s is %s, want %s", field, tools.Types[fixtureItem+"/"+field], want)
		}
	}
	for _, want := range []string{
		"App " + fixtureApp + ", declared in " + declaringRepo + "@0123456789ab:apps/" + fixtureApp + "/manifest.json, to be installed on 2 repositories (agentlab, workspace-manager) of " + fixtureOrg,
		`Click "Create GitHub App" there; waiting up to 10s for GitHub's redirect.`,
		"created App " + fixtureApp + " (id 4242, client id " + fixtureClientID + "): https://github.com/apps/" + fixtureApp,
		"wrote " + fixtureItem + ": app-id 4242, client-id " + fixtureClientID + ", slug " + fixtureApp + " (configuration); client-secret " + fmt.Sprint(len(fixtureSecret)) + " bytes, hmac:",
		"; private-key " + fmt.Sprint(len(keyPEM)) + " bytes, hmac:",
		"; webhook-secret " + fmt.Sprint(len(fixtureWebhook)) + " bytes, hmac:",
		`Click "Install" there; waiting up to 10s for the installation.`,
		"installed: installation-id 9001 (repositories: selected), wrote " + fixtureItem + "/" + appInstallationID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	calls := strings.Join(tools.Calls, "\n")
	noFixtureSecret(t, "the answer", out)
	noFixtureSecret(t, "the command lines", calls)
	redirect, _ := g.manifest[appRedirectField].(string)
	if g.manifest["name"] != fixtureApp || g.manifest["url"] != "https://example.org" || !strings.HasPrefix(redirect, "http://127.0.0.1:") || !strings.HasSuffix(redirect, appCallbackPath) || g.state == "" {
		t.Errorf("GitHub received the manifest %v with state %q", g.manifest, g.state)
	}
	if q := g.installQuery; q.Get("suggested_target_id") != fixtureOrgID || strings.Join(q["repository_ids[]"], " ") != "101 102" {
		t.Errorf("the installation page's query: %v", q)
	}
	if g.tokens == 0 {
		t.Error("the installations were never asked as the App")
	}
	if writes := strings.Count(calls, opItemCall+" create") + strings.Count(calls, opItemCall+" edit"); writes != 2 {
		t.Errorf("%d op writes, want the credentials in one and the installation id in one:\n%s", writes, calls)
	}
	evs := createEvents(t, a)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, fixtureApp+": "+fixtureItem+": app-id 4242, client-id "+fixtureClientID+", slug "+fixtureApp+"; "+fixtureItem+"/app-id 4 bytes, hmac:") ||
		!strings.Contains(evs[0].Detail, "; installation-id 9001 (selected)") {
		t.Errorf("the log: %+v", evs)
	}
}

// --json answers the ids, the slug, the fields' lengths and fingerprints
// and the installation; an absent installation file preselects every
// repository.
func TestAppCreateJSONAndAllRepositories(t *testing.T) {
	a, g, _ := createApp(t, nil)
	a.json = true
	out, err := runApp(t, a, appCreate, fixtureApp, timeoutFlag, tenSeconds)
	if err != nil {
		t.Fatalf("app create --json: %v\n%s", err, out)
	}
	var res appCreation
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Item != fixtureItem || res.AppID != fixtureAppID || res.ClientID != fixtureClientID || res.Slug != fixtureApp ||
		res.InstallationID != fixtureInstall || res.RepositorySelection != selectedRepos || res.Installation != "" || len(res.Fields) != 7 || res.Fields[3].Bytes != len(fixtureSecret) {
		t.Errorf("--json = %+v, %v:\n%s", res, err, out)
	}
	noFixtureSecret(t, "the JSON answer", out)
	if q := g.installQuery; q.Get("suggested_target_id") != fixtureOrgID || len(q["repository_ids[]"]) != 0 {
		t.Errorf("the installation page's query: %v", q)
	}
}

// Refused in one line, nothing written and no page opened: an App no
// manifest declares under its name, one GitHub already knows by that name,
// an installation file of another shape, a repository the organisation
// lacks, the agent sandbox, no shared vault, and in an agent session the
// browser lease not held.
func TestAppCreateRefusesBeforeAnyClick(t *testing.T) {
	a, _, tools := createApp(t, map[string]string{
		"apps/renamed/manifest.json":     `{"name": "another-app", "url": "https://example.org"}`,
		"apps/taken/manifest.json":       takenManifest,
		"apps/shaped/manifest.json":      `{"name": "shaped", "url": "https://example.org"}`,
		"apps/shaped/installation.json":  `{"repositories": "some"}`,
		"apps/lacking/manifest.json":     `{"name": "lacking", "url": "https://example.org"}`,
		"apps/lacking/installation.json": `{"repositories": ["gone"]}`,
	})
	opened := 0
	openBrowser = func(context.Context, string) error { opened++; return nil }
	for name, tc := range map[string]struct {
		args []string
		env  string
		code int
		want string
	}{
		"undeclared": {[]string{appCreate, "nobody"}, "", ExitRefused, "App nobody is not declared: " + declaringRepo + " declares no App at apps/nobody/manifest.json"},
		renamedApp:   {[]string{appCreate, renamedApp}, "", ExitRefused, "App renamed is not declared: " + declaringRepo + "@0123456789ab:apps/renamed/manifest.json names the App another-app"},
		"known":      {[]string{appCreate, "taken"}, "", ExitRefused, "GitHub already knows an App named taken (taken, id 7, owned by someone): https://github.com/apps/taken; nothing written"},
		"shaped":     {[]string{appCreate, "shaped"}, "", ExitRefused, `"repositories" is "all" or a list of names, not "some"`},
		"lacking":    {[]string{appCreate, "lacking"}, "", ExitRefused, "names the repository gone, which " + fixtureOrg + " lacks; nothing written"},
		"sandbox":    {[]string{appCreate, fixtureApp}, sandbox.Env, ExitRefused, "agent sandbox"},
		"no lease":   {[]string{appCreate, fixtureApp}, agentMarkers[0], ExitRefused, "the lease browser is held by nobody, not by you: claim it first (beekeeper lease claim browser)"},
		"no name":    {[]string{appCreate, " "}, "", ExitUsage, "by its name"},
	} {
		a.out = &bytes.Buffer{}
		if tc.env != "" {
			t.Setenv(tc.env, "1")
		}
		out, err := runApp(t, a, tc.args...)
		if tc.env != "" {
			t.Setenv(tc.env, "")
		}
		if Code(err) != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: app %s = %v (exit %d), want exit %d with %q\n%s", name, strings.Join(tc.args, " "), err, Code(err), tc.code, tc.want, out)
		}
	}
	a.cfg.Secret.Vault = ""
	if _, err := runApp(t, a, appCreate, fixtureApp); Code(err) != ExitVault {
		t.Errorf("without a shared vault: %v", err)
	}
	if calls := strings.Join(tools.Calls, "\n"); opened != 0 || strings.Contains(calls, opItemCall) {
		t.Errorf("a refused creation opened %d pages and ran op:\n%s", opened, calls)
	}
	if len(tools.Vault) != 1 {
		t.Errorf("a refused creation wrote the vault: %q", tools.Vault)
	}
	for _, e := range createEvents(t, a) {
		if !strings.Contains(e.Detail, "failed: ") {
			t.Errorf("a refusal logged %q", e.Detail)
		}
	}
}

// A redirect whose state is not this run's, and a Create click that does
// not come within the timeout, are refused in one line with nothing
// written; in an agent session holding the browser lease the flow runs.
func TestAppCreateRefusesTheRedirectOrTheWait(t *testing.T) {
	a, g, tools := createApp(t, nil)
	t.Setenv(agentMarkers[0], "1")
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(browserResource, lease.Holder{Holder: a.as, Name: a.as}); err != nil {
		t.Fatal(err)
	}
	g.wrongState = true
	_, err := runApp(t, a, appCreate, fixtureApp, timeoutFlag, tenSeconds)
	if Code(err) != ExitRefused || err == nil || err.Error() != "the redirect's state is not this run's: nothing written" {
		t.Errorf("another state: %v", err)
	}
	g.wrongState = false
	openBrowser = func(context.Context, string) error { return nil }
	a.out = &bytes.Buffer{}
	_, err = runApp(t, a, appCreate, fixtureApp, timeoutFlag, "100ms")
	if Code(err) != ExitRefused || err == nil || err.Error() != "no redirect from GitHub within 100ms: the Create click did not come; nothing written" {
		t.Errorf("no Create click: %v", err)
	}
	if calls := strings.Join(tools.Calls, "\n"); strings.Contains(calls, opItemCall) {
		t.Errorf("a refused creation ran op:\n%s", calls)
	}
	if len(tools.Vault) != 1 {
		t.Errorf("a refused creation wrote the vault: %q", tools.Vault)
	}
	if evs := createEvents(t, a); len(evs) != 2 || !strings.Contains(evs[0].Detail, "failed: the redirect's state") || !strings.Contains(evs[1].Detail, "failed: no redirect") {
		t.Errorf("the log: %+v", evs)
	}
}

// An Install click that does not come leaves the credentials as written
// and says how the installation's id is recorded.
func TestAppCreateWaitsForTheInstallClick(t *testing.T) {
	a, g, tools := createApp(t, nil)
	g.noInstall = true
	out, err := runApp(t, a, appCreate, fixtureApp, timeoutFlag, "300ms")
	if Code(err) != ExitRefused || err == nil || !strings.HasPrefix(err.Error(), "the vault holds the App's credentials; no installation on "+fixtureOrg+" within 300ms") ||
		!strings.Contains(err.Error(), "beekeeper secret store "+fixtureItem+"/"+appInstallationID) {
		t.Errorf("no Install click: %v\n%s", err, out)
	}
	if tools.Vault[fixtureItem+"/"+githubClientSecret] != fixtureSecret || tools.Vault[fixtureItem+"/"+appInstallationID] != "" {
		t.Errorf("the vault holds %q", tools.Vault)
	}
	if !strings.Contains(out, "wrote "+fixtureItem+": app-id 4242") {
		t.Errorf("the answer lacks the write:\n%s", out)
	}
	noFixtureSecret(t, "the answer", out+err.Error())
	if evs := createEvents(t, a); len(evs) != 1 || !strings.Contains(evs[0].Detail, "failed: the vault holds the App's credentials") {
		t.Errorf("the log: %+v", evs)
	}
}

// GitHub's slug of a name, and the installation page's query.
func TestAppSlugAndInstallURL(t *testing.T) {
	for name, want := range map[string]string{"Example Lab Skills": fixtureApp, fixtureApp: fixtureApp, "  My_App 2!": "my-app-2"} {
		if got := appSlug(name); got != want {
			t.Errorf("appSlug(%q) = %q, want %q", name, got, want)
		}
	}
	prev := githubWeb
	githubWeb = "https://github.example"
	t.Cleanup(func() { githubWeb = prev })
	if got := installURL("my-app", 77, []int64{101, 102}); got != "https://github.example/apps/my-app/installations/new/permissions?repository_ids%5B%5D=101&repository_ids%5B%5D=102&suggested_target_id=77" {
		t.Errorf("installURL = %q", got)
	}
	if got := installURL("my-app", 77, nil); got != "https://github.example/apps/my-app/installations/new/permissions?suggested_target_id=77" {
		t.Errorf("installURL for all = %q", got)
	}
}
