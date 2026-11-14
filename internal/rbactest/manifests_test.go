package rbactest

import (
	"context"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func valueSetNames() []string {
	names := make([]string, 0, len(valueSets))
	for name := range valueSets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func find(docs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, doc := range docs {
		if doc.GetKind() == kind && doc.GetName() == name {
			return doc
		}
	}
	return nil
}

// TestRBACStaysInTheNamespace: no cluster-wide RBAC, and only the engine is bound to anything.
func TestRBACStaysInTheNamespace(t *testing.T) {
	t.Parallel()
	for _, set := range valueSetNames() {
		for _, doc := range rendered[set] {
			switch doc.GetKind() {
			case "ClusterRole", "ClusterRoleBinding":
				t.Errorf("%s values: %s %s is cluster-wide", set, doc.GetKind(), doc.GetName())
			case "Role", kindRoleBinding:
				if doc.GetNamespace() != namespace {
					t.Errorf("%s values: %s %s is in namespace %q, not the release's", set, doc.GetKind(), doc.GetName(), doc.GetNamespace())
				}
			}
			if doc.GetKind() != kindRoleBinding {
				continue
			}
			subjects, _, _ := unstructured.NestedSlice(doc.Object, "subjects")
			for _, s := range subjects {
				subject, _ := s.(map[string]any)
				if subject["kind"] != "ServiceAccount" || subject["name"] != engineSA || subject["namespace"] != namespace {
					t.Errorf("%s values: RoleBinding %s binds %v; only the engine's ServiceAccount may be bound", set, doc.GetName(), subject)
				}
			}
		}
	}
}

// TestOnlyTheEngineGetsAToken: the engine mounts its token explicitly (admission policies may turn
// the default off), the receiver never.
func TestOnlyTheEngineGetsAToken(t *testing.T) {
	t.Parallel()
	for _, set := range valueSetNames() {
		docs := rendered[set]
		for _, c := range []struct {
			deployment, serviceAccount string
			token                      bool
		}{
			{engineSA, engineSA, true},
			{receiverSA, receiverSA, false},
		} {
			d := find(docs, "Deployment", c.deployment)
			if d == nil {
				t.Fatalf("%s values: no Deployment %s", set, c.deployment)
			}
			if sa, _, _ := unstructured.NestedString(d.Object, "spec", "template", "spec", "serviceAccountName"); sa != c.serviceAccount {
				t.Errorf("%s values: Deployment %s runs as %q, want %q", set, c.deployment, sa, c.serviceAccount)
			}
			token, found, _ := unstructured.NestedBool(d.Object, "spec", "template", "spec", "automountServiceAccountToken")
			if !found || token != c.token {
				t.Errorf("%s values: Deployment %s automountServiceAccountToken = %v (set: %v), want %v", set, c.deployment, token, found, c.token)
			}
			sa := find(docs, "ServiceAccount", c.serviceAccount)
			if sa == nil {
				t.Fatalf("%s values: no ServiceAccount %s", set, c.serviceAccount)
			}
			if token, found, _ := unstructured.NestedBool(sa.Object, "automountServiceAccountToken"); !found || token != c.token {
				t.Errorf("%s values: ServiceAccount %s automountServiceAccountToken = %v (set: %v), want %v", set, c.serviceAccount, token, found, c.token)
			}
		}
	}
}

// TestManifestsAreValid creates every rendered object of a built-in kind as a server-side dry run,
// so the API server validates it. The kinds of other operators (PodMonitor, PrometheusRule,
// CiliumNetworkPolicy) have no CRDs here.
func TestManifestsAreValid(t *testing.T) {
	t.Parallel()
	c, err := client.New(adminConfig, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	builtIn := map[string]bool{"v1": true, "apps/v1": true, "rbac.authorization.k8s.io/v1": true, "policy/v1": true}
	for _, set := range valueSetNames() {
		var checked []string
		for _, doc := range rendered[set] {
			if !builtIn[doc.GetAPIVersion()] {
				continue
			}
			obj := doc.DeepCopy()
			obj.SetNamespace(validateNamespace)
			if err := c.Create(context.Background(), obj, client.DryRunAll); err != nil {
				t.Errorf("%s values: %s %s: %v", set, doc.GetKind(), doc.GetName(), err)
			}
			checked = append(checked, doc.GetKind()+"/"+doc.GetName())
		}
		if len(checked) == 0 {
			t.Fatalf("%s values: no manifests of a built-in kind", set)
		}
		t.Logf("%s values: validated %s", set, strings.Join(checked, ", "))
	}
}
