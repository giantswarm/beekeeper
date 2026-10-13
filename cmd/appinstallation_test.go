//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The fake installation's tokens and the vault's selector token: obvious
// fakes.
const (
	installToken  = "planted-Installation-Token-6d2a"
	selectorToken = "planted-Selector-Token-8e4b"
	selectorRef   = "op://Shared/installation-selector/credential"
	installCmd    = "installation"
	reposFlag     = "--repos"
)

// fakeInstallation is GitHub's API for the fixture App's one installation
// on the fixture organisation: the App's token (a JWT under the fixture
// key) reads and suspends it, an installation token reads its
// repositories, and only the selector token changes them. Repository ids
// are 100 + their index in repoIDs.
type fakeInstallation struct {
	mu        sync.Mutex
	selection string
	installed bool
	suspended bool
	repos     []string
	// calls are the requests in order, "METHOD path".
	calls   []string
	revoked bool
}

// repoIDs are the organisation's repositories by id.
var repoIDs = []string{"agentlab", "workspace-manager", "sandbox"}

func newFakeInstallation(t *testing.T, selection string, repos ...string) *fakeInstallation {
	t.Helper()
	f := &fakeInstallation{selection: selection, installed: true, repos: repos}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	prevWeb, prevAPI, prevGH := githubWeb, githubAPI, appsGH
	githubWeb, githubAPI = srv.URL, srv.URL
	appsGH = func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		if call == "api orgs/"+fixtureOrg+" --jq .id" {
			return []byte(fixtureOrgID + "\n"), nil
		}
		for i, r := range repoIDs {
			if call == "api repos/"+fixtureOrg+"/"+r+" --jq .id" {
				return []byte(strconv.Itoa(100+i) + "\n"), nil
			}
		}
		return nil, errors.New("gh: Not Found (HTTP 404)")
	}
	t.Cleanup(func() { githubWeb, githubAPI, appsGH = prevWeb, prevAPI, prevGH })
	return f
}

func (f *fakeInstallation) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	auth := r.Header.Get("Authorization")
	asApp := (&fakeGitHub{}).appToken(auth)
	asInst := auth == "token "+installToken
	inst := "/app/installations/" + strconv.FormatInt(fixtureInstall, 10)
	deny := func(perm string) {
		if perm != "" {
			w.Header().Set("X-Accepted-GitHub-Permissions", perm)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message": "Resource not accessible by integration"}`)
	}
	switch p := r.URL.Path; {
	case p == "/login/device/code" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		if r.FormValue("client_id") != fixtureClientID || r.Header.Get("Accept") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"error": "device_flow_disabled", "error_description": "Device Flow must be explicitly enabled for this App"}`)
	case !asApp && !asInst && !strings.HasPrefix(p, "/user/installations/"):
		w.WriteHeader(http.StatusUnauthorized)
	case p == "/app" && asApp:
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fixtureAppID, "slug": fixtureApp, "client_id": fixtureClientID})
	case p == "/orgs/"+fixtureOrg+"/installation" && asApp:
		if !f.installed {
			http.NotFound(w, r)
			return
		}
		var at any
		if f.suspended {
			at = "2026-10-13T13:30:10Z"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fixtureInstall, "repository_selection": f.selection, "suspended_at": at})
	case p == inst+"/suspended" && asApp:
		f.suspended = r.Method == http.MethodPut
		w.WriteHeader(http.StatusNoContent)
	case p == inst && r.Method == http.MethodDelete && asApp:
		f.installed = false
		w.WriteHeader(http.StatusNoContent)
	case p == inst+"/access_tokens" && asApp:
		if f.suspended {
			deny("")
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token": "`+installToken+`"}`)
	case p == "/installation/repositories" && asInst:
		reached := f.repos
		if f.selection == "all" {
			reached = repoIDs
		}
		var list []map[string]any
		for _, n := range reached {
			list = append(list, map[string]any{"name": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(reached), "repositories": list})
	case p == "/installation/token" && asInst:
		f.revoked = true
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(p, "/user/installations/"):
		if auth != "Bearer "+selectorToken {
			deny(selectionPermission)
			return
		}
		id, _ := strconv.Atoi(p[strings.LastIndex(p, "/")+1:])
		name := repoIDs[id-100]
		switch {
		case f.selection != selectedRepos:
			w.WriteHeader(http.StatusUnprocessableEntity)
		case r.Method == http.MethodPut:
			if !slices.Contains(f.repos, name) {
				f.repos = append(f.repos, name)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			f.repos = slices.DeleteFunc(f.repos, func(x string) bool { return x == name })
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.NotFound(w, r)
	}
}

// installationApp is secretApp with the fixture App's key in the Secret
// apps.secrets names for the fixture organisation, and a selector token in
// the shared vault.
func installationApp(t *testing.T) (*app, *secrettest.Tools) {
	t.Helper()
	a, tools, repo := secretApp(t)
	_, keyPEM := fixtureKey()
	file := githubAppConfig(a, repo)
	plain := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: github-app\n  namespace: team-agents\nstringData:\n  private-key: |\n    " +
		strings.ReplaceAll(strings.TrimSpace(keyPEM), "\n", "\n    ") + "\n"
	if err := os.WriteFile(file, secrettest.Encrypt(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Apps.Repo = fixtureOrg + "/github"
	tools.Vault[selectorRef] = selectorToken
	return a, tools
}

// runInstallation runs app installation for the fixture organisation's
// Secret and App, with args after them.
func runInstallation(t *testing.T, a *app, args ...string) (string, error) {
	t.Helper()
	out, err := runApp(t, a, append([]string{installCmd, fixtureOrg, "--app", fixtureAppIDText}, args...)...)
	noFixtureSecret(t, "the output", out)
	for _, v := range []string{installToken, selectorToken} {
		if strings.Contains(out, v) {
			t.Errorf("the output carries a token:\n%s", out)
		}
	}
	return out, err
}

// installationEvents are the app.installation events, each without a value.
func installationEvents(t *testing.T, a *app) []state.Event {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == appInstallationOp })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		noFixtureSecret(t, "the log", e.Detail)
		if strings.Contains(e.Detail, installToken) || strings.Contains(e.Detail, selectorToken) {
			t.Errorf("the log carries a token: %s", e.Detail)
		}
	}
	return evs
}

