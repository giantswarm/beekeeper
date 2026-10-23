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

// fakeProducer puts an az on PATH that prints a token's JSON, the password
// once, and the password on stderr too (as --debug would); called with
// --fail it exits 2 instead. It answers the allowed argv that runs it.
func fakeProducer(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\necho 'response: %s' >&2\ncase \" $* \" in *' --fail '*) exit 2;; esac\nprintf '{\"passwords\":[{\"value\":\"%s\"}]}\\n'\n", secretValue, secretValue)
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return append(strings.Fields("az acr token credential generate"), nameFlag, "pull-token")
}

func TestSecretCaptureAnswersAndLogsNoValue(t *testing.T) {
	a, tools, repo := secretApp(t)
	prod := fakeProducer(t)
	producing := func(args ...string) []string { return append(append(args, "--"), prod...) }
	file := filepath.Join(repo, "pull.sops.yaml")
	out, err := runSecret(a, append(producing(captureOp, file, dockerFlag, testRegistry, usernameFlag, "pull-token",
		nameFlag, "pull", namespaceFlag, "flux-system", vaultFlag, "op://Shared/pull/password", "--jq", ".passwords[0].value"), "--password1")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "wrote op://Shared/pull/password and "+file+"#stringData..dockerconfigjson: ") ||
		!strings.Contains(out, " bytes as the dockerconfigjson of registry.example.io, hmac:") {
		t.Errorf("capture answers %q", out)
	}
	noSecret(t, "capture", out)
	if tools.Vault["op://Shared/pull/password"] != secretValue {
		t.Error("the vault does not hold the printed value")
	}
	// the whole output into a path, as JSON
	a.out, a.json = &bytes.Buffer{}, true
	out, err = runSecret(a, producing(captureOp, file+"#stringData.raw")...)
	if err != nil || !strings.Contains(out, `"key": "`+file+`#stringData.raw"`) {
		t.Errorf("capture --json = %q, %v", out, err)
	}
	noSecret(t, "capture --json", out)
	a.json = false
	for _, c := range []struct {
		args []string
		code int
	}{
		{append(producing(captureOp, file+"#stringData.x"), "--fail"), ExitRefused},
		{producing(captureOp, file+"#stringData.x", "--jq", ".passwords"), ExitRefused},
		{[]string{captureOp, file + "#stringData.x", "--", "sh", "-c", strings.Join(prod, " ")}, ExitRefused},
		{append(producing(captureOp, file+"#stringData.x"), "--debug"), ExitRefused},
		{producing(captureOp, file, dockerFlag, testRegistry), ExitUsage},
		{append([]string{captureOp, file + "#stringData.x"}, prod...), ExitUsage},
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
	if len(evs) != 6 {
		t.Errorf("%d capture events, want 6", len(evs))
	}
	for _, e := range evs {
		noSecret(t, "the log", e.Detail)
		if !strings.Contains(e.Detail, " from "+prod[0]) && !strings.Contains(e.Detail, " from sh -c") {
			t.Errorf("the log names no producer: %s", e.Detail)
		}
	}
	if !strings.Contains(evs[0].Detail, " from "+strings.Join(prod, " ")+" --password1: ") {
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
	for _, want := range []string{"--jq", dockerFlag, usernameFlag, vaultFlag, "--encode", "kubernetes.io/dockerconfigjson", "secret.env", "openssl genpkey", "stderr discarded"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("capture --help lacks %q: %v", want, err)
		}
	}
}
