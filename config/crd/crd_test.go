package crd_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	openapierrors "k8s.io/kube-openapi/pkg/validation/errors"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/beekeeper/config/crd"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// TestQueueAdmission exercises the persisted queue bound independently of the
// store's pre-write guard. A generated schema can stay in sync with the API
// source while accidentally losing or tightening that bound.
func TestQueueAdmission(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	by := v1alpha1.Party{Name: "fixture", Team: "test-team"}
	for _, tc := range []struct {
		plural string
		object func(int) any
	}{
		{"environments", func(n int) any {
			queue := make([]v1alpha1.Grant, n)
			for i := range queue {
				queue[i] = v1alpha1.Grant{To: v1alpha1.Party{Name: "worker-" + strconv.Itoa(i)}, By: by, At: at}
			}
			return &v1alpha1.Environment{
				Spec:   v1alpha1.EnvironmentSpec{Installation: "test-installation"},
				Status: v1alpha1.EnvironmentStatus{Queue: queue, QueueLength: n},
			}
		}},
		{"mergelanes", func(n int) any {
			queue := make([]v1alpha1.LaneEntry, n)
			for i := range queue {
				queue[i] = v1alpha1.LaneEntry{By: by, Repo: "example/repo", PR: i + 1, Phase: "waiting", Arrived: at}
			}
			return &v1alpha1.MergeLane{
				Status: v1alpha1.MergeLaneStatus{Queue: queue, QueueLength: n},
			}
		}},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			schema := queueSchema(t, tc.plural)
			validator := validate.NewSchemaValidator(schema, nil, "", strfmt.Default)
			for _, n := range []int{0, v1alpha1.MaxQueue, v1alpha1.MaxQueue + 1} {
				t.Run(strconv.Itoa(n), func(t *testing.T) {
					raw, err := json.Marshal(tc.object(n))
					if err != nil {
						t.Fatal(err)
					}
					var obj map[string]any
					if err := json.Unmarshal(raw, &obj); err != nil {
						t.Fatal(err)
					}
					// Queue has omitempty on the API types. Validate an explicit
					// empty array too, so minItems cannot silently tighten the bound.
					if n == 0 {
						obj["status"].(map[string]any)["queue"] = []any{}
					}
					result := validator.Validate(obj)
					if n <= v1alpha1.MaxQueue {
						if !result.IsValid() {
							t.Fatalf("valid queue of %d entries rejected: %v", n, result.Errors)
						}
						return
					}
					// Require the queue-bound error, so malformed fixture fields
					// cannot stand in for the admission rule under test.
					if len(result.Errors) != 1 {
						t.Fatalf("queue of %d entries: want one queue limit error, got %v", n, result.Errors)
					}
					var validation *openapierrors.Validation
					if !errors.As(result.Errors[0], &validation) || validation.Code() != openapierrors.MaxItemsFailCode || validation.Name != "status.queue" {
						t.Fatalf("queue of %d entries: want status.queue maxItems error, got %v", n, result.Errors)
					}
				})
			}
		})
	}
}

func queueSchema(t *testing.T, plural string) *spec.Schema {
	t.Helper()
	raw, err := crd.Files.ReadFile("beekeeper.giantswarm.io_" + plural + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var definition apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &definition); err != nil {
		t.Fatal(err)
	}
	for _, version := range definition.Spec.Versions {
		if version.Name != v1alpha1.GroupVersion.Version {
			continue
		}
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			t.Fatal("CRD has no OpenAPI schema")
		}
		raw, err := json.Marshal(version.Schema.OpenAPIV3Schema)
		if err != nil {
			t.Fatal(err)
		}
		var schema spec.Schema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		return &schema
	}
	t.Fatalf("CRD has no version %s", v1alpha1.GroupVersion.Version)
	return nil
}
