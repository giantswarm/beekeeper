// Package teleport keeps the desk's Teleport login: it reads the active
// profile's expiry from tsh and renews the login in a staging home, which
// replaces the profile directory in one exchange once the new login has
// succeeded. It reads metadata only (cluster, user, expiry), never the
// certificates or keys, and keeps tsh's output, which carries the one-time
// login URL, in a log file instead of returning it.
package teleport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ErrNotLoggedIn is a profile directory without an active profile.
var ErrNotLoggedIn = errors.New("not logged in")

// Profile is the active profile's metadata, as tsh status prints it.
type Profile struct {
	Cluster    string    `json:"cluster"`
	Username   string    `json:"username"`
	ValidUntil time.Time `json:"valid_until"`
}

// Left is how long the profile is valid from now, negative once it expired.
func (p Profile) Left(now time.Time) time.Duration { return p.ValidUntil.Sub(now) }

// ParseStatus reads the active profile from `tsh status --format=json`.
func ParseStatus(raw []byte) (Profile, error) {
	var s struct {
		Active *Profile `json:"active"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return Profile{}, fmt.Errorf("tsh status: %w", err)
	}
	if s.Active == nil || s.Active.ValidUntil.IsZero() {
		return Profile{}, ErrNotLoggedIn
	}
	return *s.Active, nil
}

// Status reads the active profile of the profile directory home with tsh.
func Status(ctx context.Context, tsh, home string) (Profile, error) {
	c := exec.CommandContext(ctx, tsh, "status", "--format=json") //nolint:gosec // the configured tsh binary
	c.Env = append(os.Environ(), "TELEPORT_HOME="+home)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if p, perr := ParseStatus(out); perr == nil {
		return p, nil
	}
	if bytes.Contains(stderr.Bytes(), []byte("Not logged in")) {
		return Profile{}, ErrNotLoggedIn
	}
	if err == nil {
		_, err = ParseStatus(out)
	}
	return Profile{}, fmt.Errorf("tsh status: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
}

// Renewal is one renewal of the login of the profile directory Home.
type Renewal struct {
	// Tsh is the tsh binary.
	Tsh string
	// Home is the profile directory the kube contexts' tsh reads (~/.tsh).
	Home string
	// Dir holds the staging home, the previous profile and the login log.
	Dir string
	// Proxy and Auth are tsh login's --proxy and --auth; an empty Auth is
	// the cluster's default connector.
	Proxy, Auth string
	// Timeout bounds the login, the browser's round trip included.
	Timeout time.Duration
	// Login runs the login into the staging home, its output into log; nil
	// is tsh login.
	Login func(ctx context.Context, home string, log *os.File) error
	// Status reads a profile directory; nil is Status with Tsh.
	Status func(ctx context.Context, home string) (Profile, error)
}

// Log is the file the last login's output went to: it carries the one-time
// login URL, so it is never printed.
func (r Renewal) Log() string { return filepath.Join(r.Dir, "login.log") }

// Previous is where the profile before the last renewal is kept.
func (r Renewal) Previous() string { return filepath.Join(r.Dir, "previous") }

// Renew logs in into an empty staging home (a valid profile makes tsh login
// print the status only) and, once that login holds a profile valid past
// now, exchanges the staging home with Home in one step: the old
// certificate serves until the new one is in place. The profile before is
// kept as Previous. On any failure Home stays as it was.
func (r Renewal) Renew(ctx context.Context, now time.Time) (Profile, error) {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return Profile{}, err
	}
	staging, err := os.MkdirTemp(r.Dir, "staging-")
	if err != nil {
		return Profile{}, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := r.login(ctx, staging); err != nil {
		return Profile{}, err
	}
	p, err := r.status(ctx, staging)
	if err != nil {
		return Profile{}, fmt.Errorf("the new login: %w", err)
	}
	if p.Left(now) <= 0 {
		return Profile{}, fmt.Errorf("the new login expired at %s already", p.ValidUntil.Format(time.RFC3339))
	}
	// The login's kubeconfig stays out of the profile: the person's
	// kubeconfig keeps its contexts, and they read Home.
	_ = os.Remove(filepath.Join(staging, kubeconfig))
	if err := r.swap(staging); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// kubeconfig is the file in the staging home tsh login writes its kube
// contexts to instead of the person's kubeconfig.
const kubeconfig = "kubeconfig"

func (r Renewal) login(ctx context.Context, staging string) error {
	log, err := os.OpenFile(r.Log(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	login := r.Login
	if login == nil {
		login = r.tshLogin
	}
	err = login(ctx, staging, log)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("tsh login did not complete within %s (the browser's sign-in may need a person; the log is %s)", r.Timeout, r.Log())
	case err != nil:
		return fmt.Errorf("tsh login: %w (the log is %s)", err, r.Log())
	}
	return nil
}

// tshLogin runs tsh login into home: tsh opens the login URL in the default
// browser and waits on its localhost callback.
func (r Renewal) tshLogin(ctx context.Context, home string, log *os.File) error {
	args := []string{"login", "--proxy=" + r.Proxy}
	if r.Auth != "" {
		args = append(args, "--auth="+r.Auth)
	}
	c := exec.CommandContext(ctx, r.Tsh, args...) //nolint:gosec // the configured tsh binary and proxy
	c.Env = append(os.Environ(), "TELEPORT_HOME="+home, "KUBECONFIG="+filepath.Join(home, kubeconfig))
	c.Stdout, c.Stderr = log, log
	c.WaitDelay = 5 * time.Second
	return c.Run()
}

func (r Renewal) status(ctx context.Context, home string) (Profile, error) {
	if r.Status != nil {
		return r.Status(ctx, home)
	}
	return Status(ctx, r.Tsh, home)
}

// swap puts staging in Home's place and keeps what was there as Previous.
func (r Renewal) swap(staging string) error {
	if _, err := os.Lstat(r.Home); errors.Is(err, os.ErrNotExist) {
		return os.Rename(staging, r.Home)
	}
	if err := exchange(staging, r.Home); err != nil {
		return fmt.Errorf("swap %s into %s: %w", staging, r.Home, err)
	}
	// staging holds the previous profile now; its certificate expires
	// within hours, the one it replaces expired long ago.
	if err := os.RemoveAll(r.Previous()); err != nil {
		return err
	}
	return os.Rename(staging, r.Previous())
}
