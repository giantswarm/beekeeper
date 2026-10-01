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
	if err := os.WriteFile(filepath.Join(home, "profile"), []byte("2026-10-01T12:30:00Z"), 0o600); err != nil {
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
	if prev, _ := readProfile(context.Background(), r.Previous()); prev.ValidUntil.Format(time.RFC3339) != "2026-10-01T12:30:00Z" {
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
		"login fails": {fakeTsh{err: errors.New("exit status 1")}, "tsh login: exit status 1"},
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
		if got, _ := readProfile(context.Background(), r.Home); got.ValidUntil.Format(time.RFC3339) != "2026-10-01T12:30:00Z" {
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
