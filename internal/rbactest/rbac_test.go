package rbactest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	release = "beekeeper"
	// namespace is the release's namespace, where the engine's Role grants everything it needs.
	namespace = "beekeeper"
	// otherNamespace is any other namespace: the engine may do nothing there.
	otherNamespace = "elsewhere"
	// validateNamespace takes the server-side dry runs of every rendered manifest.
	validateNamespace = "validate"

	engineSA   = "beekeeper-engine"
	receiverSA = "beekeeper-receiver"
	// unboundSA has no binding at all: what every ServiceAccount may do anyway.
	unboundSA = "unbound"

	envtestK8sVersion = "1.37.0"
	apiGroup          = "beekeeper.giantswarm.io"
	coreGroup         = "core"
	rbacGroup         = "rbac.authorization.k8s.io"

	kindRoleBinding = "RoleBinding"
	installations   = "installations"

	verbGet    = "get"
	verbList   = "list"
	verbWatch  = "watch"
	verbCreate = "create"
	verbUpdate = "update"
	verbPatch  = "patch"
	verbDelete = "delete"
)

var chartDir = filepath.Join("..", "..", "helm", "beekeeper")

// The value sets the chart is rendered with: its defaults and production-like values.
var valueSets = map[string][]string{
	"defaults":   nil,
	"production": {filepath.Join(chartDir, "ci", "ci-values.yaml")},
}

// Set up once by TestMain.
var (
	adminConfig *rest.Config
	rendered    map[string][]*unstructured.Unstructured
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	unavailable := func(format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		if os.Getenv("CI") != "" {
			fmt.Fprintln(os.Stderr, "rbactest:", msg)
			return 1
		}
		fmt.Fprintln(os.Stderr, "rbactest: skipping:", msg)
		return 0
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "rbactest:", err)
		return 1
	}

	if _, err := exec.LookPath("helm"); err != nil {
		return unavailable("helm is not on PATH")
	}
	assets, err := envtestAssets()
	if err != nil {
		return unavailable("%v (make test-deps fetches them)", err)
	}

	helmHome, err := os.MkdirTemp("", "rbactest-helm-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(helmHome) }()
	rendered = map[string][]*unstructured.Unstructured{}
	for name, files := range valueSets {
		docs, err := render(helmHome, files)
		if err != nil {
			return fail(fmt.Errorf("render with %s values: %w", name, err))
		}
		rendered[name] = docs
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(chartDir, "crds")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assets,
	}
	cfg, err := env.Start()
	if err != nil {
		return fail(fmt.Errorf("start envtest from %s: %w", assets, err))
	}
	defer func() { _ = env.Stop() }()
	adminConfig = cfg

	if err := install(cfg, rendered["production"]); err != nil {
		return fail(err)
	}
	return m.Run()
}

// envtestAssets finds kube-apiserver and etcd: KUBEBUILDER_ASSETS, else the test-deps cache.
func envtestAssets() (string, error) {
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		deps := os.Getenv("BEEKEEPER_TEST_DEPS")
		if deps == "" {
			cache, err := os.UserCacheDir()
			if err != nil {
				return "", err
			}
			deps = filepath.Join(cache, "beekeeper", "test-deps")
		}
		dir = filepath.Join(deps, "envtest", "k8s", fmt.Sprintf("%s-%s-%s", envtestK8sVersion, runtime.GOOS, runtime.GOARCH))
	}
	for _, bin := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(dir, bin)); err != nil {
			return "", fmt.Errorf("no envtest binaries in %s: %w", dir, err)
		}
	}
	return dir, nil
}

