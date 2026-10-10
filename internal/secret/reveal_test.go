package secret_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// The dex-app values patch the split starts from: configuration (ids,
// names, redirect URIs, peers) encrypted next to the clients' secrets, all
// generated dummies.
const (
	clientSecret = "planted-Client-9f2e61d0b7"
	peerSecret   = "planted-Peer-0c4a7e2913" //nolint:gosec // a planted test value
	keyLike      = "q8Zt4WmN2xVb7KpR5sLd"
	dexPatch     = `oidc:
  staticClients:
    muster:
      id: muster
      secret: ` + peerSecret + `
  extraStaticClients:
    - id: kagent
      name: Kagent
      public: false
      secret: ` + clientSecret + `
      redirectURIs:
        - https://kagent.example.org/callback
    - id: ` + keyLike + `
      secret: ` + clientSecret + `
    - id: ` + peerSecret + `
  authenticator:
    trustedPeers:
      - kagent
      - muster
issuer: https://dex.example.org
`
)

// The paths of the extra clients and of the first one's secret.
const (
	extraClients = "oidc.extraStaticClients"
	extraSecret  = extraClients + ".0.secret"
	kagentID     = extraClients + ".0.id"
	musterID     = "oidc.staticClients.muster.id"
	musterSecret = "oidc.staticClients.muster.secret" //nolint:gosec // a key path, no value
	peer0        = "oidc.authenticator.trustedPeers.0"
	peer1        = "oidc.authenticator.trustedPeers.1"
	underSecret  = `under the key "secret"` //nolint:gosec // a refusal's reason, no value
)

// dexScratch is a repository holding the encrypted values patch.
func dexScratch(t *testing.T) (dir, patch string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patch = filepath.Join(dir, "secret-values.yaml.patch")
	if err := os.WriteFile(patch, secrettest.Encrypt(dexPatch), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, patch
}

// noDexSecret fails when a client's secret is in what an operation answered.
func noDexSecret(t *testing.T, what string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	s := fmt.Sprintf("%s %+v", raw, v)
	for _, p := range []string{clientSecret, peerSecret} {
		if strings.Contains(s, p) {
			t.Errorf("%s answers a secret: %s", what, s)
		}
	}
}

func TestRevealConfiguration(t *testing.T) {
	tools := secrettest.New(nil)
	_, patch := dexScratch(t)
	fs, err := ops(tools).Reveal(context.Background(), patch, []string{
		kagentID, "oidc.extraStaticClients.0.redirectURIs", "oidc.extraStaticClients.0.public",
		"oidc.authenticator.trustedPeers", musterID, "oidc.extraStaticClients.1.id",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []secret.Field{
		{Path: kagentID, Value: "kagent"},
		{Path: "oidc.extraStaticClients.0.redirectURIs.0", Value: "https://kagent.example.org/callback"},
		{Path: "oidc.extraStaticClients.0.public", Value: "false"},
		{Path: peer0, Value: "kagent"},
		{Path: peer1, Value: "muster"},
		{Path: musterID, Value: "muster"},
		{Path: "oidc.extraStaticClients.1.id", Value: keyLike},
	}
	if !slices.Equal(fs, want) {
		t.Errorf("reveal = %+v", fs)
	}
	if !slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "sops decrypt --input-type yaml --output-type yaml") }) {
		t.Errorf("a values patch is decrypted as YAML: %v", tools.Calls)
	}
}

func TestRevealRefusesSecrets(t *testing.T) {
	_, patch := dexScratch(t)
	for _, tc := range []struct{ path, why string }{
		{"oidc.extraStaticClients.0", underSecret},
		{extraSecret, underSecret},
		{"oidc.extraStaticClients.1.secret", underSecret},
		{"oidc.extraStaticClients.2.id", "an id equal to the secret at " + musterSecret},
		{"oidc", underSecret},
	} {
		fs, err := ops(secrettest.New(nil)).Reveal(context.Background(), patch, []string{"issuer", tc.path})
		if !errors.Is(err, secret.ErrSecretLike) || !strings.Contains(err.Error(), tc.why) || fs != nil {
			t.Errorf("%s: %v, %+v", tc.path, err, fs)
		}
		noDexSecret(t, "reveal "+tc.path, err)
	}
	if _, err := ops(secrettest.New(nil)).Reveal(context.Background(), patch, []string{"oidc.nothing"}); err == nil || !strings.Contains(err.Error(), "no value at oidc.nothing") {
		t.Errorf("an absent path: %v", err)
	}
}

