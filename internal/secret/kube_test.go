package secret_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	otherVaultRef   = "op://Private/x/y"
	kagentNS        = "kagent"
	labContext      = "kind-agentlab"
	gazelleContext  = "teleport.giantswarm.io-gazelle"
	alnumSet        = "alnum"
	hexSet          = "hex"
	catCmd          = "cat"
	secretWord      = "secret"
	setWord         = "set"
	oauthName       = "github-oauth-client"
	clientIDKey     = "client-id"
	clientSecretKey = "client-secret"
)

func TestParseKubeTarget(t *testing.T) {
	got, err := secret.ParseKubeTarget("kind-agentlab/kagent/kagent-anthropic/ANTHROPIC_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	want := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: "kagent-anthropic", Key: "ANTHROPIC_API_KEY"}
	if got != want || got.KindCluster() != "agentlab" {
		t.Errorf("parsed %+v, cluster %q", got, got.KindCluster())
	}
	for _, bad := range []string{"kind-agentlab/kagent/kagent-anthropic", "kind-agentlab//x/k", "kind-agentlab/Kagent/x/k", "kind-agentlab/kagent/x/k y", "a/b/c/d/e"} {
		if _, err := secret.ParseKubeTarget(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if c := (secret.KubeTarget{Context: gazelleContext}).KindCluster(); c != "" {
		t.Errorf("a non-kind context names cluster %q", c)
	}
}

func TestCopyToSecretAppliesWithKindsKubeconfig(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	o := ops(tools)
	var gotKC, gotValue string
	var gotTarget secret.KubeTarget
	o.Apply = func(_ context.Context, kc []byte, tg secret.KubeTarget, v []byte) error {
		gotKC, gotTarget, gotValue = string(kc), tg, string(v)
		return nil
	}
	tg := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: "kagent-anthropic", Key: "ANTHROPIC_API_KEY"}
	n, err := o.CopyToSecret(context.Background(), secret.Ref{Op: vaultRef}, tg)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(password) || gotValue != password || gotTarget != tg || gotKC != secrettest.Kubeconfig("agentlab") {
		t.Errorf("n %d, applied %+v with %q", n, gotTarget, gotKC)
	}
}

// TestWriteSecretKeyKeepsTheSecretsOtherKeys copies two keys in sequence
// into one Secret that already holds both as placeholders and a third key:
// each write touches its key alone, the rest of the Secret stays.
func TestWriteSecretKeyKeepsTheSecretsOtherKeys(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/managed-by": "placeholders"}
	annotations := map[string]string{"example.com/placeholder": "true"}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: kagentNS, Name: oauthName, Labels: maps.Clone(labels), Annotations: maps.Clone(annotations)},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{clientIDKey: []byte("placeholder"), clientSecretKey: []byte("placeholder"), "extra": []byte("stays")},
	}
	c := fake.NewClientBuilder().WithObjects(existing).Build()
	id, secretValue := []byte("Iv1.0123456789abcdef"), []byte(strings.Repeat("s", 40))
	writeKey(t, c, clientIDKey, id)
	writeKey(t, c, clientSecretKey, secretValue)
	got := readSecret(t, c, oauthName)
	wantData := map[string][]byte{clientIDKey: id, clientSecretKey: secretValue, "extra": []byte("stays")}
	if !maps.EqualFunc(got.Data, wantData, bytes.Equal) {
		t.Errorf("data %v, want %v", keySizes(got.Data), keySizes(wantData))
	}
	if !maps.Equal(got.Labels, labels) || !maps.Equal(got.Annotations, annotations) || got.Type != corev1.SecretTypeOpaque {
		t.Errorf("labels %v, annotations %v, type %q changed", got.Labels, got.Annotations, got.Type)
	}
}

// TestWriteSecretKeyCreatesAnAbsentSecret writes two keys into a Secret
// that does not exist yet: the first write creates it, the second adds
// its key next to the first.
func TestWriteSecretKeyCreatesAnAbsentSecret(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	id, secretValue := []byte("Iv1.0123456789abcdef"), []byte(strings.Repeat("s", 40))
	writeKey(t, c, clientIDKey, id)
	writeKey(t, c, clientSecretKey, secretValue)
	got := readSecret(t, c, oauthName)
	wantData := map[string][]byte{clientIDKey: id, clientSecretKey: secretValue}
	if !maps.EqualFunc(got.Data, wantData, bytes.Equal) {
		t.Errorf("data %v, want %v", keySizes(got.Data), keySizes(wantData))
	}
}

func writeKey(t *testing.T, c client.Client, key string, value []byte) {
	t.Helper()
	tg := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: oauthName, Key: key}
	if err := secret.WriteSecretKey(context.Background(), c, tg, value); err != nil {
		t.Fatalf("%s: %v", tg, err)
	}
}

func readSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kagentNS, Name: name}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

// keySizes names a Secret's keys with their values' sizes: what a failure
// may print.
func keySizes(data map[string][]byte) map[string]int {
	sizes := make(map[string]int, len(data))
	for k, v := range data {
		sizes[k] = len(v)
	}
	return sizes
}

func TestCopyToSecretRedactsTheApplyError(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	o := ops(tools)
	o.Apply = func(_ context.Context, _ []byte, _ secret.KubeTarget, v []byte) error {
		return errors.New("Secret is invalid: " + string(v))
	}
	_, err := o.CopyToSecret(context.Background(), secret.Ref{Op: vaultRef},
		secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: "x", Key: "k"})
	if err == nil {
		t.Fatal("no error")
	}
	noValue(t, "the error", err.Error())
}

func TestCopyToSecretOnlyIntoAKindContext(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	o := ops(tools)
	o.Apply = func(context.Context, []byte, secret.KubeTarget, []byte) error { t.Error("applied"); return nil }
	_, err := o.CopyToSecret(context.Background(), secret.Ref{Op: vaultRef},
		secret.KubeTarget{Context: gazelleContext, Namespace: kagentNS, Name: "x", Key: "k"})
	if err == nil || !strings.Contains(err.Error(), "kind-<cluster>") {
		t.Errorf("a non-kind context = %v", err)
	}
	if len(tools.Calls) != 0 {
		t.Errorf("ran %q", tools.Calls)
	}
}

func TestAVaultFailureIsErrVault(t *testing.T) {
	tools := secrettest.New(nil)
	o := ops(tools)
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: vaultRef}); !errors.Is(err, secret.ErrVault) {
		t.Errorf("op failing = %v", err)
	}
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: otherVaultRef}); errors.Is(err, secret.ErrVault) {
		t.Errorf("another vault is a refusal, not the vault failing: %v", err)
	}
	o.Token = ""
	if _, err := o.Fingerprints(context.Background(), secret.Ref{Op: vaultRef}); !errors.Is(err, secret.ErrVault) {
		t.Errorf("no token = %v", err)
	}
}
