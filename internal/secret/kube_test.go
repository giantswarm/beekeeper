package secret_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
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

const (
	configKey        = "config.yaml"
	clientSecretPath = "clientSecret"
	noMapping        = "no YAML mapping"
	oauthKubeRef     = secret.K8sRef + labContext + "/" + kagentNS + "/" + oauthName + "/" + configKey
)

// connectorDoc is the document the Secret's key holds in the tests: a
// connector's client credentials, as an identity provider's owner places
// them.
const connectorDoc = "clientID: app-123\nclientSecret: " + password + "\n"

func TestParseRefKube(t *testing.T) {
	r, err := secret.ParseRef(oauthKubeRef + "#oidc." + clientSecretPath)
	if err != nil {
		t.Fatal(err)
	}
	want := secret.Ref{Kube: secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: oauthName, Key: configKey}, Path: "oidc." + clientSecretPath}
	if r != want || !r.Single() || !r.IsKube() || r.String() != oauthKubeRef+"#oidc."+clientSecretPath {
		t.Errorf("parsed %+v, %q", r, r)
	}
	whole, err := secret.ParseRef(oauthKubeRef)
	if err != nil || whole.Path != "" || !whole.Single() || whole.String() != oauthKubeRef {
		t.Errorf("the whole key parsed %+v, %q, %v", whole, whole, err)
	}
	for _, bad := range []string{oauthKubeRef + "#", secret.K8sRef + labContext + "/" + kagentNS + "/" + oauthName, secret.K8sRef + "/" + kagentNS + "/x/k",
		secret.K8sRef + labContext + "/Kagent/x/k", secret.K8sRef + labContext + "/" + kagentNS + "/x/k y"} {
		if _, err := secret.ParseRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	p, err := secret.ParsePair(oauthKubeRef + "#" + clientSecretPath + "=client-secret")
	if err != nil || p.Src != (secret.Ref{Kube: want.Kube, Path: clientSecretPath}) || p.Path != "client-secret" {
		t.Errorf("the pair parsed %+v, %v", p, err)
	}
}

// TestCopyFromALabSecretKeyReadsThroughKind copies one value of the
// document a lab Secret's key holds, then the whole key, into a SOPS file:
// the read goes through kind's kubeconfig like a write into the lab does,
// compare and fingerprint take the same source, and no answer carries a
// value.
func TestCopyFromALabSecretKeyReadsThroughKind(t *testing.T) {
	_, dst := scratch(t)
	tools := secrettest.New(nil)
	o := ops(tools)
	var gotKC string
	var gotTarget secret.KubeTarget
	o.Read = func(_ context.Context, kc []byte, tg secret.KubeTarget) ([]byte, error) {
		gotKC, gotTarget = string(kc), tg
		return []byte(connectorDoc), nil
	}
	ctx := context.Background()
	tg := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: oauthName, Key: configKey}
	field := secret.Ref{Kube: tg, Path: clientSecretPath}
	to := secret.Ref{File: dst, Path: "stringData." + clientSecretPath}
	n, err := o.CopyValue(ctx, field, to)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(password) || gotTarget != tg || gotKC != secrettest.Kubeconfig("agentlab") {
		t.Errorf("n %d, read %+v with %q", n, gotTarget, gotKC)
	}
	if n, err := o.CopyValue(ctx, secret.Ref{Kube: tg}, secret.Ref{File: dst, Path: "stringData.config"}); err != nil || n != len(connectorDoc) {
		t.Errorf("the whole key: %d bytes, %v", n, err)
	}
	plain := decrypted(t, tools, dst)
	for _, w := range []string{clientSecretPath + ": " + password, "config: |"} {
		if !strings.Contains(plain, w) {
			t.Errorf("the file lacks %q:\n%s", w, plain)
		}
	}
	vs, err := o.Compare(ctx, field, to)
	if err != nil || len(vs) != 1 || vs[0].State != secret.Equal {
		t.Errorf("compare = %+v, %v", vs, err)
	}
	ps, err := o.Fingerprints(ctx, secret.Ref{Kube: tg, Path: "clientID"})
	if err != nil || len(ps) != 1 || ps[0].Key != oauthKubeRef+"#clientID" {
		t.Errorf("fingerprint = %+v, %v", ps, err)
	}
	noValue(t, "the fingerprints", ps)
	if _, err := o.CopyValue(ctx, secret.Ref{Op: vaultRef}, field); err == nil || !strings.Contains(err.Error(), "file#path") {
		t.Errorf("a k8s:// destination = %v", err)
	}
}

