package secret_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const tokenUser = "x-access-token"

func TestParseEncoding(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want secret.Encoding
	}{
		{"", secret.Encoding{}},
		{"base64", secret.Encoding{Base64: true}},
		{"basic:" + tokenUser, secret.Encoding{Base64: true, User: tokenUser}},
	} {
		got, err := secret.ParseEncoding(tc.in)
		if err != nil || got != tc.want || got.String() != tc.in {
			t.Errorf("%q: %+v (%q), %v", tc.in, got, got.String(), err)
		}
	}
	for _, bad := range []string{"hex", "base32", "basic", "basic:", "basic:a:b", "Base64"} {
		if _, err := secret.ParseEncoding(bad); err == nil || !strings.Contains(err.Error(), "one of base64, basic:<user>") && !strings.Contains(err.Error(), "basic:<user>, a user") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestEncodingApply(t *testing.T) {
	for _, tc := range []struct {
		enc  secret.Encoding
		want string
	}{
		{secret.Encoding{}, password},
		{secret.Encoding{Base64: true}, base64.StdEncoding.EncodeToString([]byte(password))},
		{secret.Encoding{Base64: true, User: tokenUser}, base64.StdEncoding.EncodeToString([]byte(tokenUser + ":" + password))},
	} {
		if got := tc.enc.Apply(password); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.enc, got, tc.want)
		}
	}
}

// identityPrints makes a fingerprint the value itself, so a test sees which
// form was fingerprinted.
func identityPrints(o *secret.Ops) *secret.Ops {
	o.Fingerprint = func(v string) string { return "fp:" + v }
	return o
}

func TestCopyToSecretEncodesBeforeTheWrite(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	o := identityPrints(ops(tools))
	o.Encode = secret.Encoding{Base64: true, User: tokenUser}
	stored := map[secret.KubeTarget][]byte{}
	o.Apply = func(_ context.Context, _ []byte, tg secret.KubeTarget, v []byte) error {
		stored[tg] = v
		return nil
	}
	o.Read = func(_ context.Context, _ []byte, tg secret.KubeTarget) ([]byte, error) { return stored[tg], nil }
	tg := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: "private-skills", Key: "token"}
	want := base64.StdEncoding.EncodeToString([]byte(tokenUser + ":" + password))
	n, err := o.CopyToSecret(context.Background(), secret.Ref{Op: vaultRef}, tg)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(want) || string(stored[tg]) != want {
		t.Errorf("n %d, stored %q, want %q", n, stored[tg], want)
	}
	got, err := o.SecretFingerprint(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := o.Fingerprints(context.Background(), secret.Ref{Op: vaultRef})
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != 1 || got.Fingerprint != expected[0].Fingerprint || got.Fingerprint != "fp:"+want {
		t.Errorf("the Secret's %+v, the expected form's %+v", got, expected)
	}
}

func TestSecretFingerprintRefusesANonKindContext(t *testing.T) {
	o := ops(secrettest.New(nil))
	if _, err := o.SecretFingerprint(context.Background(), secret.KubeTarget{Context: gazelleContext, Namespace: kagentNS, Name: "x", Key: "k"}); err == nil {
		t.Error("a non-kind context was read")
	}
}

func TestFingerprintEncodedTakesOneValue(t *testing.T) {
	o := ops(secrettest.New(map[string]string{vaultRef: password}))
	o.Encode = secret.Encoding{Base64: true}
	if _, err := o.Fingerprints(context.Background(), secret.Ref{File: "app.sops.yaml"}); err == nil || !strings.Contains(err.Error(), "name one value") {
		t.Errorf("a whole file was fingerprinted encoded: %v", err)
	}
}

func TestCopyToConsumerEncodes(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	o := ops(tools)
	o.Encode = secret.Encoding{Base64: true}
	code, out, err := o.CopyToConsumer(context.Background(), secret.Ref{Op: vaultRef}, []string{"sh", "-c", "cat; true", "--password-stdin"}, secret.Stdin{})
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(password))) || strings.Contains(out, password) {
		t.Errorf("the consumer's output carries the value: %q", out)
	}
	if !strings.Contains(out, "[redacted: "+vaultRef+"]") {
		t.Errorf("the consumer's output %q: no redaction of the encoded value", out)
	}
}
