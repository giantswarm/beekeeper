package teleport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// status is tsh status --format=json as tsh 18 prints it, trimmed.
const status = `{
  "active": {
    "profile_url": "https://login.example.com:443",
    "username": "ada",
    "cluster": "login.example.com",
    "roles": ["dev"],
    "valid_until": "2026-10-01T22:38:37+02:00",
    "kubernetes_enabled": true
  },
  "profiles": []
}`

func TestParseStatus(t *testing.T) {
	p, err := ParseStatus([]byte(status))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 1, 20, 38, 37, 0, time.UTC)
	if p.Username != "ada" || p.Cluster != "login.example.com" || !p.ValidUntil.Equal(want) {
		t.Fatalf("got %+v", p)
	}
	if left := p.Left(want.Add(-time.Hour)); left != time.Hour {
		t.Fatalf("left %s", left)
	}
	for _, raw := range []string{`{"active": null, "profiles": []}`, `{}`, `{"active": {"cluster": "x"}}`} {
		if _, err := ParseStatus([]byte(raw)); !errors.Is(err, ErrNotLoggedIn) {
			t.Errorf("%s: %v, want not logged in", raw, err)
		}
	}
	if _, err := ParseStatus([]byte("ERROR: Not logged in.")); err == nil || errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("garbage: %v", err)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p := Profile{ValidUntil: now.Add(2 * time.Hour)}
	failed := Record{FailedAt: now.Add(-time.Minute), FailedFor: p.ValidUntil, Reason: "timed out"}
	cases := []struct {
		name   string
		p      Profile
		perr   error
		r      Record
		due    bool
		reason string
	}{
		{"valid", p, nil, Record{}, false, "valid until"},
		{"under the margin", Profile{ValidUntil: now.Add(80 * time.Minute)}, nil, Record{}, true, ""},
		{"past its expiry", Profile{ValidUntil: now.Add(-time.Hour)}, nil, Record{}, true, ""},
		{"not logged in", Profile{}, ErrNotLoggedIn, Record{}, true, ""},
		{"failed for this profile", Profile{ValidUntil: p.ValidUntil}, nil, failed, false, "failed (timed out)"},
		{"failed for an older profile", Profile{ValidUntil: now.Add(time.Hour)}, nil, failed, true, ""},
		{"failed while logged out", Profile{}, ErrNotLoggedIn, Record{FailedAt: now}, false, "failed"},
		{"unreadable", Profile{}, errors.New("tsh status: boom"), Record{}, false, "boom"},
	}
	for _, c := range cases {
		due, reason := Due(c.p, c.perr, c.r, now, 90*time.Minute)
		if due != c.due || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: due %v %q, want %v %q", c.name, due, reason, c.due, c.reason)
		}
	}
}

// fakeTsh is a login that writes a profile file whose content is its
// expiry, and the status that reads it back.
type fakeTsh struct {
	until time.Time
	err   error
	hang  bool
}

func (f fakeTsh) login(ctx context.Context, home string, log *os.File) error {
	_, _ = log.WriteString("https://login.example.com/web/login?secret=one-time\n")
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.err != nil {
		return f.err
	}
	if err := os.WriteFile(filepath.Join(home, kubeconfig), []byte("contexts"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "profile"), []byte(f.until.Format(time.RFC3339)), 0o600)
}

// homeUntil is the expiry of the profile renewal's home starts with.
const homeUntil = "2026-10-01T12:30:00Z"

// loginFailed is the error of a login whose tsh exited 1.
const loginFailed = "tsh login: exit status 1"

func readProfile(_ context.Context, home string) (Profile, error) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(home, "profile")))
	if err != nil {
		return Profile{}, ErrNotLoggedIn
	}
	t, err := time.Parse(time.RFC3339, string(raw))
	return Profile{ValidUntil: t}, err
}

func renewal(t *testing.T, f fakeTsh) Renewal {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "tsh")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "profile"), []byte(homeUntil), 0o600); err != nil {
		t.Fatal(err)
	}
	return Renewal{Home: home, Dir: filepath.Join(dir, "state", "teleport"), Timeout: time.Second, Login: f.login, Status: readProfile}
}

