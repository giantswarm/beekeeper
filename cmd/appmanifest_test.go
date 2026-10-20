//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	manifestFlag = "--manifest"
	orgFlag      = "--org"
	userFlag     = "--user"
	// fixtureUser is the personal account of --user, fixtureUserID its id,
	// fixtureRepoID the id of the repository an installation file names.
	fixtureUser   = "example-person"
	fixtureUserID = "88"
	fixtureRepoID = "901"
	fixtureRepo   = "workspaces"
)

// pageLine is the progress line that names the loopback page.
var pageLine = regexp.MustCompile(`open(?:ing)? (http://127\.0\.0\.1:\d+)/ in the browser`)

// browsingWriter is the caller of --no-open: it reads the progress and
// opens the loopback page it names, as beekeeper browse would.
type browsingWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	opened bool
}

func (w *browsingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if m := pageLine.FindStringSubmatch(w.buf.String()); m != nil && !w.opened {
		w.opened = true
		go func() { _ = browse(m[1] + "/") }()
	}
	return n, err
}

func (w *browsingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// manifestApp is secretApp with the installation example's Secret, a local
// manifest of the fixture App, the fake GitHub (which knows no App of that
// name), and a browser that runs the flow by itself; in the person's
// terminal. It answers the Secret's file and the manifest's.
func manifestApp(t *testing.T) (*app, *fakeGitHub, *secrettest.Tools, string, string) {
	t.Helper()
	a, tools, repo := secretApp(t)
	file := githubAppConfig(a, repo)
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifest, []byte(fixtureManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newFakeGitHub(t)
	prevGH, prevOpen := appsGH, openBrowser
	t.Cleanup(func() { appsGH, openBrowser = prevGH, prevOpen })
	appsGH = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "api apps/taken":
			return []byte(`{"id": 7, "slug": "taken", "html_url": "https://github.com/apps/taken", "owner": {"login": "someone"}}`), nil
		case "api orgs/" + fixtureOrg + " --jq .id":
			return []byte(fixtureOrgID + "\n"), nil
		case "api users/" + fixtureUser + " --jq .id":
			return []byte(fixtureUserID + "\n"), nil
		case "api repos/" + fixtureUser + "/" + fixtureRepo + " --jq .id":
			return []byte(fixtureRepoID + "\n"), nil
		}
		return nil, errors.New("gh: Not Found (HTTP 404)")
	}
	openBrowser = func(_ context.Context, page string) error {
		go func() { _ = browse(page) }()
		return nil
	}
	for _, k := range agentMarkers {
		t.Setenv(k, "")
	}
	return a, g, tools, file, manifest
}