// A read acts as the App with the Secret's key: the installation, its
// selection and the repositories its token reaches, the token revoked, the
// device flow probed with the client id, the expiry named unread, and the
// same in the log.
func TestAppInstallationReadsTheScopeAsTheApp(t *testing.T) {
	a, _ := installationApp(t)
	f := newFakeInstallation(t, selectedRepos, "agentlab", "workspace-manager")
	out, err := runInstallation(t, a)
	if err != nil {
		t.Fatalf("app installation: %v\n%s", err, out)
	}
	for _, want := range []string{"app " + fixtureApp + " (id 4242, client id " + fixtureClientID + ")",
		"installation 9001 on example: selected repositories, 2 reached (agentlab, workspace-manager)", "device flow: disabled", "user token expiry: unread"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	if !f.revoked {
		t.Error("the installation token was not revoked")
	}
	evs := installationEvents(t, a)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "installation 9001 on example: selected repositories, 2 reached") {
		t.Errorf("the log = %+v", evs)
	}
}

// An installation on all repositories answers their count, not their
// names; --repos with a token is refused naming the setting, nothing
// changed.
func TestAppInstallationAllRepositories(t *testing.T) {
	a, _ := installationApp(t)
	f := newFakeInstallation(t, "all")
	out, err := runInstallation(t, a)
	if err != nil || !strings.Contains(out, "all repositories, 3 reached\n") || strings.Contains(out, "workspace-manager") {
		t.Fatalf("app installation: %v\n%s", err, out)
	}
	out, err = runInstallation(t, a, reposFlag, "agentlab", "--token", selectorRef)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "reaches all repositories of example: no API narrows such an installation") {
		t.Fatalf("--repos on all = %v\n%s", err, out)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, "/user/installations/") }) {
		t.Errorf("a change was sent: %v", f.calls)
	}
}

// --repos makes the selection exactly the named repositories with the
// selector token, additions first, and reads it back.
func TestAppInstallationAppliesTheSelection(t *testing.T) {
	a, _ := installationApp(t)
	f := newFakeInstallation(t, selectedRepos, "agentlab", "workspace-manager")
	out, err := runInstallation(t, a, reposFlag, "agentlab,sandbox", "--token", selectorRef)
	if err != nil {
		t.Fatalf("app installation --repos: %v\n%s", err, out)
	}
	for _, want := range []string{"2 reached (agentlab, sandbox)", "added sandbox", "removed workspace-manager"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer lacks %q:\n%s", want, out)
		}
	}
	put := slices.Index(f.calls, "PUT /user/installations/9001/repositories/102")
	del := slices.Index(f.calls, "DELETE /user/installations/9001/repositories/101")
	if put < 0 || del < put {
		t.Errorf("the changes = %v, want the addition before the removal", f.calls)
	}
	evs := installationEvents(t, a)
	if len(evs) != 1 || !strings.Contains(evs[0].Detail, "added sandbox; removed workspace-manager") {
		t.Errorf("the log = %+v", evs)
	}
}

