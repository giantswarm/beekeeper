package secret

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRealSOPSRevealUnset reveals configuration from and unsets keys in a
// Helm values patch (*.yaml.patch, a name sops types as binary) with the
// real sops and a throwaway age key, where both are installed: the file
// keeps its recipient, gets a new MAC, and decrypts without the keys.
func TestRealSOPSRevealUnset(t *testing.T) {
	for _, bin := range []string{sopsBin, ageKeygen} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	const secretValue = "planted-Client-real-4d0a7c"
	dir := t.TempDir()
	key := filepath.Join(dir, "age.key")
	if out, err := exec.Command(ageKeygen, "-o", key).CombinedOutput(); err != nil { //nolint:gosec // the test's scratch key
		t.Fatalf("age-keygen: %v: %s", err, out)
	}
	recipient, err := exec.Command(ageKeygen, "-y", key).Output() //nolint:gosec // the test's scratch key
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", key)
	rules := fmt.Sprintf("creation_rules:\n  - path_regex: '.*\\.yaml\\.patch$'\n    age: %s\n", strings.TrimSpace(string(recipient)))
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &Ops{Run: Exec}
	ctx := context.Background()
	patch := filepath.Join(dir, "secret-values.yaml.patch")
	doc, err := parseDocument([]byte("oidc:\n  extraStaticClients:\n    - id: kagent\n      secret: " + secretValue +
		"\n      redirectURIs:\n        - https://kagent.example.org/callback\n  staticClients:\n    muster:\n      id: muster\n      secret: " + secretValue + "\nissuer: https://dex.example.org\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := o.encrypt(ctx, doc, patch); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(patch) //nolint:gosec // the test's scratch file
	fs, err := o.Reveal(ctx, patch, []string{"oidc.extraStaticClients.0.id", "oidc.extraStaticClients.0.redirectURIs"})
	if err != nil || len(fs) != 2 || fs[0].Value != "kagent" || fs[1].Value != "https://kagent.example.org/callback" {
		t.Fatalf("reveal = %+v, %v", fs, err)
	}
	res, err := o.Unset(ctx, patch, []string{"oidc.extraStaticClients", "oidc.staticClients.muster.secret"}, true)
	if err != nil || !res.Written {
		t.Fatalf("unset = %+v, %v", res, err)
	}
	if want := []string{"issuer", "oidc.staticClients.muster.id"}; !slices.Equal(res.Kept, want) {
		t.Errorf("kept = %v", res.Kept)
	}
	after, _ := os.ReadFile(patch) //nolint:gosec // the test's scratch file
	if !strings.Contains(string(after), strings.TrimSpace(string(recipient))) {
		t.Error("the file lost its recipient")
	}
	if mac := func(raw []byte) string {
		_, m, _ := strings.Cut(string(raw), "mac: ")
		m, _, _ = strings.Cut(m, "\n")
		return m
	}; mac(before) == mac(after) {
		t.Error("the MAC stayed")
	}
	got, err := o.decrypt(ctx, patch)
	if err != nil {
		t.Fatal(err)
	}
	leaves := got.leaves()
	if _, ok := leaves["oidc.extraStaticClients.0.id"]; ok || leaves["oidc.staticClients.muster.id"] != "muster" || len(leaves) != 2 {
		t.Errorf("after unset: %v", sortedKeys(leaves))
	}
	if res, err := o.Unset(ctx, patch, []string{"oidc.extraStaticClients"}, true); err != nil || res.Written {
		t.Errorf("a second run = %+v, %v", res, err)
	}
}
