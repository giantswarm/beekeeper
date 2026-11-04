package crd

import (
	"fmt"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// Validate the shipped schemas directly, bypassing the store's queue guard.
func TestQueueAdmission(t *testing.T) {
	for _, kind := range []struct {
		name, manifest string
		entry          func(int) map[string]interface{}
	}{
		{
			name: "Environment", manifest: "beekeeper.giantswarm.io_environments.yaml",
			entry: func(i int) map[string]interface{} {
				return map[string]interface{}{
					"at": "2026-01-01T00:00:00Z",
					"by": map[string]interface{}{"name": "test-supervisor"},
					"to": map[string]interface{}{"name": fmt.Sprintf("test-agent-%d", i)},
				}
			},
		},
		{
			name: "MergeLane", manifest: "beekeeper.giantswarm.io_mergelanes.yaml",
			entry: func(i int) map[string]interface{} {
				return map[string]interface{}{
					"arrived": "2026-01-01T00:00:00Z",
					"by":      map[string]interface{}{"name": "test-agent"},
					"phase":   "waiting",
					"repo":    "test-org/test-repo",
					"pr":      int64(i + 1),
				}
			},
		},
	} {
		t.Run(kind.name, func(t *testing.T) {
			data, err := Files.ReadFile(kind.manifest)
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.UnmarshalStrict(data, &crd); err != nil {
				t.Fatal(err)
			}
			for _, version := range crd.Spec.Versions {
				t.Run(version.Name, func(t *testing.T) {
					if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
						t.Fatal("missing OpenAPI schema")
					}
					var schema apiextensions.JSONSchemaProps
					if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, &schema, nil); err != nil {
						t.Fatal(err)
					}
					validator, _, err := validation.NewSchemaValidator(&schema)
					if err != nil {
						t.Fatal(err)
					}
					// Literal boundaries keep the documented 20-entry contract pinned
					// even if source markers and generated YAML change together.
					for _, size := range []int{0, 20, 21} {
						t.Run(fmt.Sprintf("entries_%d", size), func(t *testing.T) {
							queue := make([]interface{}, size)
							for i := range queue {
								queue[i] = kind.entry(i)
							}
							payload := map[string]interface{}{
								"apiVersion": crd.Spec.Group + "/" + version.Name,
								"kind":       kind.name,
								"metadata":   map[string]interface{}{"name": "test-queue"},
								"spec":       map[string]interface{}{"installation": "test-installation"},
								"status":     map[string]interface{}{"queue": queue},
							}
							errs := validation.ValidateCustomResource(nil, payload, validator)
							if size <= 20 {
								if len(errs) != 0 {
									t.Fatalf("valid queue rejected: %v", errs)
								}
								return
							}
							if len(errs) != 1 || errs[0].Field != "status.queue" || errs[0].Type != field.ErrorTypeTooMany {
								t.Fatalf("want only TooMany at status.queue, got %v", errs)
							}
						})
					}
				})
			}
		})
	}
}