// --repos is refused without a selector token before any call; a token
// GitHub refuses names the permission it accepts.
func TestAppInstallationNeedsASelectorToken(t *testing.T) {
	a, tools := installationApp(t)
	f := newFakeInstallation(t, selectedRepos, "agentlab")
	if _, err := runInstallation(t, a, reposFlag, "sandbox"); Code(err) != ExitRefused || !strings.Contains(err.Error(), selectionPermission) {
		t.Fatalf("--repos without --token = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls before the refusal: %v", f.calls)
	}
	tools.Vault[selectorRef] = "planted-Other-Token-1a1a"
	_, err := runInstallation(t, a, reposFlag, "sandbox", "--token", selectorRef)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "adding sandbox") || !strings.Contains(err.Error(), "accepts the permission "+selectionPermission) {
		t.Fatalf("--repos with a token GitHub refuses = %v", err)
	}
}

// --suspend and --unsuspend set the suspension as the App and read it back;
// a suspended installation answers no repositories and refuses --repos.
func TestAppInstallationSuspends(t *testing.T) {
	a, _ := installationApp(t)
	f := newFakeInstallation(t, "all")
	out, err := runInstallation(t, a, "--suspend")
	if err != nil || !strings.Contains(out, "all repositories, suspended since 2026-10-13T13:30:10Z") || !strings.Contains(out, "\nsuspended\n") || !f.suspended {
		t.Fatalf("--suspend: %v\n%s", err, out)
	}
	if slices.Contains(f.calls, "POST /app/installations/9001/access_tokens") {
		t.Errorf("a suspended installation was asked for a token: %v", f.calls)
	}
	f.selection = selectedRepos
	if _, err := runInstallation(t, a, reposFlag, "sandbox", "--token", selectorRef); Code(err) != ExitRefused || !strings.Contains(err.Error(), "is suspended since") {
		t.Fatalf("--repos while suspended = %v", err)
	}
	out, err = runInstallation(t, a, "--unsuspend")
	if err != nil || !strings.Contains(out, "\nunsuspended\n") || f.suspended || !strings.Contains(out, "selected repositories, 0 reached") {
		t.Fatalf("--unsuspend: %v\n%s", err, out)
	}
}

// --reinstall looks the repositories up before it deletes the installation
// as the App, and answers the installation page with them preselected; a
// repository the organisation lacks deletes nothing. The next read says
// the App is installed nowhere.
func TestAppInstallationReinstall(t *testing.T) {
	a, _ := installationApp(t)
	f := newFakeInstallation(t, "all")
	if _, err := runInstallation(t, a, reposFlag, "agentlab,missing", "--reinstall"); Code(err) != ExitRefused || !strings.Contains(err.Error(), "nothing deleted") || !f.installed {
		t.Fatalf("--reinstall with a missing repository = %v, installed %v", err, f.installed)
	}
	out, err := runInstallation(t, a, reposFlag, "agentlab,sandbox", "--reinstall")
	if err != nil || f.installed {
		t.Fatalf("--reinstall: %v, installed %v\n%s", err, f.installed, out)
	}
	want := githubWeb + "/apps/" + fixtureApp + "/installations/new/permissions?repository_ids%5B%5D=100&repository_ids%5B%5D=102&suggested_target_id=" + fixtureOrgID
	if !strings.Contains(out, "installation 9001 on example: uninstalled\n") || !strings.Contains(out, "click Install at "+want+"\n") {
		t.Errorf("the answer lacks the uninstall and %s:\n%s", want, out)
	}
	if _, err := runInstallation(t, a); Code(err) != ExitRefused || !strings.Contains(err.Error(), "not installed on example") {
		t.Errorf("the read after = %v", err)
	}
}

// The flags' combinations are checked before any call.
func TestAppInstallationUsage(t *testing.T) {
	a, _ := installationApp(t)
	f := newFakeInstallation(t, selectedRepos)
	for _, args := range [][]string{{"--suspend", "--unsuspend"}, {"--reinstall"}, {reposFlag, "a", "--reinstall", "--token", selectorRef},
		{reposFlag, "a", "--suspend"}, {"--token", selectorRef}} {
		if _, err := runInstallation(t, a, args...); Code(err) != ExitUsage {
			t.Errorf("%v = %v, want a usage error", args, err)
		}
	}
	if _, err := runApp(t, a, installCmd, fixtureOrg); Code(err) != ExitUsage {
		t.Errorf("no --app = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls: %v", f.calls)
	}
}