// TestKubeSourceResolvesTheContextInTheKubeconfigFiles reads a Secret of
// a context outside a lab: the context is found in the configured
// kubeconfig files, merged as kubectl merges them, and the read's
// kubeconfig names it as the current context; a context in none of them,
// or no files at all, is refused before any read.
func TestKubeSourceResolvesTheContextInTheKubeconfigFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, context string) string {
		p := filepath.Join(dir, name)
		cfg := "apiVersion: v1\nkind: Config\ncurrent-context: " + context + "\nclusters:\n- name: " + context + "\n  cluster:\n    server: https://127.0.0.1:6443\n" +
			"contexts:\n- name: " + context + "\n  context:\n    cluster: " + context + "\n    user: " + context + "\nusers:\n- name: " + context + "\n  user:\n    token: planted-kubeconfig-" + context + "\n"
		if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o := ops(secrettest.New(nil))
	o.Kubeconfig = []string{write("machine.yaml", "other"), write("teleport.yaml", gazelleContext)}
	reads := 0
	var got *clientcmdapi.Config
	o.Read = func(_ context.Context, kc []byte, _ secret.KubeTarget) ([]byte, error) {
		reads++
		var err error
		got, err = clientcmd.Load(kc)
		return []byte(token), err
	}
	ctx := context.Background()
	tg := secret.KubeTarget{Context: gazelleContext, Namespace: kagentNS, Name: oauthName, Key: configKey}
	ps, err := o.Fingerprints(ctx, secret.Ref{Kube: tg})
	if err != nil || len(ps) != 1 {
		t.Fatalf("fingerprint = %+v, %v", ps, err)
	}
	if got == nil || got.CurrentContext != gazelleContext || len(got.Contexts) != 2 {
		t.Errorf("the read's kubeconfig: %+v", got)
	}
	noValue(t, "the fingerprint", ps)
	absent := secret.KubeTarget{Context: "teleport.giantswarm.io-absent", Namespace: kagentNS, Name: oauthName, Key: configKey}
	if _, err := o.Fingerprints(ctx, secret.Ref{Kube: absent}); err == nil || !strings.Contains(err.Error(), "no such context") || !strings.Contains(err.Error(), "teleport.yaml") {
		t.Errorf("an absent context = %v", err)
	}
	o.Kubeconfig = nil
	if _, err := o.Fingerprints(ctx, secret.Ref{Kube: tg}); err == nil || !strings.Contains(err.Error(), "kube.kubeconfig") {
		t.Errorf("no kubeconfig = %v", err)
	}
	if reads != 1 {
		t.Errorf("%d reads, want the one of the resolved context", reads)
	}
}

// TestKubeSourceErrorsNameTheMissingPartAndNoValue: a Secret, key or path
// absent, and a key holding no YAML mapping where a path is asked, fail in
// one line naming it, the key's content never quoted.
func TestKubeSourceErrorsNameTheMissingPartAndNoValue(t *testing.T) {
	o := ops(secrettest.New(nil))
	ctx := context.Background()
	tg := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: oauthName, Key: configKey}
	for _, tc := range []struct {
		name, want string
		path       string
		read       func() ([]byte, error)
	}{
		{"absent Secret", "not found", "", func() ([]byte, error) { return nil, errors.New(`secrets "github-oauth-client" not found`) }},
		{"absent path", "no value at oidc.clientToken", "oidc.clientToken", func() ([]byte, error) { return []byte(connectorDoc), nil }},
		{"no mapping", noMapping, clientSecretPath, func() ([]byte, error) { return []byte(password), nil }},
		{"a list", noMapping, clientSecretPath, func() ([]byte, error) { return []byte("- " + password + "\n"), nil }},
	} {
		o.Read = func(context.Context, []byte, secret.KubeTarget) ([]byte, error) { return tc.read() }
		_, err := o.Fingerprints(ctx, secret.Ref{Kube: tg, Path: tc.path})
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tg.String()) {
			t.Errorf("%s = %v, want %q naming %s", tc.name, err, tc.want, tg)
			continue
		}
		noValue(t, tc.name, err.Error())
	}
	// a key holding no YAML is still a value when no path is asked
	o.Read = func(context.Context, []byte, secret.KubeTarget) ([]byte, error) { return []byte(password), nil }
	ps, err := o.Fingerprints(ctx, secret.Ref{Kube: tg})
	if err != nil || len(ps) != 1 || ps[0].Fingerprint != o.Fingerprint(password) {
		t.Errorf("the whole key = %+v, %v", ps, err)
	}
}
