//go:build unix

package cmd

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// fakeGH writes a gh into a new directory that prints tok as its login's
// token, and fails when a token variable reaches it.
// testOrg is the org the person tests ask about.
const testOrg = "acme"

func fakeGH(t *testing.T, tok string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ -z \"$GH_TOKEN$GITHUB_TOKEN\" ] || exit 9\necho " + tok + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { //nolint:gosec // a fake gh
		t.Fatal(err)
	}
	return dir
}

// personGitHub stands in for GitHub: the person's token is an active
// member's and lists a private member, any other token's own membership is
// refused as an integration's. It counts the roster reads.
func personGitHub(t *testing.T) (reads *int) {
	t.Helper()
	reads = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer persontok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/user/memberships/orgs/acme":
			_, _ = w.Write([]byte(`{"state":"active"}`))
		case "/orgs/acme/members":
			*reads++
			_, _ = w.Write([]byte(`[{"login":"PrivateMember"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
	return reads
}

// personApp is an app whose PATH has the agent's gh link first (the App's
// token, in agents.shell.path) and the person's own gh after it, with the
// App's token in GH_TOKEN as an agent shell carries it.
func personApp(t *testing.T, personTok string) *app {
	t.Helper()
	link, own := fakeGH(t, "apptok"), fakeGH(t, personTok)
	t.Setenv("PATH", link+string(os.PathListSeparator)+own+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "apptok")
	cfg := &config.Config{StateDir: t.TempDir()}
	cfg.Agents.Shell.Path = []string{link}
	return &app{cfg: cfg, now: time.Now()}
}

func askPerson(t *testing.T, a *app, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c := a.personCmd()
	c.SetArgs(append([]string{"--org", testOrg}, args...))
	c.SetOut(&out)
	c.SetErr(&out)
	err := c.Execute()
	return out.String(), err
}

// A private member is a member and an outsider is not, both answered from
// the person's own login past the agent's gh and GH_TOKEN; the roster is
// read once a day.
func TestPersonAnswersFromTheKeptRoster(t *testing.T) {
	reads := personGitHub(t)
	a := personApp(t, "persontok")
	for login, want := range map[string]string{"privatemember": github.Member, "outsider": github.NotMember, "PrivateMember": github.Member} {
		if out, err := askPerson(t, a, login); err != nil || out != want+"\n" {
			t.Errorf("person %s = %q, %v, want %q", login, out, err, want)
		}
	}
	if *reads != 1 {
		t.Errorf("roster read %d times, want once", *reads)
	}
	if _, err := askPerson(t, a, "--refresh", "outsider"); err != nil || *reads != 2 {
		t.Errorf("person --refresh = %v after %d reads, want a second read", err, *reads)
	}
	b, err := os.ReadFile(a.rosterPath(testOrg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tok") {
		t.Errorf("the kept roster %s holds a token", b)
	}
}

// A roster older than a day is read again.
func TestPersonRereadsAStaleRoster(t *testing.T) {
	reads := personGitHub(t)
	a := personApp(t, "persontok")
	if err := writeKept(a.rosterPath(testOrg), []byte(`{"org":"acme","members":["someone"],"at":"`+time.Now().Add(-25*time.Hour).UTC().Format(time.RFC3339)+`"}`)); err != nil {
		t.Fatal(err)
	}
	if out, err := askPerson(t, a, "privatemember"); err != nil || out != github.Member+"\n" || *reads != 1 {
		t.Errorf("person = %q, %v after %d reads, want member from a new roster", out, err, *reads)
	}
}

// A token that may not read the membership answers permission missing and
// exits 3, keeps nothing and never says not a member.
func TestPersonPermissionMissing(t *testing.T) {
	personGitHub(t)
	a := personApp(t, "outsidertok")
	out, err := askPerson(t, a, "privatemember")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != ExitRefused || !strings.HasPrefix(out, "permission missing\n") || strings.Contains(out, "not a member") {
		t.Fatalf("person = %q, %v, want permission missing and exit %d", out, err, ExitRefused)
	}
	if _, err := os.Stat(a.rosterPath(testOrg)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a roster was kept: %v", err)
	}
}

// Without a gh of the person's own, person fails rather than asking with
// the agent's.
func TestPersonWithoutAnOwnGH(t *testing.T) {
	personGitHub(t)
	link := fakeGH(t, "persontok")
	t.Setenv("PATH", link)
	cfg := &config.Config{StateDir: t.TempDir()}
	cfg.Agents.Shell.Path = []string{link}
	if _, err := askPerson(t, &app{cfg: cfg}, "privatemember"); err == nil || !strings.Contains(err.Error(), "no gh on PATH outside") {
		t.Fatalf("person = %v, want no gh of the person's own", err)
	}
}

// The broker runs person only, as its session.
func TestBrokeredPersonArgv(t *testing.T) {
	if _, err := brokeredPersonArgv(sandbox.Request{Args: []string{personName, "--org=acme", "--", "x"}}, true); err != nil {
		t.Errorf("person refused: %v", err)
	}
	for _, args := range [][]string{{"secret", "--", "x"}, {personName, "--as=someone", "--", "x"}, {personName, "--config=/c", "--", "x"}} {
		if _, err := brokeredPersonArgv(sandbox.Request{Args: args}, true); err == nil {
			t.Errorf("brokered %v, want a refusal", args)
		}
	}
}