// runManifest runs app create with stdout in a.out and stderr in errOut.
func runManifest(a *app, errOut interface{ Write([]byte) (int, error) }, args ...string) (string, error) {
	a.out = &bytes.Buffer{}
	c := a.appCmd()
	c.SetArgs(append([]string{appCreate}, args...))
	c.SetOut(a.out)
	c.SetErr(errOut)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

// --manifest creates the App a local manifest declares through the
// loopback listener, converts the code GitHub redirects with in-process
// and writes the credentials into the installation's SOPS Secret; stdout
// is the App's id and client id alone, and the browser goes on to the
// App's installation page.
func TestAppCreateManifestWritesTheSecret(t *testing.T) {
	a, g, tools, file, manifest := manifestApp(t)
	var stderr bytes.Buffer
	out, err := runManifest(a, &stderr, fixtureOrg, manifestFlag, manifest, orgFlag, fixtureOrg, timeoutFlag, tenSeconds)
	if err != nil {
		t.Fatalf("app create --manifest: %v\n%s", err, stderr.String())
	}
	if out != "app-id 4242\nclient-id "+fixtureClientID+"\n" {
		t.Errorf("stdout = %q, want the App's id and client id alone", out)
	}
	_, keyPEM := fixtureKey()
	plain, err := tools.Run(context.Background(), "", nil, nil, "sops", "decrypt", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: github-app", "namespace: team-agents", strings.Split(keyPEM, "\n")[1], "client-secret: " + fixtureSecret, "webhook-secret: " + fixtureWebhook} {
		if !strings.Contains(string(plain), want) {
			t.Errorf("the Secret lacks %q", want)
		}
	}
	for _, want := range []string{"App " + fixtureApp + ", declared in " + manifest, "opening http://127.0.0.1:", `Click "Create GitHub App" there`,
		"wrote " + file + "#stringData.private-key: ", "wrote " + file + "#stringData.client-secret: "} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	noFixtureSecret(t, "stdout and stderr", out+stderr.String())
	if strings.Contains(stderr.String(), fixtureCode) {
		t.Errorf("stderr carries the code:\n%s", stderr.String())
	}
	g.mu.Lock()
	redirect, _ := g.manifest[appRedirectField].(string)
	if g.manifest["name"] != fixtureApp || !strings.HasPrefix(redirect, "http://127.0.0.1:") || !strings.HasSuffix(redirect, appCallbackPath) || g.state == "" {
		t.Errorf("GitHub received the manifest %v with state %q", g.manifest, g.state)
	}
	g.mu.Unlock()
	g.mu.Lock()
	if g.createPath != "/organizations/"+fixtureOrg+"/settings/apps/new" {
		t.Errorf("the manifest went to %s, want the organisation's page", g.createPath)
	}
	g.mu.Unlock()
	// the browser follows the callback's redirect on its own, after the
	// command answered it, to the installation page with the organisation
	// preselected and, without an installation file, all its repositories
	if q := installQuery(t, g); q.Get("suggested_target_id") != fixtureOrgID || q.Has("repository_ids[]") {
		t.Errorf("the installation page's query = %v, want the organisation %s and no repository", q, fixtureOrgID)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret."+githubAppOp })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "app "+fixtureApp+", app-id 4242") {
		t.Fatalf("the log: %+v, %v", evs, err)
	}
	noFixtureSecret(t, "the log", evs[0].Detail)
	if strings.Contains(evs[0].Detail, fixtureCode) {
		t.Errorf("the log carries the code: %s", evs[0].Detail)
	}
	// the run is off the record, the App in beekeeper's ledger with the
	// Secret that holds its credentials
	if st, err := a.store.Read(); err != nil || len(st.AppRuns) != 0 || len(st.AppsCreated) != 1 || st.AppsCreated[0].Slug != fixtureApp ||
		st.AppsCreated[0].Stored != file || st.AppsCreated[0].Org != fixtureOrg || st.AppsCreated[0].Declared != manifest {
		t.Errorf("after the run: runs %+v, ledger %+v, %v", st.AppRuns, st.AppsCreated, err)
	}
}

// installQuery waits for the browser to reach the App's installation page
// and answers the page's query.
func installQuery(t *testing.T, g *fakeGitHub) url.Values {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		g.mu.Lock()
		q := g.installQuery
		g.mu.Unlock()
		if q != nil {
			return q
		}
	}
	t.Fatal("the browser never reached the App's installation page")
	return nil
}

// --user creates the App under the personal account: the manifest goes to
// GitHub's settings/apps/new, the conversion runs in this process and the
// credentials land in the installation's SOPS Secret, and the installation
// page preselects the account and the repositories the installation file
// beside the manifest names.
func TestAppCreateManifestForAPersonalAccount(t *testing.T) {
	a, g, tools, file, manifest := manifestApp(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(manifest), "installation.json"), []byte(`{"repositories": ["`+fixtureRepo+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	out, err := runManifest(a, &stderr, fixtureOrg, manifestFlag, manifest, userFlag, fixtureUser, timeoutFlag, tenSeconds)
	if err != nil {
		t.Fatalf("app create --manifest --user: %v\n%s", err, stderr.String())
	}
	if out != "app-id 4242\nclient-id "+fixtureClientID+"\n" {
		t.Errorf("stdout = %q, want the App's id and client id alone", out)
	}
	if !strings.Contains(stderr.String(), "it submits the manifest to GitHub (settings/apps/new)") {
		t.Errorf("stderr names no personal manifest page:\n%s", stderr.String())
	}
	noFixtureSecret(t, "stdout and stderr", out+stderr.String())
	plain, err := tools.Run(context.Background(), "", nil, nil, "sops", "decrypt", file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), "client-secret: "+fixtureSecret) {
		t.Error("the Secret lacks the App's client secret")
	}
	g.mu.Lock()
	if g.createPath != personalNewApp || g.manifest["name"] != fixtureApp {
		t.Errorf("the manifest %v went to %s, want the personal account's settings/apps/new", g.manifest, g.createPath)
	}
	g.mu.Unlock()
	if q := installQuery(t, g); q.Get("suggested_target_id") != fixtureUserID || !slices.Equal(q["repository_ids[]"], []string{fixtureRepoID}) {
		t.Errorf("the installation page's query = %v, want the account %s and the repository %s", q, fixtureUserID, fixtureRepoID)
	}
	if st, err := a.store.Read(); err != nil || len(st.AppsCreated) != 1 || st.AppsCreated[0].Org != fixtureUser || st.AppsCreated[0].Stored != file {
		t.Errorf("the ledger: %+v, %v", st.AppsCreated, err)
	}
}

