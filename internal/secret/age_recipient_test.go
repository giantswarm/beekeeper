package secret

import (
	"context"
	"errors"
	"fmt"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/giantswarm/beekeeper/internal/state"
)

const agePasswordPath = "stringData.password"

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
			src := Ref{File: file, Path: agePasswordPath}
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
			if _, err := o.CopyValue(context.Background(), src, Ref{File: dst, Path: agePasswordPath}); err != nil {
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
			for _, w := range []string{"recipient " + r, `"` + AgeItemTitle(r) + `"`, `no installation item of Shared, Common holds it: 1 checked`} {
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

// A fresh Ops instance reuses public metadata, while rotation and removal
// invalidate the hint and resume the scan without reading any item twice.
func TestAgeRecipientCache(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	file := sopsFileAt(t, filepath.Join(t.TempDir(), "installations", "elver", "secret.yaml"), "", r)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &vaultsTools{vaults: map[string]map[string]map[string]string{
		ageVault:  {elverItem: {noteField: other.String()}, unrelatedItem: {ageItemField: "x"}},
		ageVaultB: {cedarItem: {noteField: id.String()}},
	}}
	lookup := func(want int, missing bool) {
		t.Helper()
		f.calls = nil
		o := &Ops{Run: f.run, Vault: ageVault, Token: "t", AgeVaults: []string{ageVaultB}, AgeCache: store}
		src := Ref{File: file, Path: agePasswordPath}
		_, err := o.Compare(context.Background(), src, Ref{Op: "op://" + ageVault + "/" + unrelatedItem + "/" + ageItemField})
		if missing {
			if !errors.Is(err, ErrNoAgeIdentity) {
				t.Fatalf("want missing identity: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		reads := 0
		for _, c := range f.calls {
			if strings.HasPrefix(c, "op item get ") {
				reads++
			}
		}
		if reads != want {
			t.Fatalf("read %d items, want %d", reads, want)
		}
	}
	lookup(2, false)
	lookup(1, false)
	raw, err := os.ReadFile(filepath.Join(store.Dir(), ageCacheFile))
	if err != nil {
		t.Fatal(err)
	}
	var locations map[string]ageItemLocation
	if _, err := store.ReadFile(ageCacheFile, &locations); err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[r] != (ageItemLocation{ageVaultB, cedarItem}) {
		t.Fatalf("unexpected cache metadata: %+v", locations)
	}
	if strings.Contains(string(raw), id.String()) || strings.Contains(string(raw), other.String()) || strings.Contains(string(raw), "AGE-SECRET-KEY-") {
		t.Fatal("identity leaked into cache")
	}
	// Rotate the cached item and move the identity to a different vault.
	f.vaults[ageVaultB][cedarItem][noteField] = other.String()
	f.vaults[ageVault][elverItem][noteField] = id.String()
	lookup(2, false)
	lookup(1, false)
	// Remove the cached item entirely; only the remaining item is read.
	delete(f.vaults[ageVault], elverItem)
	lookup(1, true)
	locations = nil
	if _, err := store.ReadFile(ageCacheFile, &locations); err != nil {
		t.Fatal(err)
	}
	if len(locations) != 0 {
		t.Fatal("stale entry retained")
	}
	// A cache entry cannot grant access to an unconfigured vault.
	if err := store.WriteFile(ageCacheFile, map[string]ageItemLocation{r: {"unconfigured", cedarItem}}); err != nil {
		t.Fatal(err)
	}
	lookup(1, true)
	for _, c := range f.calls {
		if strings.Contains(c, "unconfigured") {
			t.Fatal("read a vault outside configuration")
		}
	}
	// Damaged metadata is a cache miss, never a decryption failure.
	if err := os.WriteFile(filepath.Join(store.Dir(), ageCacheFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.vaults[ageVaultB][cedarItem][noteField] = id.String()
	lookup(1, false)
}

func TestAgeRecipientRefusalCountsItems(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	file := sopsFile(t, "", id.Recipient().String())
	items := map[string]map[string]string{}
	for i := range 120 {
		items[fmt.Sprintf("installation-%03d.agekey", i)] = map[string]string{noteField: other.String()}
	}
	f := &vaultsTools{vaults: map[string]map[string]map[string]string{ageVault: items}}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t"}
	_, err = o.values(context.Background(), Ref{File: file})
	if !errors.Is(err, ErrNoAgeIdentity) {
		t.Fatalf("want missing identity: %v", err)
	}
	msg := strings.TrimPrefix(err.Error(), file)
	if !strings.Contains(msg, id.Recipient().String()) || !strings.Contains(msg, "120 checked") || strings.Contains(msg, "\n") || strings.Contains(msg, "installation-000") || len(msg) > 400 {
		t.Fatalf("unexpected refusal: %s", msg)
	}
}
