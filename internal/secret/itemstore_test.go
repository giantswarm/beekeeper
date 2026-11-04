package secret_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	itemVault     = "Shared"
	itemTitle     = "example-app"
	itemRef       = "op://" + itemVault + "/" + itemTitle
	appIDKey      = "app-id"
	concealedType = "CONCEALED"
	plainType     = "STRING"
)

// A store into an item writes every field in one op call, the
// configuration as strings and the secrets concealed, creating the item
// and keeping its other fields, and answers each field's length and
// fingerprint; it refuses what no item takes before op runs.
func TestStoreFieldsWritesTheItemInOneCall(t *testing.T) {
	tools := secrettest.New(map[string]string{itemRef + "/notes": "kept"})
	o := &secret.Ops{Run: tools.Run, Vault: itemVault, Token: saToken, Fingerprint: func(v string) string { return fmt.Sprintf("fp-%d", len(v)) }}
	ctx := context.Background()
	fields := []secret.ItemField{
		{Label: appIDKey, Value: "4242", Plain: true},
		{Label: clientSecretKey, Value: storedValue},
		{Label: "private-key", Value: madeUpKey},
	}
	out, err := o.StoreFields(ctx, itemVault, itemTitle, fields)
	if err != nil || len(out) != 3 {
		t.Fatalf("StoreFields = %+v, %v", out, err)
	}
	for i, f := range fields {
		if want := (secret.Stored{Ref: itemRef + "/" + f.Label, Bytes: len(f.Value), Fingerprint: fmt.Sprintf("fp-%d", len(f.Value))}); out[i] != want {
			t.Errorf("stored[%d] = %+v, want %+v", i, out[i], want)
		}
		if tools.Vault[itemRef+"/"+f.Label] != f.Value {
			t.Errorf("the vault holds %q at %s", tools.Vault[itemRef+"/"+f.Label], f.Label)
		}
	}
	if tools.Types[itemRef+"/app-id"] != plainType || tools.Types[itemRef+"/"+clientSecretKey] != concealedType || tools.Types[itemRef+"/private-key"] != concealedType {
		t.Errorf("the field types: %q", tools.Types)
	}
	if tools.Vault[itemRef+"/notes"] != "kept" {
		t.Error("the item's other field is gone")
	}
	lines := strings.Join(tools.Calls, "\n")
	if strings.Contains(lines, storedValue) || strings.Contains(lines, "planted-key") || strings.Contains(lines, "4242") {
		t.Errorf("a command line carries a value:\n%s", lines)
	}
	if writes := strings.Count(lines, "op item edit") + strings.Count(lines, "op item create"); writes != 1 {
		t.Errorf("%d op writes, want one:\n%s", writes, lines)
	}
	calls := len(tools.Calls)
	for _, bad := range []struct {
		vault  string
		fields []secret.ItemField
		want   string
	}{
		{itemVault, nil, "no field"},
		{itemVault, []secret.ItemField{{Label: "", Value: "v"}}, "a field's label names it"},
		{itemVault, []secret.ItemField{{Label: "a/b", Value: "v"}}, "a field's label names it"},
		{itemVault, []secret.ItemField{{Label: "x", Value: ""}}, "the value is empty"},
		{itemVault, []secret.ItemField{{Label: "x", Value: strings.Repeat("x", secret.MaxStoreBytes+1)}}, "more than a field takes"},
		{itemVault, []secret.ItemField{{Label: "a", Value: strings.Repeat("x", secret.MaxStoreBytes)}, {Label: "b", Value: strings.Repeat("x", secret.MaxStoreBytes)},
			{Label: "c", Value: strings.Repeat("x", secret.MaxStoreBytes)}, {Label: "d", Value: strings.Repeat("x", secret.MaxStoreBytes)}, {Label: "e", Value: "x"}}, "more than one store into an item takes"},
		{"Other", []secret.ItemField{{Label: "x", Value: "v"}}, "only the shared vault"},
	} {
		if _, err := o.StoreFields(ctx, bad.vault, itemTitle, bad.fields); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s with %d fields: %v, want %q", bad.vault, len(bad.fields), err, bad.want)
		}
	}
	if len(tools.Calls) != calls {
		t.Errorf("a refused store ran op: %q", tools.Calls[calls:])
	}
	for _, bad := range []string{"op://Shared", "op://Shared/a/b", "Shared/a", ""} {
		if _, _, err := secret.ParseItem(bad); err == nil {
			t.Errorf("ParseItem(%q) took an item", bad)
		}
	}
	if v, i, err := secret.ParseItem(itemRef); err != nil || v != itemVault || i != itemTitle {
		t.Errorf("ParseItem = %q, %q, %v", v, i, err)
	}
}

// Adding the first custom secret to a text-only item must produce the same
// valid Password template as creation, without replacing an existing password.
func TestStoreFirstSecretAfterText(t *testing.T) {
	const original = "fixture-original-value"
	const next = "fixture-new-secret"
	const existingPassword = "fixture-existing-password"
	for _, api := range []string{"StoreValue", "StoreFields"} {
		for _, label := range []string{"username", "notesPlain", appIDKey, storePasswordAttribute} {
			if api == "StoreValue" && label == appIDKey {
				continue // StoreValue has no plain custom field option.
			}
			t.Run(api+"/"+label, func(t *testing.T) {
				tools := secrettest.New(nil)
				o := &secret.Ops{Run: tools.Run, Vault: itemVault, Token: saToken, Fingerprint: func(string) string { return "fp" }}
				ctx := context.Background()
				store := func(fields []secret.ItemField) error {
					if api == "StoreFields" {
						_, err := o.StoreFields(ctx, itemVault, itemTitle, fields)
						return err
					}
					_, err := o.StoreValue(ctx, secret.Ref{Op: itemRef + "/" + fields[0].Label}, fields[0].Value)
					return err
				}
				value, wantPassword := original, next
				if label == storePasswordAttribute {
					value, wantPassword = existingPassword, existingPassword
				}
				if err := store([]secret.ItemField{{Label: label, Value: value, Plain: label != storePasswordAttribute}}); err != nil {
					t.Fatal(err)
				}
				if label != storePasswordAttribute && tools.Vault[itemRef+"/password"] != "" {
					t.Fatal("text-only item has a nonempty password")
				}
				if err := store([]secret.ItemField{{Label: clientSecretKey, Value: next}}); err != nil {
					t.Fatal(err)
				}
				for field, want := range map[string]string{label: value, storePasswordAttribute: wantPassword, clientSecretKey: next} {
					ref := itemRef + "/" + field
					if tools.Vault[ref] != want {
						t.Errorf("field %s was not preserved or populated", field)
					}
					wantType := concealedType
					if field != storePasswordAttribute && field != clientSecretKey {
						wantType = plainType
					}
					if tools.Types[ref] != wantType {
						t.Errorf("field %s type = %s, want %s", field, tools.Types[ref], wantType)
					}
				}
				lines := strings.Join(tools.Calls, "\n")
				for _, v := range []string{original, next, existingPassword} {
					if strings.Contains(lines, v) {
						t.Error("command line contains a value")
					}
				}
				if strings.Count(lines, "op item create") != 1 || strings.Count(lines, "op item edit") != 1 {
					t.Error("expected one creation followed by one edit")
				}
			})
		}
	}
}
