package secret

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealSOPSCopy runs copy and compare with the real sops and a throwaway
// age key, where both are installed.
func TestRealSOPSCopy(t *testing.T) {
	for _, bin := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	const password, pwPath = "planted-Pass-real-3e8a51", "stringData.password"
	dir := t.TempDir()
	key := filepath.Join(dir, "age.key")
	if out, err := exec.Command("age-keygen", "-o", key).CombinedOutput(); err != nil { //nolint:gosec // the test's scratch key
		t.Fatalf("age-keygen: %v: %s", err, out)
	}
	recipient, err := exec.Command("age-keygen", "-y", key).Output() //nolint:gosec // the test's scratch key
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", key)
	rules := fmt.Sprintf("creation_rules:\n  - path_regex: '^team-./.*\\.sops\\.yaml$'\n    encrypted_regex: '^(data|stringData)$'\n    age: %s\n", strings.TrimSpace(string(recipient)))
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &Ops{Run: Exec, Fingerprint: func(v string) string { return fmt.Sprint(len(v)) }}
	ctx := context.Background()
	src := filepath.Join(dir, "team-a", "src.sops.yaml")
	dst := filepath.Join(dir, "team-b", "dst.sops.yaml")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(filepath.Dir(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc, err := parseDocument([]byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: team-a\nstringData:\n  password: " + password + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := o.encrypt(ctx, doc, src); err != nil {
		t.Fatal(err)
	}
	keys, err := o.CopyFile(ctx, Ref{File: src}, dst, "app-copy", "team-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != (Key{Name: pwPath, Bytes: len(password)}) {
		t.Errorf("keys = %+v", keys)
	}
	raw, err := os.ReadFile(dst) //nolint:gosec // the test's scratch file
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"name: app-copy", "namespace: team-b", "password: ENC["} {
		if !strings.Contains(string(raw), w) {
			t.Errorf("the copy lacks %q", w)
		}
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("the copy holds the value in plaintext")
	}
	vs, err := o.Compare(ctx, Ref{File: src, Path: pwPath}, Ref{File: dst, Path: pwPath})
	if err != nil || vs[0].State != Equal {
		t.Errorf("compare = %+v, %v", vs, err)
	}
	if _, err := o.CopyValue(ctx, Ref{File: dst, Path: pwPath}, Ref{File: dst, Path: "stringData.again"}); err != nil {
		t.Fatal(err)
	}
	if v, err := o.values(ctx, Ref{File: dst}); err != nil || v["stringData.again"] != password || v["metadata.name"] != "app-copy" {
		t.Errorf("after a copy into a path: %v (%d keys)", err, len(v))
	}
}
