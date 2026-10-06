package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// Only git's signing call passes: the flags in git's order and a key that
// is no flag and carries no control character.
func TestGPGSignKey(t *testing.T) {
	for _, tc := range []struct {
		args []string
		key  string
	}{
		{[]string{"--status-fd=2", "-bsau", "ABCDEF0123456789"}, "ABCDEF0123456789"},
		{[]string{"--status-fd=2", "-bsau", "Timo <timo@example.com>"}, "Timo <timo@example.com>"},
		{[]string{"--status-fd=2", "-bsau", "--export-secret-keys"}, ""},
		{[]string{"--status-fd=2", "-bsau", "a\nb"}, ""},
		{[]string{"--status-fd=2", "-bsau", ""}, ""},
		{[]string{"--keyid-format=long", "--status-fd=1", "--verify", "/tmp/sig", "-"}, ""},
		{[]string{"--status-fd=2", "-bsau", "KEY", "--armor"}, ""},
		{[]string{"-bsau", "--status-fd=2", "KEY"}, ""},
	} {
		key, ok := gpgSignKey(tc.args)
		if key != tc.key || ok != (tc.key != "") {
			t.Errorf("gpgSignKey(%q) = %q, %v; want %q", tc.args, key, ok, tc.key)
		}
	}
}

// The broker runs gpg with git's signing call, the payload on its standard
// input, and answers its output and exit code; any other call is refused
// before gpg runs.
func TestBrokeredSign(t *testing.T) {
	dir := t.TempDir()
	gpg := filepath.Join(dir, "gpg")
	script := "#!/bin/sh\necho \"$*\" >\"" + dir + "/args\"\necho '[GNUPG:] SIG_CREATED D' >&2\nprintf 'SIG:'; cat\n"
	if err := os.WriteFile(gpg, []byte(script), 0o700); err != nil { //nolint:gosec // the test's fake gpg
		t.Fatal(err)
	}
	sign := brokeredSign(gpg)
	r, err := sign(context.Background(), 0, sandbox.Request{Op: sandbox.OpSign, Args: []string{"--status-fd=2", "-bsau", "KEY"}, Input: []byte("tree x\n\xff")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Out != "SIG:tree x\n\xff" || !strings.Contains(r.Err, "SIG_CREATED") || r.Code != 0 {
		t.Errorf("reply = %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "args")); string(b) != "--status-fd=2 -bsau KEY\n" { //nolint:gosec // the test's own file
		t.Errorf("gpg ran with %q", b)
	}
	if _, err := sign(context.Background(), 0, sandbox.Request{Op: sandbox.OpSign, Args: []string{"--export-secret-keys"}}); err == nil {
		t.Error("an export: signed")
	}
}
