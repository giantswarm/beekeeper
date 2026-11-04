package secrettest_test

import (
	"context"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestItemCategorySurvivesCreateAndEdit(t *testing.T) {
	tools := secrettest.New(nil)
	ops := &secret.Ops{Run: tools.Run, Vault: "Shared", Token: "fixture-service-account"}
	ctx := context.Background()

	for _, value := range []string{"fixture-first-value", "fixture-second-value"} {
		if _, err := ops.StoreFields(ctx, "Shared", "widget", []secret.ItemField{{Label: "credential", Value: value}}); err != nil {
			t.Fatal(err)
		}
		items, err := ops.Items(ctx, "Shared", secret.ItemsOptions{Match: "password"})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("category match returned %d items, want one", len(items))
		}
		if items[0].Title != "widget" || items[0].Category != "PASSWORD" {
			t.Errorf("item metadata = %+v", items[0])
		}
	}
}
