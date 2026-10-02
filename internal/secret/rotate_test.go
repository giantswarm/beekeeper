package secret_test

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	issued    = "planted-Issued-0b6f2d8e41" //nolint:gosec // a planted test value
	unrelated = "planted-Other-3a9c5e7d10"
)

// carriersRepo is a scratch repository whose a.sops.yaml carries the
// vault's value raw and in base64 beside an unrelated one, and whose
// b.sops.yaml carries none of it.
func carriersRepo(t *testing.T, old string) (files []string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, aFile)
	plain := "stringData:\n  password: " + old + "\n  other: " + unrelated + "\ndata:\n  password: " +
		base64.StdEncoding.EncodeToString([]byte(old)) + "\n"
	b := filepath.Join(dir, "b.sops.yaml")
	for f, p := range map[string]string{a: plain, b: "stringData:\n  other: " + unrelated + "\n"} {
		if err := os.WriteFile(f, secrettest.Encrypt(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{a, b}
}

func index(t *testing.T) *guard.Index {
	t.Helper()
	ix, err := guard.OpenIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// leaf is the value at a path of a file the fake encrypted.
func leaf(t *testing.T, tools *secrettest.Tools, file, path string) string {
	t.Helper()
	for l := range strings.Lines(decrypted(t, tools, file)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), ": "); ok && k == path {
			return v
		}
	}
	return ""
}

func TestRotateGeneratedWritesTheVaultFirstThenEveryCarrier(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	files := carriersRepo(t, password)
	ix := index(t)
	ref, _ := secret.ParseRef(vaultRef)
	rot, err := ops(tools).RotateGenerated(context.Background(), ref, files, ix, 32, "alnum")
	if err != nil {
		t.Fatal(err)
	}
	noValue(t, "rotate", rot)
	nv := tools.Vault[vaultRef]
	if nv == password || len(nv) != 32 {
		t.Fatalf("the vault holds no new value of 32 characters (%d)", len(nv))
	}
	want := []secret.Carrier{{Ref: files[0] + "#data.password", Base64: true}, {Ref: files[0] + "#stringData.password"}}
	if !slices.Equal(rot.Carriers, want) {
		t.Errorf("carriers = %+v, want %+v", rot.Carriers, want)
	}
	text := decrypted(t, tools, files[0])
	if strings.Contains(text, password) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(password))) {
		t.Error("a carrier still holds the old value")
	}
	if !strings.Contains(text, "password: "+nv) || !strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(nv))) ||
		!strings.Contains(text, "other: "+unrelated) {
		t.Errorf("a.sops.yaml carries the new value raw and in base64, the unrelated one kept:\n%s", text)
	}
	vault := slices.IndexFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op item edit") })
	sops := slices.IndexFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "sops") && strings.Contains(c, "encrypt") })
	if vault < 0 || sops < 0 || vault > sops {
		t.Errorf("the vault is written first: %q", tools.Calls)
	}
	for _, c := range tools.Calls {
		if strings.Contains(c, files[1]) && strings.Contains(c, "encrypt") {
			t.Errorf("b.sops.yaml carries nothing and is rewritten: %s", c)
		}
	}
	if fp, _ := ix.ValueFingerprint(vaultRef); fp != ix.Fingerprint(nv) {
		t.Error("the index does not hold the new value")
	}
}

func TestRotateIssuedCarriesTheVaultsNewValue(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	files := carriersRepo(t, password)
	ix := index(t)
	ix.Add(vaultRef, password)
	ref, _ := secret.ParseRef(vaultRef)
	o := ops(tools)
	if _, err := o.RotateIssued(context.Background(), ref, files, ix); err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Fatalf("a vault that still holds the old value: %v", err)
	}
	tools.Vault[vaultRef] = issued
	rot, err := o.RotateIssued(context.Background(), ref, files, ix)
	if err != nil {
		t.Fatal(err)
	}
	noValue(t, "rotate", rot)
	if len(rot.Carriers) != 2 || leaf(t, tools, files[0], "password") != issued {
		t.Errorf("carriers = %+v, a.sops.yaml:\n%s", rot.Carriers, decrypted(t, tools, files[0]))
	}
	if slices.ContainsFunc(tools.Calls, func(c string) bool { return strings.HasPrefix(c, "op item") }) {
		t.Errorf("an issued value is not written to the vault: %q", tools.Calls)
	}
	if _, err := o.RotateIssued(context.Background(), ref, files, index(t)); err == nil || strings.Contains(err.Error(), issued) {
		t.Errorf("no recorded fingerprint: %v", err)
	}
}

func TestRotateChangesNothingWhenAFileCannotBeRead(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: password})
	files := carriersRepo(t, password)
	files = append(files, filepath.Join(t.TempDir(), "gone.sops.yaml"))
	ref, _ := secret.ParseRef(vaultRef)
	_, err := ops(tools).RotateGenerated(context.Background(), ref, files, index(t), 32, "alnum")
	if err == nil || !strings.Contains(err.Error(), "nothing rotated") {
		t.Fatalf("err = %v", err)
	}
	if tools.Vault[vaultRef] != password || leaf(t, tools, files[0], "password") != password {
		t.Error("a rotation that cannot read every file changed something")
	}
}

func TestRotateTakesTheVaultFieldOnly(t *testing.T) {
	tools := secrettest.New(nil)
	ref, _ := secret.ParseRef(aFile + "#" + pwPath)
	if _, err := ops(tools).RotateGenerated(context.Background(), ref, nil, index(t), 32, "alnum"); err == nil {
		t.Error("a SOPS path is the carrier, not the source")
	}
}

func TestRotatePlatformRunsTheManagersRotation(t *testing.T) {
	var calls []string
	run := func(_ context.Context, _ string, _ []string, _ io.Reader, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return []byte("action rotate-1: pull request opened, token " + "ghp_" + strings.Repeat("a1B2", 9) + "\n"), nil
	}
	o := &secret.Ops{Run: run}
	r, err := secret.ParsePlatformRef("platform://hazel/muster/hazel-muster-valkey-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.RotatePlatform(context.Background(), r, " ", false); err == nil {
		t.Error("a rotation without a reason")
	}
	out, err := o.RotatePlatform(context.Background(), r, "leaked", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "platformctl installation reconcile hazel muster --dry-run --reason leaked --rotate hazel-muster-valkey-password"
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if strings.Contains(out, "ghp_") {
		t.Errorf("platformctl's answer is not redacted: %s", out)
	}
	if _, err := secret.ParsePlatformRef("platform://hazel/muster"); err == nil {
		t.Error("a platform reference names installation, capability and name")
	}
}
