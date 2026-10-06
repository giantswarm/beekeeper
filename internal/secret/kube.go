package secret

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// kindContext is the prefix of the context kind names a cluster's.
const kindContext = "kind-"

// FieldManager is the server-side apply manager a copied key is written
// under.
const FieldManager = "beekeeper-secret"

// KubeTarget is one key of a Secret: <context>/<namespace>/<name>/<key>.
type KubeTarget struct {
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// ParseKubeTarget reads <context>/<namespace>/<name>/<key>.
func ParseKubeTarget(s string) (KubeTarget, error) {
	p := strings.Split(s, "/")
	if len(p) != 4 || slicesHasEmpty(p) {
		return KubeTarget{}, fmt.Errorf("%q: a Secret's key is <context>/<namespace>/<name>/<key>", s)
	}
	t := KubeTarget{Context: p[0], Namespace: p[1], Name: p[2], Key: p[3]}
	if errs := validation.IsDNS1123Label(t.Namespace); len(errs) > 0 {
		return KubeTarget{}, fmt.Errorf("%q: namespace %s", s, errs[0])
	}
	if errs := validation.IsDNS1123Subdomain(t.Name); len(errs) > 0 {
		return KubeTarget{}, fmt.Errorf("%q: name %s", s, errs[0])
	}
	if errs := validation.IsConfigMapKey(t.Key); len(errs) > 0 {
		return KubeTarget{}, fmt.Errorf("%q: key %s", s, errs[0])
	}
	return t, nil
}

// String is the target as a log and an answer name it.
func (t KubeTarget) String() string {
	return t.Context + "/" + t.Namespace + "/" + t.Name + "/" + t.Key
}

// KindCluster is the kind cluster a kind-<cluster> context names, "" for
// any other context.
func (t KubeTarget) KindCluster() string {
	cl, ok := strings.CutPrefix(t.Context, kindContext)
	if !ok {
		return ""
	}
	return cl
}

// SecretApplier writes value into one key of a Secret in the cluster a
// kubeconfig reaches, the Secret's other keys kept.
type SecretApplier func(ctx context.Context, kubeconfig []byte, t KubeTarget, value []byte) error

// ApplySecret is the SecretApplier of a real cluster: a server-side apply
// that owns the one key, creating the Secret when absent.
func ApplySecret(ctx context.Context, kubeconfig []byte, t KubeTarget, value []byte) error {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("the kubeconfig: %w", err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return err
	}
	ac := corev1ac.Secret(t.Name, t.Namespace).WithData(map[string][]byte{t.Key: value})
	return c.Apply(ctx, ac, client.FieldOwner(FieldManager), client.ForceOwnership)
}

// CopyToSecret copies one value into a key of a Secret in a kind cluster,
// reached with the admin kubeconfig kind answers, which stays in memory
// like the value. It answers the value's length. Which contexts a caller
// may write to is the caller's to check.
func (o *Ops) CopyToSecret(ctx context.Context, src Ref, t KubeTarget) (int, error) {
	if t.KindCluster() == "" {
		return 0, fmt.Errorf("%s: a Secret is written only into a kind lab's context, kind-<cluster>", t.Context)
	}
	v, err := o.value(ctx, src)
	if err != nil {
		return 0, err
	}
	return len(v), o.toSecret(ctx, v, src.String(), t)
}

// toSecret writes v into a key of a Secret in t's kind cluster, an error
// carrying v redacted as ref.
func (o *Ops) toSecret(ctx context.Context, v, ref string, t KubeTarget) error {
	kc, err := o.Run(ctx, "", nil, nil, "kind", "get", "kubeconfig", "--name", t.KindCluster())
	if err != nil {
		return fmt.Errorf("%s: %w", t.Context, err)
	}
	apply := o.Apply
	if apply == nil {
		apply = ApplySecret
	}
	if err := apply(ctx, kc, t, []byte(v)); err != nil {
		return fmt.Errorf("%s: %s", t, redact(err.Error(), v, ref))
	}
	return nil
}

// ErrVault marks a failure to read the shared vault: none configured, no
// token, or op answering an error or no answer in time.
var ErrVault = errors.New("the shared vault cannot be read")