// An App app create --manifest --user created on the person's own account,
// which apps.repo does not declare, is recorded for a consent from
// beekeeper's ledger: its local manifest's callback hosts, its owner and
// client id as the App itself answers them with the key in the Secret. The
// hook then answers its consent page for a declared callback host alone.
// Another client id, the settings changes of an organisation's App, an
// owner other than the person, a manifest naming another App and an App
// declared nowhere are refused, naming where allow looked.
func TestAppAllowPersonalAccountApp(t *testing.T) {
	a, _, _, _, manifest := manifestApp(t)
	const callback = "muster.example-person.test"
	declaration := `{"name": "` + fixtureApp + `", "url": "https://example.org", "public": false, "callback_urls": ["https://` + callback + `/oauth/proxy/callback"]}`
	if err := os.WriteFile(manifest, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if _, err := runManifest(a, &stderr, fixtureOrg, manifestFlag, manifest, userFlag, fixtureUser, timeoutFlag, tenSeconds); err != nil {
		t.Fatalf("app create --manifest --user: %v\n%s", err, stderr.String())
	}
	a.cfg.Apps.Repo, a.cfg.Apps.Manifest = fixtureOrg+"/github", "apps/{name}/manifest.json"
	person := fixtureUser
	created := appsGH
	appsGH = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "api user --jq .login" {
			return []byte(person + "\n"), nil
		}
		return created(ctx, args...)
	}
	allow := func(args ...string) (string, error) {
		a.out = &bytes.Buffer{}
		out, err := runApp(t, a, append([]string{appAllow, fixtureApp, wordFlag, personsWord}, args...)...)
		noFixtureSecret(t, "app allow's answer", out)
		return out, err
	}
	other := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(other, []byte(`{"name": "another-app"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"another client id": {[]string{clientIDFlag, "Iv1.another"}, "App " + fixtureApp + " (id 4242) has the client id " + fixtureClientID + ", not Iv1.another"},
		"a permission":      {[]string{permissionFlag, deviceChange}, "App " + fixtureApp + " is on " + fixtureUser + "'s own account: the hook answers a permission change or a callback URL on an organisation's settings pages alone"},
		"another manifest":  {[]string{clientIDFlag, fixtureClientID, manifestFlag, other}, "App " + fixtureApp + " is not declared: " + other + " names the App another-app"},
	} {
		if _, err := allow(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: app allow = %v, want %q", name, err, tc.want)
		}
	}
	person = "someone-else"
	if _, err := allow(clientIDFlag, fixtureClientID); err == nil ||
		!strings.Contains(err.Error(), "belongs to the user "+fixtureUser+", not to the person's own account someone-else") {
		t.Errorf("app allow for another person's App = %v", err)
	}
	person = fixtureUser
	a.out = &bytes.Buffer{}
	if _, err := runApp(t, a, appAllow, "nowhere", clientIDFlag, fixtureClientID, wordFlag, personsWord); err == nil ||
		!strings.Contains(err.Error(), "App nowhere is not declared: ") || !strings.Contains(err.Error(), "nor does beekeeper's ledger hold an App nowhere") {
		t.Errorf("app allow for an App declared nowhere = %v", err)
	}
	out, err := allow(clientIDFlag, fixtureClientID)
	declared := manifest + " (App 4242 on " + fixtureUser + "'s own account)"
	if err != nil || out != "App "+fixtureApp+" on record, declared in "+declared+": its consent page with callback "+callback+" is answered by the hook\n" {
		t.Fatalf("app allow = %q, %v", out, err)
	}
	st, err := a.store.Read()
	if err != nil || len(st.Apps) != 1 || st.Apps[0].Org != fixtureUser || st.Apps[0].ClientID != fixtureClientID || st.Apps[0].Declared != declared {
		t.Fatalf("the record = %+v, %v", st.Apps, err)
	}
	consent := "https://github.com/login/oauth/authorize?client_id=" + fixtureClientID + "&redirect_uri=https%3A%2F%2F" + callback + "%2Foauth%2Fproxy%2Fcallback&response_type=code&state=x"
	title := "Authorize " + fixtureApp
	if out, _ := runApp(t, a, appCheck, consent, titleFlag, title); !strings.HasPrefix(out, checkAllow) {
		t.Errorf("check on the App's consent page = %q", out)
	}
	if out, _ := runApp(t, a, appCheck, strings.ReplaceAll(consent, callback, "example.org"), titleFlag, title); !strings.HasPrefix(out, checkRefuse) {
		t.Errorf("check on another callback host = %q", out)
	}
}

// --no-open opens no browser: the caller opens the page the progress
// names, and the flow runs as before.
func TestAppCreateManifestNoOpen(t *testing.T) {
	a, _, _, _, manifest := manifestApp(t)
	openBrowser = func(context.Context, string) error {
		t.Error("--no-open opened the browser")
		return nil
	}
	w := &browsingWriter{}
	out, err := runManifest(a, w, fixtureOrg, manifestFlag, manifest, orgFlag, fixtureOrg, "--no-open", timeoutFlag, tenSeconds)
	if err != nil || out != "app-id 4242\nclient-id "+fixtureClientID+"\n" {
		t.Fatalf("--no-open = %q, %v\n%s", out, err, w.String())
	}
	if !strings.Contains(w.String(), "open http://127.0.0.1:") {
		t.Errorf("the progress names no page:\n%s", w.String())
	}
}

// Refused before any page: an installation apps.secrets lacks, a manifest
// without a name, an App GitHub knows by that name, no organisation, and
// --manifest's flags without it; a redirect of another state writes
// nothing.
func TestAppCreateManifestRefuses(t *testing.T) {
	a, g, tools, file, manifest := manifestApp(t)
	opened := 0
	openBrowser = func(context.Context, string) error { opened++; return nil }
	dir := t.TempDir()
	nameless, taken := filepath.Join(dir, "nameless.json"), filepath.Join(dir, "taken.json")
	_ = os.WriteFile(nameless, []byte(`{"url": "https://example.org"}`), 0o600)
	_ = os.WriteFile(taken, []byte(takenManifest), 0o600)
	for name, tc := range map[string]struct {
		args []string
		code int
		want string
	}{
		"installation": {[]string{"elsewhere", manifestFlag, manifest, orgFlag, fixtureOrg}, ExitUsage, `apps.secrets names no Secret for "elsewhere"`},
		"nameless":     {[]string{fixtureOrg, manifestFlag, nameless, orgFlag, fixtureOrg}, ExitUsage, `names no App ("name")`},
		"no file":      {[]string{fixtureOrg, manifestFlag, filepath.Join(dir, "absent.json"), orgFlag, fixtureOrg}, ExitUsage, "--manifest: "},
		"known":        {[]string{fixtureOrg, manifestFlag, taken, orgFlag, fixtureOrg}, ExitRefused, "GitHub already knows an App named taken"},
		"no org":       {[]string{fixtureOrg, manifestFlag, manifest}, ExitUsage, "--org or --user names the account"},
		"org and user": {[]string{fixtureOrg, manifestFlag, manifest, orgFlag, fixtureOrg, userFlag, fixtureUser}, ExitUsage, "--org or --user owns the App, not both"},
		"flags":        {[]string{fixtureApp, "--no-open"}, ExitUsage, "go with --manifest"},
		"user flag":    {[]string{fixtureApp, userFlag, fixtureUser}, ExitUsage, "go with --manifest"},
		"no account":   {[]string{fixtureOrg, manifestFlag, manifest, userFlag, "absent-person"}, ExitError, "the account absent-person: "},
	} {
		_, err := runManifest(a, &bytes.Buffer{}, tc.args...)
		if Code(err) != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (exit %d), want exit %d with %q", name, err, Code(err), tc.code, tc.want)
		}
	}
	// an installation file that names a repository the account lacks, or
	// is no installation file, refuses before any page
	inst := filepath.Join(filepath.Dir(manifest), "installation.json")
	for content, want := range map[string]string{
		`{"repositories": ["absent"]}`: "names the repository absent, which " + fixtureUser + " lacks; nothing written",
		`{"repositories": "some"}`:     inst + `: "repositories" is "all" or a list of names`,
	} {
		if err := os.WriteFile(inst, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runManifest(a, &bytes.Buffer{}, fixtureOrg, manifestFlag, manifest, userFlag, fixtureUser); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("installation file %s: %v, want %q", content, err, want)
		}
	}
	if err := os.Remove(inst); err != nil {
		t.Fatal(err)
	}
	if opened != 0 {
		t.Errorf("a refusal opened %d pages", opened)
	}
	openBrowser = func(_ context.Context, page string) error {
		go func() { _ = browse(page) }()
		return nil
	}
	g.mu.Lock()
	g.wrongState = true
	g.mu.Unlock()
	_, err := runManifest(a, &bytes.Buffer{}, fixtureOrg, manifestFlag, manifest, orgFlag, fixtureOrg, timeoutFlag, tenSeconds)
	if Code(err) != ExitRefused || err == nil || err.Error() != "the redirect's state is not this run's: nothing written" {
		t.Errorf("another state: %v", err)
	}
	if _, statErr := os.Stat(file); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a refused creation wrote %s", file)
	}
	_ = tools
}