// render runs helm template on the chart, with its own empty helm home.
func render(helmHome string, valueFiles []string) ([]*unstructured.Unstructured, error) {
	args := []string{"template", release, chartDir, "--namespace", namespace}
	for _, f := range valueFiles {
		args = append(args, "--values", f)
	}
	cmd := exec.Command("helm", args...) // #nosec G204 -- the chart's own paths and fixed flags
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(helmHome, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(helmHome, "config"),
		"HELM_DATA_HOME="+filepath.Join(helmHome, "data"),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("helm %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return decode(out)
}

func decode(manifests []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifests), 4096)
	var docs []*unstructured.Unstructured
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, err
		}
		if len(obj) == 0 {
			continue
		}
		docs = append(docs, &unstructured.Unstructured{Object: obj})
	}
}

// install creates the namespaces and applies the chart's ServiceAccounts and RBAC as an admin.
func install(cfg *rest.Config, docs []*unstructured.Unstructured) error {
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, ns := range []string{namespace, otherNamespace, validateNamespace} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return fmt.Errorf("create namespace %s: %w", ns, err)
		}
	}
	for _, doc := range docs {
		switch doc.GetKind() {
		case "ServiceAccount", "Role", kindRoleBinding, "ClusterRole", "ClusterRoleBinding":
			if err := c.Create(ctx, doc.DeepCopy()); err != nil {
				return fmt.Errorf("create %s %s: %w", doc.GetKind(), doc.GetName(), err)
			}
		}
	}
	return nil
}

func serviceAccountUser(name string) (string, []string) {
	return "system:serviceaccount:" + namespace + ":" + name,
		[]string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"}
}

// as returns a config that impersonates one of the namespace's ServiceAccounts.
func as(sa string) *rest.Config {
	cfg := rest.CopyConfig(adminConfig)
	user, groups := serviceAccountUser(sa)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	return cfg
}

