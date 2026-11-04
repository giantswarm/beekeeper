package secret_test

import (
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestStoreAddsFirstSecretAfterText(t *testing.T) {
	const (
		itemRef       = "op://Shared/widget/"
		firstValue    = "fixture-original-value"
		secretValue   = "fixture-new-secret"
		concealedType = "CONCEALED"
		passwordField = "password"
	)
	for _, tc := range []struct {
		field string
		batch bool
	}{
		{field: "app-id", batch: true},
		{field: "username"},
		{field: "notesPlain"},
		{field: passwordField},
	} {
		t.Run(tc.field, func(t *testing.T) {
			tools := secrettest.New(nil)
			ops := &secret.Ops{Run: tools.Run, Vault: "Shared", Token: "fixture-service-account"}
			ctx := context.Background()
			store := func(label, value string, plain bool) {
				t.Helper()
				if tc.batch {
					_, err := ops.StoreFields(ctx, "Shared", "widget", []secret.ItemField{{Label: label, Value: value, Plain: plain}})
					if err != nil {
						t.Fatal(err)
					}
				} else if _, err := ops.StoreValue(ctx, secret.Ref{Op: itemRef + label}, value); err != nil {
					t.Fatal(err)
				}
			}
			store(tc.field, firstValue, tc.batch)
			store("client-secret", secretValue, false)
			if tools.Vault[itemRef+tc.field] != firstValue || tools.Vault[itemRef+"client-secret"] != secretValue {
				t.Error("the store did not preserve both fields")
			}
			wantPassword, wantFirstType := secretValue, "STRING"
			if tc.field == passwordField {
				wantPassword, wantFirstType = firstValue, concealedType
			}
			if tools.Vault[itemRef+passwordField] != wantPassword {
				t.Error("the built-in password was not filled or its existing value changed")
			}
			if tools.Types[itemRef+tc.field] != wantFirstType || tools.Types[itemRef+passwordField] != concealedType || tools.Types[itemRef+"client-secret"] != concealedType {
				t.Errorf("field types = %v", tools.Types)
			}
			if lines := strings.Join(tools.Calls, "\n"); strings.Contains(lines, firstValue) || strings.Contains(lines, secretValue) {
				t.Error("a command line carries a value")
			}
		})
	}
}