func TestRenewSwapsTheStagingHomeIn(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := renewal(t, fakeTsh{until: now.Add(12 * time.Hour)})
	p, err := r.Renew(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !p.ValidUntil.Equal(now.Add(12 * time.Hour)) {
		t.Fatalf("renewed until %s", p.ValidUntil)
	}
	if got, _ := readProfile(context.Background(), r.Home); !got.ValidUntil.Equal(p.ValidUntil) {
		t.Fatalf("home holds %s", got.ValidUntil)
	}
	if prev, _ := readProfile(context.Background(), r.Previous()); prev.ValidUntil.Format(time.RFC3339) != homeUntil {
		t.Fatalf("previous holds %s", prev.ValidUntil)
	}
	if _, err := os.Stat(filepath.Join(r.Home, kubeconfig)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the login's kubeconfig moved into the home: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(r.Dir, "staging-*")); len(left) > 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	// A second renewal replaces the previous profile.
	if _, err := r.Renew(context.Background(), now.Add(11*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if prev, _ := readProfile(context.Background(), r.Previous()); !prev.ValidUntil.Equal(p.ValidUntil) {
		t.Fatalf("previous holds %s after the second renewal", prev.ValidUntil)
	}
}

func TestRenewWithoutAHome(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := renewal(t, fakeTsh{until: now.Add(12 * time.Hour)})
	if err := os.RemoveAll(r.Home); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Renew(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if got, _ := readProfile(context.Background(), r.Home); got.ValidUntil.IsZero() {
		t.Fatal("no profile in the home")
	}
}

func TestRenewFailureLeavesTheHome(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		f    fakeTsh
		want string
	}{
		"login fails": {fakeTsh{err: errors.New("exit status 1")}, loginFailed},
		"timeout":     {fakeTsh{hang: true}, "did not complete within 1s"},
		"expired":     {fakeTsh{until: now.Add(-time.Minute)}, "expired"},
	}
	for name, c := range cases {
		r := renewal(t, c.f)
		_, err := r.Renew(context.Background(), now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if strings.Contains(err.Error(), "secret=") {
			t.Errorf("%s: the login URL reached the error: %v", name, err)
		}
		if got, _ := readProfile(context.Background(), r.Home); got.ValidUntil.Format(time.RFC3339) != homeUntil {
			t.Errorf("%s: home changed to %s", name, got.ValidUntil)
		}
		if left, _ := filepath.Glob(filepath.Join(r.Dir, "staging-*")); len(left) > 0 {
			t.Errorf("%s: staging left behind: %v", name, left)
		}
		if fi, err := os.Stat(r.Log()); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: log %v %v", name, fi, err)
		}
	}
}

// callbackTimeoutOutput is tsh login's output when the proxy's SSO callback
// exchange times out after the browser's sign-in.
const callbackTimeoutOutput = "ERROR: identity provider callback failed: Get \"https://proxy.example.com/v1/webapi/github/callback\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)\n"

// attempts is a login that plays one fakeTsh per attempt, the timeouts
// among them writing callbackTimeoutOutput.
type attempts struct {
	logins  []fakeTsh
	timeout []bool
	n       *int
}

func (a attempts) login(ctx context.Context, home string, log *os.File) error {
	i := *a.n
	*a.n++
	if a.timeout[i] {
		_, _ = log.WriteString(callbackTimeoutOutput)
	}
	return a.logins[i].login(ctx, home, log)
}

func TestRenewRetriesTheCallbackTimeoutOnce(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	failed := fakeTsh{err: errors.New("exit status 1")}
	ok := fakeTsh{until: now.Add(12 * time.Hour)}
	cases := map[string]struct {
		logins   []fakeTsh
		timeout  []bool
		want     string // the error, "" for a renewal
		attempts int
		retried  bool
	}{
		"retry succeeds":      {[]fakeTsh{failed, ok}, []bool{true, false}, "", 2, true},
		"retry fails":         {[]fakeTsh{failed, failed}, []bool{true, false}, loginFailed, 2, true},
		"retry times out too": {[]fakeTsh{failed, failed, ok}, []bool{true, true, false}, "SSO callback timed out", 2, true},
		"other failure":       {[]fakeTsh{failed, ok}, []bool{false, false}, loginFailed, 1, false},
		"login times out":     {[]fakeTsh{{hang: true}, ok}, []bool{false, false}, "did not complete within 1s", 1, false},
	}
	for name, c := range cases {
		n := 0
		r := renewal(t, fakeTsh{})
		r.Login = attempts{logins: c.logins, timeout: c.timeout, n: &n}.login
		var retried error
		r.Retry = func(first error) { retried = first }
		p, err := r.Renew(context.Background(), now)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if n != c.attempts {
			t.Errorf("%s: %d logins, want %d", name, n, c.attempts)
		}
		if c.retried != (retried != nil) || retried != nil && !errors.Is(retried, ErrCallbackTimeout) {
			t.Errorf("%s: Retry heard %v", name, retried)
		}
		home, _ := readProfile(context.Background(), r.Home)
		if c.want == "" && !home.ValidUntil.Equal(p.ValidUntil) || c.want != "" && home.ValidUntil.Format(time.RFC3339) != homeUntil {
			t.Errorf("%s: home holds %s", name, home.ValidUntil)
		}
		raw, _ := os.ReadFile(r.Log())
		if got := strings.Count(string(raw), "secret=one-time"); got != c.attempts {
			t.Errorf("%s: the log holds %d logins, want %d", name, got, c.attempts)
		}
	}
}