func clientAs(t *testing.T, sa string) client.Client {
	t.Helper()
	c, err := client.New(as(sa), client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// permissions are what a ServiceAccount may do in ns, one "verb group/resource" per rule,
// from a SelfSubjectRulesReview.
func permissions(t *testing.T, sa, ns string) map[string]bool {
	t.Helper()
	cs, err := kubernetes.NewForConfig(as(sa))
	if err != nil {
		t.Fatal(err)
	}
	review, err := cs.AuthorizationV1().SelfSubjectRulesReviews().Create(context.Background(),
		&authorizationv1.SelfSubjectRulesReview{Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: ns}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if review.Status.Incomplete {
		t.Fatalf("rules review of %s in %s is incomplete: %s", sa, ns, review.Status.EvaluationError)
	}
	set := map[string]bool{}
	for _, rule := range review.Status.ResourceRules {
		names := ""
		if len(rule.ResourceNames) > 0 {
			names = " " + strings.Join(rule.ResourceNames, ",")
		}
		for _, verb := range rule.Verbs {
			for _, group := range rule.APIGroups {
				if group == "" {
					group = coreGroup
				}
				for _, resource := range rule.Resources {
					set[verb+" "+group+"/"+resource+names] = true
				}
			}
		}
	}
	for _, rule := range review.Status.NonResourceRules {
		for _, verb := range rule.Verbs {
			for _, url := range rule.NonResourceURLs {
				set[verb+" "+url] = true
			}
		}
	}
	return set
}

// beyondUnbound is what sa may do in ns beyond what a ServiceAccount without any binding may.
func beyondUnbound(t *testing.T, sa, ns string) []string {
	t.Helper()
	baseline := permissions(t, unboundSA, ns)
	var extra []string
	for p := range permissions(t, sa, ns) {
		if !baseline[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	return extra
}

// enginePermissions is everything the engine needs, all in its namespace.
func enginePermissions() []string {
	var p []string
	add := func(group string, resources, verbs []string) {
		for _, r := range resources {
			for _, v := range verbs {
				p = append(p, v+" "+group+"/"+r)
			}
		}
	}
	owned := []string{"items", "changes", "budgets"}
	config := []string{installations, "teams", "people"}
	all := append(append([]string{}, owned...), config...)
	add(apiGroup, owned, []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete})
	add(apiGroup, config, []string{verbGet, verbList, verbWatch})
	for _, r := range all {
		add(apiGroup, []string{r + "/status"}, []string{verbGet, verbUpdate, verbPatch})
	}
	add("coordination.k8s.io", []string{"leases"}, []string{verbGet, verbCreate, verbUpdate})
	add(coreGroup, []string{"events"}, []string{verbCreate, verbPatch})
	add("events.k8s.io", []string{"events"}, []string{verbCreate, verbPatch})
	sort.Strings(p)
	return p
}

func TestEngineMayDoExactlyWhatItNeedsInItsNamespace(t *testing.T) {
	t.Parallel()
	got := beyondUnbound(t, engineSA, namespace)
	want := enginePermissions()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the engine's permissions in %s:\n%s", namespace, diff(want, got))
	}
}

func TestEngineMayDoNothingInOtherNamespaces(t *testing.T) {
	t.Parallel()
	if got := beyondUnbound(t, engineSA, otherNamespace); len(got) > 0 {
		t.Errorf("the engine may do more than an unbound ServiceAccount in %s: %v", otherNamespace, got)
	}
}

func TestReceiverMayDoNothing(t *testing.T) {
	t.Parallel()
	for _, ns := range []string{namespace, otherNamespace} {
		if got := beyondUnbound(t, receiverSA, ns); len(got) > 0 {
			t.Errorf("the receiver may do more than an unbound ServiceAccount in %s: %v", ns, got)
		}
	}
}

// TestAccessReviews asks the authorizer itself (SubjectAccessReview), so cluster-scoped and
// all-namespace access is covered too, which a namespace's rules review does not show.
func TestAccessReviews(t *testing.T) {
	t.Parallel()
	type access struct {
		verb, group, resource, subresource, namespace string
	}
	var allowed, denied []access
	for _, p := range enginePermissions() {
		verb, rest, _ := strings.Cut(p, " ")
		group, resource, _ := strings.Cut(rest, "/")
		resource, sub, _ := strings.Cut(resource, "/")
		if group == coreGroup {
			group = ""
		}
		allowed = append(allowed, access{verb, group, resource, sub, namespace})
		denied = append(denied, access{verb, group, resource, sub, otherNamespace})
		if verb == verbList || verb == verbWatch {
			denied = append(denied, access{verb, group, resource, sub, ""}) // across all namespaces
		}
	}
	denied = append(denied,
		// Configuration is applied by the deployment, never by the engine.
		access{verbCreate, apiGroup, installations, "", namespace},
		access{verbUpdate, apiGroup, installations, "", namespace},
		access{verbDelete, apiGroup, "teams", "", namespace},
		access{verbPatch, apiGroup, "people", "", namespace},
		access{verbGet, "", "secrets", "", namespace},
		access{verbList, "", "secrets", "", namespace},
		access{verbGet, "", "configmaps", "", namespace},
		access{verbList, "", "pods", "", namespace},
		access{verbDelete, "coordination.k8s.io", "leases", "", namespace},
		access{verbCreate, rbacGroup, "roles", "", namespace},
		access{verbCreate, rbacGroup, "rolebindings", "", namespace},
		access{"impersonate", "", "serviceaccounts", "", namespace},
		access{verbCreate, "", "serviceaccounts", "token", namespace},
		access{verbList, "", "namespaces", "", ""},
		access{verbGet, "", "nodes", "", ""},
		access{verbCreate, "apiextensions.k8s.io", "customresourcedefinitions", "", ""},
		access{verbCreate, rbacGroup, "clusterrolebindings", "", ""},
	)

	cs, err := kubernetes.NewForConfig(adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	review := func(sa string, a access) bool {
		t.Helper()
		user, groups := serviceAccountUser(sa)
		r, err := cs.AuthorizationV1().SubjectAccessReviews().Create(context.Background(), &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   user,
				Groups: groups,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: a.namespace, Verb: a.verb, Group: a.group, Resource: a.resource, Subresource: a.subresource,
				},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return r.Status.Allowed
	}
	for _, a := range allowed {
		if !review(engineSA, a) {
			t.Errorf("the engine may not %+v", a)
		}
		if review(receiverSA, a) {
			t.Errorf("the receiver may %+v", a)
		}
	}
	for _, a := range denied {
		if review(engineSA, a) {
			t.Errorf("the engine may %+v", a)
		}
		if review(receiverSA, a) {
			t.Errorf("the receiver may %+v", a)
		}
	}
}

// TestEngineCallsTheAPI makes the engine's kinds of calls as its ServiceAccount.
func TestEngineCallsTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := clientAs(t, engineSA)

	item := &unstructured.Unstructured{}
	item.SetAPIVersion(apiGroup + "/v1alpha1")
	item.SetKind("Item")
	item.SetNamespace(namespace)
	item.SetName("giantswarm-roadmap-101")
	must(t, "create an Item", c.Create(ctx, item))
	must(t, "update the Item's status", c.Status().Update(ctx, item))
	must(t, "delete the Item", c.Delete(ctx, item))

	installations := &unstructured.UnstructuredList{}
	installations.SetAPIVersion(apiGroup + "/v1alpha1")
	installations.SetKind("InstallationList")
	must(t, "list Installations", c.List(ctx, installations, client.InNamespace(namespace)))

	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "beekeeper-engine"}}
	must(t, "create the leader Lease", c.Create(ctx, lease))
	must(t, "get the leader Lease", c.Get(ctx, client.ObjectKeyFromObject(lease), lease))
	holder := "beekeeper-engine-a"
	lease.Spec.HolderIdentity = &holder
	must(t, "update the leader Lease", c.Update(ctx, lease))

	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: "giantswarm-roadmap-101.1"},
		InvolvedObject: corev1.ObjectReference{Kind: "Item", Namespace: namespace, Name: "giantswarm-roadmap-101", APIVersion: apiGroup + "/v1alpha1"},
		Reason:         "Observed",
		Message:        "rbactest",
		Type:           corev1.EventTypeNormal,
	}
	must(t, "create an Event", c.Create(ctx, event))

	installation := &unstructured.Unstructured{}
	installation.SetAPIVersion(apiGroup + "/v1alpha1")
	installation.SetKind("Installation")
	installation.SetNamespace(namespace)
	installation.SetName("giantswarm")
	forbidden(t, "create an Installation", c.Create(ctx, installation))
	forbidden(t, "list Secrets", c.List(ctx, &corev1.SecretList{}, client.InNamespace(namespace)))
	other := item.DeepCopy()
	other.SetNamespace(otherNamespace)
	other.SetResourceVersion("")
	forbidden(t, "create an Item in another namespace", c.Create(ctx, other))
}

