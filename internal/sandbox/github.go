package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The GitHub token of a sandboxed session is the devctl App's short-lived
// user token, which the broker holds in its memory alone and the egress
// proxy puts into the Authorization header of requests to GitHub's hosts
// (egress.go). The sandbox gets only what trusts and uses that proxy, in
// the egress directory under the user's runtime directory, which
// sandboxed commands read but never write:
const (
	// EgressCA is the proxy's CA certificate.
	EgressCA = "ca.pem"
	// EgressBundle is the system's roots and the proxy's CA, for the
	// clients that take one file of roots (Go, OpenSSL, curl, git).
	EgressBundle = "bundle.pem"
	// EgressGH is gh's configuration directory: a login whose token is a
	// fixed word, since the proxy sets the header (GHLogin).
	EgressGH = "gh"
)

// GHLogin is gh's login in the sandbox: gh needs one to call GitHub, and
// the proxy replaces the header it sends. It is no secret.
const GHLogin = "beekeeper-egress-proxy"

// systemRoots are the system's root bundles, in Go's order.
var systemRoots = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// EgressDir is the egress directory under the runtime directory, "" without
// one.
func EgressDir(runtimeDir string) string {
	if runtimeDir == "" {
		return ""
	}
	return filepath.Join(runtimeDir, "beekeeper", "egress")
}

// WriteEgress puts the proxy's CA, the bundle of the system's roots and the
// CA, and gh's login into dir, each replaced in one rename.
func WriteEgress(dir string, ca []byte) error {
	var roots []byte
	for _, p := range systemRoots {
		if b, err := os.ReadFile(p); err == nil { //nolint:gosec // the system's root bundle
			roots = b
			break
		}
	}
	if roots == nil {
		return fmt.Errorf("no system root bundle (%s)", systemRoots[0])
	}
	files := map[string][]byte{
		EgressCA:                             ca,
		EgressBundle:                         append(append(bytes.TrimRight(roots, "\n"), '\n'), ca...),
		filepath.Join(EgressGH, "hosts.yml"): []byte("github.com:\n    oauth_token: " + GHLogin + "\n    git_protocol: https\n"),
	}
	if err := os.MkdirAll(filepath.Join(dir, EgressGH), 0o700); err != nil {
		return err
	}
	for name, b := range files {
		path := filepath.Join(dir, name)
		tmp := path + ".new"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// RemoveMaskedGitHub removes the masked token files earlier brokers kept
// in the runtime directory: the token now stays in the broker.
func RemoveMaskedGitHub(runtimeDir string) error {
	if runtimeDir == "" {
		return nil
	}
	dir := filepath.Join(runtimeDir, "beekeeper", "github")
	for _, name := range []string{"hosts.yml", "git-credential"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Token is the GitHub token the proxy injects, shared between its renewal
// and the proxy's requests.
type Token struct {
	mu sync.RWMutex
	v  string
}

// Get is the token, "" while there is none.
func (t *Token) Get() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.v
}

func (t *Token) set(v string) {
	t.mu.Lock()
	t.v = v
	t.mu.Unlock()
}

// KeepGitHub sets tok to what token returns now and every every until ctx
// ends, telling say what happened, never the token. A failed read keeps
// the token it had: a session's GitHub calls fail once it expires, and say
// names why.
func KeepGitHub(ctx context.Context, tok *Token, every time.Duration, token func(context.Context) (string, error), say func(string)) {
	for {
		t, err := token(ctx)
		switch {
		case err != nil:
			say(fmt.Sprintf("github token: not renewed (%v); the egress proxy keeps the token it has until it expires", err))
		case t == "":
			say("github token: devctl answered no token; the egress proxy keeps the token it has")
		case t != tok.Get():
			tok.set(t)
			say("github token: renewed for the egress proxy")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
