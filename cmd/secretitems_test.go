package cmd

import (
	"strings"
	"testing"
)

func TestSecretItemsKeyPattern(t *testing.T) {
	a, tools, _ := secretApp(t)
	tools.Vault["op://Shared/environment/notesPlain"] = "SERVICE_API_KEY=" + secretValue + "\nOTHER=unrelated"
	out, err := runSecret(a, "items", "op://Shared", "--env-keys", "--fields", "--key-pattern", "api_key")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notesPlain#SERVICE_API_KEY") || strings.Contains(out, "#OTHER") || strings.Contains(out, "db ") {
		t.Fatalf("unexpected listing: %s", out)
	}
	noSecret(t, "items", out)
}