// TestReceiverCallsNothing: the receiver's ServiceAccount is refused everything the engine does.
func TestReceiverCallsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := clientAs(t, receiverSA)

	items := &unstructured.UnstructuredList{}
	items.SetAPIVersion(apiGroup + "/v1alpha1")
	items.SetKind("ItemList")
	forbidden(t, "list Items", c.List(ctx, items, client.InNamespace(namespace)))
	forbidden(t, "create a Lease", c.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "receiver"}}))
	forbidden(t, "list Secrets", c.List(ctx, &corev1.SecretList{}, client.InNamespace(namespace)))
	forbidden(t, "create an Event", c.Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: "receiver.1"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: namespace, Name: "receiver"},
	}))
}

func must(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
}

func forbidden(t *testing.T, what string, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) {
		t.Errorf("%s: want forbidden, got %v", what, err)
	}
}

func diff(want, got []string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, p := range want {
		w[p] = true
	}
	for _, p := range got {
		g[p] = true
	}
	var b strings.Builder
	for _, p := range want {
		if !g[p] {
			fmt.Fprintf(&b, "  missing: %s\n", p)
		}
	}
	for _, p := range got {
		if !w[p] {
			fmt.Fprintf(&b, "  extra:   %s\n", p)
		}
	}
	return b.String()
}
