package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
	"github.com/giantswarm/beekeeper/internal/state"
)

// TestSecretRevealAndUnsetAsTheDexSplitCallsThem runs the command lines a
// split tool runs with --vault beekeeper --write: reveal --json for the
// configuration, unset --write for the moved keys; a secret is refused by
// its path and no value reaches an answer or the log.
func TestSecretRevealAndUnsetAsTheDexSplitCallsThem(t *testing.T) {
	a, tools, repo := secretApp(t)
	patch := filepath.Join(repo, "secret-values.yaml.patch")
	plain := "oidc:\n  extraStaticClients:\n    - id: kagent\n      secret: " + secretValue +
		"\n      redirectURIs:\n        - https://kagent.example.org/callback\n  staticClients:\n    muster:\n      id: muster\n      secret: " + secretValue + "\n"
	if err := os.WriteFile(patch, secrettest.Encrypt(plain), 0o600); err != nil {
		t.Fatal(err)
	}
	a.json = true
	out, err := runSecret(a, "reveal", patch, "oidc.extraStaticClients.0.id", "oidc.extraStaticClients.0.redirectURIs")
	var fs []secret.Field
	if err != nil || json.Unmarshal([]byte(out), &fs) != nil || len(fs) != 2 || fs[0] != (secret.Field{Path: "oidc.extraStaticClients.0.id", Value: "kagent"}) {
		t.Fatalf("reveal = %q, %v", out, err)
	}
	a.out, a.json = &bytes.Buffer{}, false
	out, err = runSecret(a, "reveal", patch, "oidc.staticClients.muster")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "oidc.staticClients.muster.secret") {
		t.Errorf("reveal of a client with its secret = %q, %v", out, err)
	}
	noSecret(t, "the refusal", out+err.Error())
	a.out = &bytes.Buffer{}
	out, err = runSecret(a, "unset", patch, "oidc.extraStaticClients", "oidc.staticClients.muster.secret")
	if err != nil || !strings.Contains(out, "would remove oidc.extraStaticClients\n") || slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "sops unset") }) {
		t.Errorf("unset's dry run = %q, %v", out, err)
	}
	a.out = &bytes.Buffer{}
	out, err = runSecret(a, "unset", patch, "oidc.extraStaticClients", "oidc.staticClients.muster.secret", "--write")
	if err != nil || !strings.Contains(out, "removed oidc.staticClients.muster.secret\nremoved oidc.extraStaticClients\n") || !strings.Contains(out, "keeps 1 keys\n  oidc.staticClients.muster.id\n") {
		t.Errorf("unset --write = %q, %v", out, err)
	}
	noSecret(t, "unset", out)
	a.out = &bytes.Buffer{}
	if out, err = runSecret(a, "unset", patch+"#oidc", "x"); Code(err) != ExitUsage {
		t.Errorf("unset of a file#path = %q, %v", out, err)
	}
	evs, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "secret.reveal" || e.Verb == "secret.unset" })
	if err != nil || len(evs) != 4 {
		t.Fatalf("%d reveal and unset events, %v", len(evs), err)
	}
	for _, e := range evs {
		noSecret(t, "the log", e.Detail)
	}
}