func TestRevealClientIDs(t *testing.T) {
	f := filepath.Join(t.TempDir(), "ids.sops.yaml")
	doc := `clients:
  - id: ` + keyLike + `
    clientID: 0123456789abcdef0123456789abcdef
    name: ` + keyLike + `
    secret: ` + clientSecret + `
  - client_id: ` + clientSecret + `
authenticator:
  trustedPeers: [` + keyLike + `, web-ui]
  peers: [ghp_` + strings.Repeat("aB3dE5fG7h", 3) + `123456]
privateKey:
  id: some-key
`
	if err := os.WriteFile(f, secrettest.Encrypt(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := ops(secrettest.New(nil)).Reveal(context.Background(), f, []string{"clients.0.id", "clients.0.clientID", "authenticator.trustedPeers"})
	want := []secret.Field{
		{Path: "clients.0.id", Value: keyLike},
		{Path: "clients.0.clientID", Value: "0123456789abcdef0123456789abcdef"},
		{Path: "authenticator.trustedPeers.0", Value: keyLike},
		{Path: "authenticator.trustedPeers.1", Value: "web-ui"},
	}
	if err != nil || !slices.Equal(fs, want) {
		t.Errorf("client ids = %+v, %v", fs, err)
	}
	for path, why := range map[string]string{
		"clients.0.name":      "a key's entropy",
		"clients.1.client_id": "an id equal to the secret at clients.0.secret",
		"authenticator.peers": "the value scanner matches it",
		"privateKey.id":       `under the key "privateKey"`,
	} {
		fs, err := ops(secrettest.New(nil)).Reveal(context.Background(), f, []string{path})
		if !errors.Is(err, secret.ErrSecretLike) || !strings.Contains(err.Error(), why) || fs != nil {
			t.Errorf("%s: %v, %+v", path, err, fs)
		}
		noDexSecret(t, "reveal "+path, err)
	}
}

func TestSecretLikeValues(t *testing.T) {
	for v, secretish := range map[string]bool{
		"dex-k8s-authenticator":                             false,
		"https://login.g8s.example.io/oauth2/callback":      false,
		"3f2a9c1e-0b7d-4e55-9a10-6c2f8e4d1b3a":              false,
		"Kagent Web":                                        false,
		"0123456789abcdef0123456789abcdef":                  true,
		"https://user:" + clientSecret + "@example.org/cb":  true,
		"ghp_" + strings.Repeat("aB3dE5fG7h", 3) + "123456": true,
		"q8Zt4WmN2xVb7KpR5sLd":                              true,
	} {
		_, err := ops(secrettest.New(nil)).Reveal(context.Background(), writeOne(t, v), []string{valueKey})
		if got := errors.Is(err, secret.ErrSecretLike); got != secretish {
			t.Errorf("%q: secret-like %v, want %v (%v)", v, got, secretish, err)
		}
	}
}

// writeOne is a fake SOPS file holding v under the key "value".
func writeOne(t *testing.T, v string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "one.sops.yaml")
	if err := os.WriteFile(f, secrettest.Encrypt("value: '"+v+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRevealTo(t *testing.T) {
	ctx := context.Background()
	tools := secrettest.New(nil)
	dir, patch := dexScratch(t)
	plain := filepath.Join(dir, "configmap-values.yaml.patch")
	if err := os.WriteFile(plain, []byte("# kept\nimage:\n  tag: 1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := secret.Ref{File: plain, Path: "oidc.authenticator.trustedPeers"}
	res, err := ops(tools).RevealTo(ctx, patch, "oidc.authenticator.trustedPeers", dst, false)
	if err != nil || res.Written || res.Unchanged || len(res.Paths) != 2 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if raw, _ := os.ReadFile(plain); strings.Contains(string(raw), "trustedPeers") { //nolint:gosec // the test's file
		t.Fatal("the dry run wrote")
	}
	if res, err = ops(tools).RevealTo(ctx, patch, "oidc.authenticator.trustedPeers", dst, true); err != nil || !res.Written {
		t.Fatalf("write = %+v, %v", res, err)
	}
	raw, _ := os.ReadFile(plain) //nolint:gosec // the test's file
	for _, w := range []string{"# kept", "tag: 1.2.3", "trustedPeers:\n      - kagent\n      - muster"} {
		if !strings.Contains(string(raw), w) {
			t.Errorf("the plaintext file lacks %q:\n%s", w, raw)
		}
	}
	if res, err = ops(tools).RevealTo(ctx, patch, "oidc.authenticator.trustedPeers", dst, true); err != nil || !res.Unchanged || res.Written {
		t.Errorf("a second run = %+v, %v", res, err)
	}
	if _, err := ops(tools).RevealTo(ctx, patch, "oidc.extraStaticClients.0", dst, true); !errors.Is(err, secret.ErrSecretLike) {
		t.Errorf("a client with its secret: %v", err)
	}
	if _, err := ops(tools).RevealTo(ctx, patch, "issuer", secret.Ref{File: patch, Path: "x"}, true); err == nil {
		t.Error("the SOPS file itself as the destination")
	}
	other := filepath.Join(dir, "other.sops.yaml")
	_ = os.WriteFile(other, secrettest.Encrypt("a: b\n"), 0o600)
	if _, err := ops(tools).RevealTo(ctx, patch, "issuer", secret.Ref{File: other, Path: "x"}, true); err == nil || !strings.Contains(err.Error(), "is a SOPS file") {
		t.Errorf("a SOPS destination: %v", err)
	}
	noDexSecret(t, "the plaintext file", string(raw))
}

func TestUnset(t *testing.T) {
	ctx := context.Background()
	tools := secrettest.New(nil)
	_, patch := dexScratch(t)
	paths := []string{extraClients, musterSecret, extraSecret}
	res, err := ops(tools).Unset(ctx, patch, paths, false)
	if err != nil || res.Written || !slices.Equal(res.Removed, []string{musterSecret, extraClients}) {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if strings.Contains(strings.Join(tools.Calls, "\n"), "sops unset") {
		t.Fatal("the dry run unset")
	}
	if res, err = ops(tools).Unset(ctx, patch, paths, true); err != nil || !res.Written {
		t.Fatalf("write = %+v, %v", res, err)
	}
	wantKept := []string{"issuer", peer0, peer1, musterID}
	if !slices.Equal(res.Kept, wantKept) {
		t.Errorf("kept = %v", res.Kept)
	}
	noDexSecret(t, "unset", res)
	if got := decrypted(t, tools, patch); strings.Contains(got, "extraStaticClients") || strings.Contains(got, peerSecret) || !strings.Contains(got, "id: muster") {
		t.Errorf("after unset:\n%s", got)
	}
	unsets := slices.DeleteFunc(slices.Clone(tools.Calls), func(c string) bool { return !strings.HasPrefix(c, "sops unset") })
	want := []string{
		`sops unset --input-type yaml --output-type yaml ` + patch + ` ["oidc"]["staticClients"]["muster"]["secret"]`,
		`sops unset --input-type yaml --output-type yaml ` + patch + ` ["oidc"]["extraStaticClients"]`,
	}
	if !slices.Equal(unsets, want) {
		t.Errorf("unset calls %q", unsets)
	}
	n := len(tools.Calls)
	if res, err = ops(tools).Unset(ctx, patch, paths, true); err != nil || res.Written || len(res.Removed) != 0 || len(res.Absent) != 2 {
		t.Errorf("a second run = %+v, %v", res, err)
	}
	if slices.ContainsFunc(tools.Calls[n:], func(c string) bool { return strings.HasPrefix(c, "sops unset") }) {
		t.Error("a second run unset again")
	}
	if _, err := ops(tools).Unset(ctx, patch, []string{"sops.mac"}, true); err == nil {
		t.Error("unset of sops' metadata")
	}
}

func TestUnsetListItems(t *testing.T) {
	tools := secrettest.New(nil)
	_, patch := dexScratch(t)
	if _, err := ops(tools).Unset(context.Background(), patch, []string{peer0, peer1}, true); err != nil {
		t.Fatal(err)
	}
	if got := decrypted(t, tools, patch); strings.Contains(got, "- kagent") || strings.Contains(got, "- muster") {
		t.Errorf("both items stay:\n%s", got)
	}
}

// TestDexSplitFlow is the dex split with --vault beekeeper --write as the
// tool runs it: the configuration revealed, each secret copied into a
// Secret file of its own, the moved keys unset from the patch.
func TestDexSplitFlow(t *testing.T) {
	ctx := context.Background()
	tools := secrettest.New(nil)
	dir, patch := dexScratch(t)
	o := ops(tools)
	fs, err := o.Reveal(ctx, patch, []string{kagentID, musterID})
	if err != nil {
		t.Fatal(err)
	}
	for i, from := range []string{extraSecret, musterSecret} {
		dst := filepath.Join(dir, "dex-client-"+fs[i].Value+"-secret.yaml")
		pairs := []secret.Pair{{Src: secret.Ref{File: patch, Path: from}, Path: secretWord}}
		if _, err := o.CopyValues(ctx, pairs, dst, &secret.NewSecret{Name: "dex-client-" + fs[i].Value, Namespace: "giantswarm"}); err != nil {
			t.Fatal(err)
		}
		vs, err := o.Compare(ctx, secret.Ref{File: patch, Path: from}, secret.Ref{File: dst, Path: "stringData.secret"})
		if err != nil || vs[0].State != secret.Equal {
			t.Fatalf("compare %s = %+v, %v", dst, vs, err)
		}
	}
	res, err := o.Unset(ctx, patch, []string{extraClients, musterSecret}, true)
	if err != nil || len(res.Removed) != 2 {
		t.Fatalf("unset = %+v, %v", res, err)
	}
	if got := decrypted(t, tools, patch); strings.Contains(got, clientSecret) || strings.Contains(got, peerSecret) {
		t.Errorf("the patch keeps a moved secret")
	}
	noDexSecret(t, "the flow", []any{fs, res})
}
