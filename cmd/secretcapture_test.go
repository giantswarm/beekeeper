//go:build unix

package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

const captureOp = "capture"

// fakeProducer is a credential generator that prints a token's JSON, the
// password once, and a line on stderr; with fail it exits 2 instead.
func fakeProducer(t *testing.T, fail bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "generate")
	code := 0
	if fail {
		code = 2
	}
	script := fmt.Sprintf("#!/bin/sh\necho 'generating' >&2\nprintf '{\"passwords\":[{\"value\":\"%s\"}]}\\n'\nexit %d\n", secretValue, code)
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	return p
}

func TestSecretCaptureAnswersAndLogsNoValue(t *testing.T) {
	a, tools, repo := secretApp(t)
	prod := fakeProducer(t, false)
	file := filepath.Join(repo, "pull.sops.yaml")
	out, err := runSecret(a, captureOp, file, "--dockerconfigjson", "registry.example.io", "--username", "pull-token",
		nameFlag, "pull", namespaceFlag, "flux-system", "--vault", "op://Shared/pull/password", "--jq", ".passwords[0].value", "--", prod, "--password1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "generating\nwrote op://Shared/pull/password and "+file+"#stringData..dockerconfigjson: ") ||
		!strings.Contains(out, " bytes as the dockerconfigjson of registry.example.io, hmac:") {
		t.Errorf("capture answers %q", out)
	}
	noSecret(t, "capture", out)
	if tools.Vault["op://Shared/pull/password"] != secretValue {
		t.Error("the vault does not hold the printed value")
	}
	// the whole output into a path, as JSON
	a.out, a.json = &bytes.Buffer{}, true
	out, err = runSecret(a, captureOp, file+"#stringData.raw", "--", prod)
	if err != nil || !strings.Contains(out, `"key": "`+file+`#stringData.raw"`) {
		t.Errorf("capture --json = %q, %v", out, err)
	}
	noSecret(t, "capture --json", out)
	a.json = false
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{captureOp, file + "#stringData.x", "--", fakeProducer(t, true)}, ExitRefused},
		{[]string{captureOp, file + "#stringData.x", "--jq", ".passwords", "--", prod}, ExitRefused},
		{[]string{captureOp, file, "--dockerconfigjson", "registry.example.io", "--", prod}, ExitUsage},
		{[]string{captureOp, file + "#stringData.x", prod}, ExitUsage},
	} {
		a.out = &bytes.Buffer{}
		out, err := runSecret(a, c.args...)
		if Code(err) != c.code {
			t.Errorf("secret %s = %v, want exit %d", strings.Join(c.args, " "), err, c.code)
		}
		noSecret(t, "a refused capture", fmt.Sprint(out, err))
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret.capture" })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Errorf("%d capture events, want 4", len(evs))
	}
	for _, e := range evs {
		noSecret(t, "the log", e.Detail)
		if !strings.Contains(e.Detail, " from "+prod) && !strings.Contains(e.Detail, "/generate") {
			t.Errorf("the log names no producer: %s", e.Detail)
		}
	}
	if !strings.Contains(evs[0].Detail, " from "+prod+" --password1: ") {
		t.Errorf("the log = %s", evs[0].Detail)
	}
}

func TestSecretCaptureHelp(t *testing.T) {
	a, _, _ := secretApp(t)
	out, err := runSecret(a, "--help")
	if err != nil || !strings.Contains(out, "capture") {
		t.Errorf("secret --help = %q, %v", out, err)
	}
	a.out = &bytes.Buffer{}
	out, err = runSecret(a, captureOp, "--help")
	for _, want := range []string{"--jq", "--dockerconfigjson", "--username", "--vault", "--encode", "kubernetes.io/dockerconfigjson", "secret.env"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("capture --help lacks %q: %v", want, err)
		}
	}
}
