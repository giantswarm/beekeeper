//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	manifestFlag = "--manifest"
	orgFlag      = "--org"
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
		if call := strings.Join(args, " "); call == "api apps/taken" {
			return []byte(`{"id": 7, "slug": "taken", "html_url": "https://github.com/apps/taken", "owner": {"login": "someone"}}`), nil
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
	if !g.appPage {
		t.Error("the browser never reached the App's installation page")
	}
	g.mu.Unlock()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret."+githubAppOp })
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Detail, "app "+fixtureApp+", app-id 4242") {
		t.Fatalf("the log: %+v, %v", evs, err)
	}
	noFixtureSecret(t, "the log", evs[0].Detail)
	if strings.Contains(evs[0].Detail, fixtureCode) {
		t.Errorf("the log carries the code: %s", evs[0].Detail)
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
		"no org":       {[]string{fixtureOrg, manifestFlag, manifest}, ExitUsage, "--org names the organisation"},
		"flags":        {[]string{fixtureApp, "--no-open"}, ExitUsage, "go with --manifest"},
	} {
		_, err := runManifest(a, &bytes.Buffer{}, tc.args...)
		if Code(err) != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (exit %d), want exit %d with %q", name, err, Code(err), tc.code, tc.want)
		}
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
