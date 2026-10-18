package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	// EgressGPG is git's gpg.program in the sandbox: it runs beekeeper
	// sandbox gpg, which has the broker sign with the person's key, out of
	// the sandbox's reach.
	EgressGPG = "gpg"
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
// CA, gh's login and git's gpg.program, which runs exe, into dir, each
// replaced in one rename.
func WriteEgress(dir string, ca []byte, exe string) error {
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
		EgressGPG:                            GPGProgram(exe),
	}
	if err := os.MkdirAll(filepath.Join(dir, EgressGH), 0o700); err != nil {
		return err
	}
	for name, b := range files {
		path := filepath.Join(dir, name)
		tmp := path + ".new"
		mode := os.FileMode(0o600)
		if name == EgressGPG {
			mode = 0o700
		}
		if err := os.WriteFile(tmp, b, mode); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// GPGProgram is git's gpg.program in the sandbox: git runs it by its path
// alone, so it is a script that runs exe's sandbox gpg.
func GPGProgram(exe string) []byte {
	return []byte("#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", `'\''`) + "' sandbox gpg \"$@\"\n")
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

// GitHubRenewal is how KeepGitHub reads the token: every Every while
// reads succeed, a failed one tried again after Backoff's waits before the
// next tick. A read is cut at Bound while the proxy holds a token, at
// ColdBound while it holds none: a broker started at boot reads first while
// devctl may still wait on what the boot has not brought up (the keyring
// on the session bus, the network), and five minutes without a token is
// every sandboxed gh call failing.
type GitHubRenewal struct {
	Every, Bound, ColdBound time.Duration
	// Backoff is the wait before the retry after each failed read in a
	// row; once spent, the next read is at Every.
	Backoff []time.Duration
}

// KeepGitHub sets tok to what token returns now and then as r says until
// ctx ends, telling say what happened, never the token. A failed read
// keeps the token it had, and say names its cause: the bound that cut it,
// the token func's error, or no token in devctl's answer.
func KeepGitHub(ctx context.Context, tok *Token, r GitHubRenewal, token func(context.Context) (string, error), say func(string)) {
	fails := 0
	for {
		held := tok.Get() != ""
		bound := r.ColdBound
		if held {
			bound = r.Bound
		}
		tctx, cancel := context.WithTimeout(ctx, bound)
		t, err := token(tctx)
		cut := errors.Is(tctx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return
		}
		var cause string
		switch {
		case cut:
			cause = fmt.Sprintf("killed at its %s bound (%v)", bound, err)
		case err != nil:
			cause = err.Error()
		case t == "":
			cause = "devctl answered no token"
		}
		wait := r.Every
		if cause == "" {
			fails = 0
			if t != tok.Get() {
				tok.set(t)
				say("github token: renewed for the egress proxy")
			}
		} else {
			fails++
			wait = r.retryAfter(fails)
			keeps := "the egress proxy has no token yet"
			if held {
				keeps = "the egress proxy keeps the token it has until it expires"
			}
			say(fmt.Sprintf("github token: not renewed: %s; try %d at %s; %s",
				cause, fails+1, time.Now().Add(wait).Local().Format(time.TimeOnly), keeps))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// retryAfter is the wait before the read after fails failed ones in a row.
func (r GitHubRenewal) retryAfter(fails int) time.Duration {
	if fails < 1 || fails > len(r.Backoff) {
		return r.Every
	}
	return min(r.Backoff[fails-1], r.Every)
}
