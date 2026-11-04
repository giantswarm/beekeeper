package secrettest_test

import (
	"context"
	"maps"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func TestItemSectionsSurviveListingAndEdit(t *testing.T) {
	const itemRef = "op://Shared/widget"
	wantVault := map[string]string{
		itemRef + "/first/notes":    "FIRST=one\n",
		itemRef + "/second/notes":   "SECOND=two\n",
		itemRef + "/first/username": "fixture-concealed-value",
		itemRef + "/notesPlain":     "plain notes",
	}
	tools := secrettest.New(maps.Clone(wantVault))
	tools.Types = map[string]string{
		itemRef + "/first/notes":  "STRING",
		itemRef + "/second/notes": "STRING",
	}
	ops := &secret.Ops{Run: tools.Run, Vault: "Shared", Token: "fixture-service-account"}
	ctx := context.Background()
	check := func() {
		t.Helper()
		items, err := ops.Items(ctx, "Shared", secret.ItemsOptions{Fields: true, EnvKeys: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("listing returned %d items, want one", len(items))
		}
		if len(items[0].Fields) != len(wantVault) {
			t.Errorf("listing returned %d fields, want %d", len(items[0].Fields), len(wantVault))
		}
		fields := map[string]secret.FieldInfo{}
		for _, field := range items[0].Fields {
			fields[field.Section+"/"+field.Label] = field
		}
		for _, section := range []string{"first", "second"} {
			field := fields[section+"/notes"]
			if field.Label != "notes" || field.Section != section || field.Type != "STRING" || field.Length != len(wantVault[itemRef+"/"+section+"/notes"]) {
				t.Errorf("%s notes metadata = %+v", section, field)
			}
		}
		if field := fields["first/username"]; field.Label != "username" || field.Section != "first" || field.Type != "CONCEALED" || field.Purpose != "" {
			t.Errorf("a section's custom username field = %+v", field)
		}
		keys := map[string]secret.EnvKey{}
		for _, key := range items[0].EnvKeys {
			keys[key.Field] = key
		}
		wantKeys := map[string]secret.EnvKey{
			"first/notes":  {Field: "first/notes", Key: "FIRST", Length: 3},
			"second/notes": {Field: "second/notes", Key: "SECOND", Length: 3},
		}
		if len(items[0].EnvKeys) != len(wantKeys) || !maps.Equal(keys, wantKeys) {
			t.Errorf("env keys = %+v, want %+v", keys, wantKeys)
		}
	}
	check()

	if _, err := ops.StoreFields(ctx, "Shared", "widget", []secret.ItemField{{Label: "credential", Value: "fixture-new-value"}}); err != nil {
		t.Fatal(err)
	}
	wantVault[itemRef+"/credential"] = "fixture-new-value"
	if !maps.Equal(tools.Vault, wantVault) {
		t.Error("editing the item changed its other references or values")
	}
	check()
}
