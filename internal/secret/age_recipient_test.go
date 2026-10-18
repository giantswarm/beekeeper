package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// A SOPS file's identity is found by the recipient its sops.age block
// names, at any path: a bare path, a management cluster's directory without
// a .sops.yaml above it, or under another installation's directory.
func TestCompareAndCopyFindTheIdentityByRecipient(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	dir := t.TempDir()
	for name, path := range map[string]string{
		"a bare path":                    filepath.Join(dir, "bare", "secret.yaml"),
		"a management cluster directory": filepath.Join(dir, "management-clusters", "cedar", "secrets", "dex.sops.yaml"),
		"another installation's path":    filepath.Join(dir, "installations", "elver", "secret.yaml"),
	} {
		t.Run(name, func(t *testing.T) {
			file := sopsFileAt(t, path, "", r)
			dstDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dstDir, ".sops.yaml"), []byte("creation_rules:\n  - age: "+r+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			dst := sopsFileAt(t, filepath.Join(dstDir, "copy.yaml"), "", r)
			f := &vaultsTools{vaults: map[string]map[string]map[string]string{
				ageVault:  {unrelatedItem: {ageItemField: "x"}, elverItem: {noteField: other.String()}},
				ageVaultB: {"alder.agekey": {noteField: other.String()}, cedarItem: {"": "# public key: " + r + "\n" + id.String() + "\n"}},
			}}
			o := &Ops{Run: f.run, Vault: ageVault, Token: "t", AgeVaults: []string{ageVaultB}}
			src := Ref{File: file, Path: "stringData.password"}
			vs, err := o.Compare(context.Background(), src, src)
			if err != nil {
				t.Fatalf("compare: %v", err)
			}
			if len(vs) != 1 || vs[0].State != Equal {
				t.Errorf("compare = %+v", vs)
			}
			if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
				t.Errorf("sops did not get the identity of the file's recipient (%d entries)", len(f.env))
			}
			if _, err := o.CopyValue(context.Background(), src, Ref{File: dst, Path: "stringData.password"}); err != nil {
				t.Fatalf("copy: %v", err)
			}
			rs, err := o.Recipients(context.Background(), file)
			if err != nil || len(rs) != 1 || rs[0].Missing() {
				t.Errorf("recipients = %+v, %v", rs, err)
			}
		})
	}
}

// A refusal names the recipient the vaults hold no identity for, never the
// path's layout.
func TestNoIdentityForTheRecipientNamesIt(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	for name, path := range map[string]string{
		"a bare path":            filepath.Join(t.TempDir(), "secret.yaml"),
		"an installation's path": filepath.Join(t.TempDir(), "installations", "cedar", "secret.yaml"),
	} {
		t.Run(name, func(t *testing.T) {
			file := sopsFileAt(t, path, "", r)
			f := &vaultsTools{vaults: map[string]map[string]map[string]string{
				ageVault:  {},
				ageVaultB: {elverItem: {noteField: other.String()}},
			}}
			o := &Ops{Run: f.run, Vault: ageVault, Token: "t", AgeVaults: []string{ageVaultB}}
			_, err := o.values(context.Background(), Ref{File: file})
			if !errors.Is(err, ErrNoAgeIdentity) || strings.Contains(err.Error(), "\n") {
				t.Fatalf("err = %v, want one line with ErrNoAgeIdentity", err)
			}
			msg := strings.ReplaceAll(err.Error(), file, "<file>")
			for _, w := range []string{"recipient " + r, `"` + AgeItemTitle(r) + `"`, `"elver.agekey" of Common`} {
				if !strings.Contains(msg, w) {
					t.Errorf("the error lacks %q: %v", w, err)
				}
			}
			for _, nw := range []string{"installations/", "cedar", "'s recipient"} {
				if strings.Contains(msg, nw) {
					t.Errorf("the error names the path layout (%q): %v", nw, err)
				}
			}
			for _, c := range f.calls {
				if strings.HasPrefix(c, "sops ") {
					t.Errorf("sops ran without an identity: %q", f.calls)
				}
			}
		})
	}
}
