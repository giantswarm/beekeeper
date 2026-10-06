package secret_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	otherVaultRef  = "op://Private/x/y"
	kagentNS       = "kagent"
	labContext     = "kind-agentlab"
	gazelleContext = "teleport.giantswarm.io-gazelle"
	alnumSet       = "alnum"
	hexSet         = "hex"
	catCmd         = "cat"
	secretWord     = "secret"
	setWord        = "set"
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
