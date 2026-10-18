package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDevctlTokenNamesTheCause is devctl answering a token, no login and
// an error of its own, each by a fake devctl.
func TestDevctlTokenNamesTheCause(t *testing.T) {
	for _, c := range []struct{ name, script, token, cause string }{
		{"token", `[ "$*" = "auth exec -- printenv GH_TOKEN" ] && echo ghu_test`, "ghu_test", ""},
		{"no login", "echo 'no GitHub token: run `devctl auth login --github-only`' >&2; exit 8", "", "no devctl login (devctl auth exec: exit status 8: no GitHub token"},
		{"own error", "echo 'keychain: dial unix: no such file' >&2; exit 1", "", "devctl's own error (devctl auth exec: exit status 1: keychain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			devctl := filepath.Join(t.TempDir(), "devctl")
			if err := os.WriteFile(devctl, []byte("#!/bin/sh\n"+c.script+"\n"), 0o700); err != nil { //nolint:gosec // a fake devctl
				t.Fatal(err)
			}
			got, err := devctlToken(context.Background(), devctl)
			if got != c.token {
				t.Errorf("token %q, want %q", got, c.token)
			}
			if c.cause == "" && err != nil || c.cause != "" && (err == nil || !strings.HasPrefix(err.Error(), c.cause)) {
				t.Errorf("error %v, want %q", err, c.cause)
			}
		})
	}
}
