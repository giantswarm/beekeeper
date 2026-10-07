package secret

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// TestRealSOPSAgeIdentity decrypts with the real sops a file whose identity
// only the vault holds: refused before sops without the mapping, decrypted
// with it.
func TestRealSOPSAgeIdentity(t *testing.T) {
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops is not installed")
	}
	id := isolateAge(t)
	dir := t.TempDir()
	rules := fmt.Sprintf("creation_rules:\n  - path_regex: '\\.sops\\.yaml$'\n    age: %s\n", id.Recipient())
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := parseDocument([]byte("stringData:\n  password: planted-Pass-age-71c2\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	file := filepath.Join(dir, "app.sops.yaml")
	if err := (&Ops{Run: Exec}).encrypt(ctx, doc, file); err != nil {
		t.Fatal(err)
	}
	vault := func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "op" {
			return []byte(id.String()), nil
		}
		return Exec(ctx, dir, env, stdin, name, args...)
	}
	o := &Ops{Run: vault, Vault: ageVault, Token: "t", Fingerprint: func(v string) string { return fmt.Sprint(len(v)) }}
	if _, err := o.Fingerprints(ctx, Ref{File: file}); !errors.Is(err, ErrNoAgeIdentity) {
		t.Fatalf("without the mapping: %v", err)
	}
	o.Ages = []AgeIdentity{{Recipient: id.Recipient().String(), Ref: ageRef}}
	ps, err := o.Fingerprints(ctx, Ref{File: file})
	if err != nil || len(ps) != 1 || ps[0].Fingerprint != fmt.Sprint(len("planted-Pass-age-71c2")) {
		t.Fatalf("with the mapping: %+v, %v", ps, err)
	}
}

// TestRealSOPSSkeleton fills a plaintext Secret skeleton with the real
// sops: metadata stays readable, the generated value is encrypted.
func TestRealSOPSSkeleton(t *testing.T) {
	for _, bin := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
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
	rules := fmt.Sprintf("creation_rules:\n  - path_regex: '\\.sops\\.yaml$'\n    encrypted_regex: '^(data|stringData)$'\n    age: %s\n", strings.TrimSpace(string(recipient)))
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "apps", "secret-s3.sops.yaml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	skel := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: app-s3\n  namespace: app\nstringData:\n"
	if err := os.WriteFile(file, []byte(skel), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &Ops{Run: Exec, Fingerprint: func(v string) string { return fmt.Sprint(len(v)) }}
	ctx := context.Background()
	if _, err := o.Set(ctx, Ref{File: file, Path: "stringData.secretKey"}, SetOptions{Length: 32, Charset: "alnum"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the test's scratch file
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"kind: Secret", "name: app-s3", "namespace: app", "secretKey: ENC[", "sops:"} {
		if !strings.Contains(string(raw), w) {
			t.Errorf("the SOPS file lacks %q", w)
		}
	}
	v, err := o.values(ctx, Ref{File: file})
	if err != nil || len(v["stringData.secretKey"]) != 32 {
		t.Fatalf("decrypted: %v (%d bytes)", err, len(v["stringData.secretKey"]))
	}
	if strings.Contains(string(raw), v["stringData.secretKey"]) {
		t.Fatal("the file holds the value in plaintext")
	}
	// A bare key on a Secret goes under stringData, where the rule encrypts it.
	if _, err := o.Set(ctx, Ref{File: file, Path: "default"}, SetOptions{Length: 32, Charset: "alnum"}); err != nil {
		t.Fatal(err)
	}
	if raw, err = os.ReadFile(file); err != nil { //nolint:gosec // the test's scratch file
		t.Fatal(err)
	}
	if v, err = o.values(ctx, Ref{File: file}); err != nil || len(v["stringData.default"]) != 32 {
		t.Fatalf("decrypted: %v (%d bytes)", err, len(v["stringData.default"]))
	}
	if !strings.Contains(string(raw), "default: ENC[") || strings.Contains(string(raw), v["stringData.default"]) {
		t.Fatal("stringData.default is not encrypted")
	}
}

// sopsBin is the real sops.
const sopsBin = "sops"

// TestRealSOPSCopyValues writes two vault fields into a new Secret with the
// real sops while no age identity is reachable, then decrypts the file with
// the throwaway recipient's identity to the two keys.
func TestRealSOPSCopyValues(t *testing.T) {
	if _, err := exec.LookPath(sopsBin); err != nil {
		t.Skip("sops is not installed")
	}
	const idRef, secretRef = "op://Shared/oauth/username", "op://Shared/oauth/credential" //nolint:gosec // vault references, no value
	const clientID, clientSecret = "planted-Client-Id-4b2e", "planted-Client-Secret-a91f07"
	id := isolateAge(t)
	dir := t.TempDir()
	rules := fmt.Sprintf("creation_rules:\n  - path_regex: '\\.sops\\.yaml$'\n    encrypted_regex: '^(data|stringData)$'\n    age: %s\n", id.Recipient())
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "op" {
			return []byte(map[string]string{idRef: clientID, secretRef: clientSecret}[args[len(args)-1]]), nil
		}
		if name == sopsBin && slices.Contains(args, "decrypt") {
			t.Errorf("copy decrypted: sops %s", strings.Join(args, " "))
		}
		return Exec(ctx, dir, env, stdin, name, args...)
	}
	o := &Ops{Run: vault, Vault: "Shared", Token: "t"}
	file := filepath.Join(dir, "oauth.sops.yaml")
	pairs := []Pair{{Src: Ref{Op: idRef}, Path: "client-id"}, {Src: Ref{Op: secretRef}, Path: "client-secret"}}
	keys, err := o.CopyValues(context.Background(), pairs, file, &NewSecret{Name: "oauth", Namespace: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != (Key{Name: "stringData.client-id", Bytes: len(clientID)}) || keys[1] != (Key{Name: "stringData.client-secret", Bytes: len(clientSecret)}) {
		t.Errorf("keys = %+v", keys)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the test's scratch file
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), clientID) || strings.Contains(string(raw), clientSecret) {
		t.Fatal("the file holds a value in plaintext")
	}
	t.Setenv(envAgeKey, id.String())
	out, err := Exec(context.Background(), "", nil, nil, sopsBin, "decrypt", "--output-type", "yaml", file)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseDocument(out)
	if err != nil {
		t.Fatal(err)
	}
	if v := doc.leaves(); v["stringData.client-id"] != clientID || v["stringData.client-secret"] != clientSecret || v["metadata.name"] != "oauth" {
		t.Errorf("decrypted %d keys, not the two values", len(v))
	}
}
