package secret

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
)

const (
	ageVault = "Shared"
	ageRef   = "op://Shared/age/identity"
	// ageDoc is a decrypted document the fake sops answers.
	ageDoc = "stringData:\n  password: x\n"
	// itemOp is op's item command, whose listing is the fakes' metadata.
	itemOp = "item"
	// opItemList is the vault's item listing, the one call that checks an
	// item exists.
	opItemList = "op item list --vault Shared --format json"
)

// ageTools is a fake sops and op: op answers the identity, sops the
// document, and every call is recorded with its environment.
type ageTools struct {
	identity string
	calls    []string
	env      []string
}

func (f *ageTools) run(_ context.Context, _ string, env []string, _ io.Reader, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "op" {
		return []byte(f.identity), nil
	}
	f.env = env
	return []byte(ageDoc), nil
}

// isolateAge clears sops' identity sources and returns a fresh identity.
func isolateAge(t *testing.T) *age.X25519Identity {
	t.Helper()
	for _, e := range append([]string{envAgeKey, envAgeKeyFile}, opaqueAgeEnv...) {
		// unset, not empty: sops runs an empty SOPS_AGE_KEY_CMD
		t.Setenv(e, "")
		if err := os.Unsetenv(e); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sopsFile writes a SOPS file encrypted to recipients, plus extra metadata.
func sopsFile(t *testing.T, extra string, recipients ...string) string {
	t.Helper()
	return sopsFileAt(t, filepath.Join(t.TempDir(), "app.sops.yaml"), extra, recipients...)
}

// sopsFileAt writes a SOPS file encrypted to recipients, plus extra
// metadata, at path.
func sopsFileAt(t *testing.T, path, extra string, recipients ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("stringData:\n  password: ENC[AES256_GCM,data:x,type:str]\nsops:\n")
	if len(recipients) > 0 {
		b.WriteString("  age:\n")
		for _, r := range recipients {
			b.WriteString("    - recipient: " + r + "\n      enc: |\n        -----BEGIN AGE ENCRYPTED FILE-----\n")
		}
	}
	b.WriteString(extra)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNoAgeIdentityFailsBeforeSOPS(t *testing.T) {
	id := isolateAge(t)
	f := &ageTools{}
	o := &Ops{Run: f.run}
	file := sopsFile(t, "", id.Recipient().String())
	_, err := o.values(context.Background(), Ref{File: file})
	if !errors.Is(err, ErrNoAgeIdentity) {
		t.Fatalf("err = %v, want ErrNoAgeIdentity", err)
	}
	for _, w := range []string{id.Recipient().String(), "SOPS_AGE_KEY (unset)", "SOPS_AGE_KEY_FILE (unset)", "keys.txt (absent)", "secret.ageIdentities", "secret.vault"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error lacks %q: %v", w, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v before failing", f.calls)
	}
}

// itemTools is a fake sops and vault: op item list answers the vault's
// item titles (metadata, no value), op read a recipient's item, sops the
// document, and every call is recorded with its environment.
type itemTools struct {
	// items map an item's title to its password field.
	items map[string]string
	calls []string
	env   []string
}

func (f *itemTools) run(_ context.Context, _ string, env []string, _ io.Reader, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "op" {
		switch args[0] {
		case itemOp:
			out := []map[string]string{}
			for title := range f.items {
				out = append(out, map[string]string{"id": "id-" + title, "title": title})
			}
			return json.Marshal(out)
		case "read":
			parts := strings.SplitN(strings.TrimPrefix(args[len(args)-1], "op://"), "/", 3)
			v, ok := f.items[parts[1]]
			if !ok {
				return nil, errors.New("op: exit 1 (isn't an item)")
			}
			return []byte(v), nil
		}
		return nil, errors.New("op: exit 2 (unknown)")
	}
	f.env = env
	return []byte(ageDoc), nil
}

func TestAgeIdentityFromTheVaultItem(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	// the file's first recipient has no item, its second one has
	file := sopsFile(t, "", other.Recipient().String(), r)
	args := []string{file + "#stringData.password"}
	f := &itemTools{items: map[string]string{AgeItemTitle(r): id.String() + "\n", "unrelated": "x"}}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t"}
	if !o.AgeNeedsVault("", args) || !o.AgeNeedsIdentity("", args) {
		t.Errorf("AgeNeedsVault = %v, AgeNeedsIdentity = %v: want the vault", o.AgeNeedsVault("", args), o.AgeNeedsIdentity("", args))
	}
	vs, err := o.values(context.Background(), Ref{File: file})
	if err != nil {
		t.Fatal(err)
	}
	if vs["stringData.password"] != "x" {
		t.Errorf("values = %v", vs)
	}
	want := []string{opItemList, "op read --no-newline " + AgeItemRef(ageVault, r)}
	if got := f.calls[:len(f.calls)-1]; !slices.Equal(got, want) || !strings.HasPrefix(f.calls[len(f.calls)-1], "sops ") {
		t.Errorf("calls = %q: want %q, then sops", f.calls, want)
	}
	if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
		t.Errorf("sops did not get the identity alone (%d entries)", len(f.env))
	}

	// an entry of secret.ageIdentities comes first: the vault is not asked
	keys := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(keys, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	o.Ages = []AgeIdentity{{Recipient: r, Ref: FileRef + keys}}
	if o.AgeNeedsVault("", args) {
		t.Error("AgeNeedsVault = true with the identity in a file")
	}
	if _, err := o.values(context.Background(), Ref{File: file}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "sops ") {
		t.Errorf("calls = %q: want sops alone", f.calls)
	}
}

func TestNoVaultItemFailsInOneLineNamingTheInstallation(t *testing.T) {
	id := isolateAge(t)
	r := id.Recipient().String()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := sopsFileAt(t, filepath.Join(dir, "management-clusters", "graveler", "secrets", "app.sops.yaml"), "", r)
	f := &itemTools{items: map[string]string{"unrelated": "x"}}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t", Installations: []string{"gazelle", "graveler"}}
	_, err := o.values(context.Background(), Ref{File: file})
	if !errors.Is(err, ErrNoAgeIdentity) || errors.Is(err, ErrVault) {
		t.Fatalf("err = %v, want ErrNoAgeIdentity and no vault error", err)
	}
	for _, w := range []string{"graveler's recipient " + r, `"` + AgeItemTitle(r) + `"`, "the vault Shared holds no item", "AGE-SECRET-KEY-1"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error lacks %q: %v", w, err)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the error is more than one line: %v", err)
	}
	if len(f.calls) != 1 || f.calls[0] != opItemList {
		t.Errorf("calls = %q: want the vault's item listing alone, no read and no sops", f.calls)
	}

	// a file outside any installation names no installation
	f.calls = nil
	_, err = o.values(context.Background(), Ref{File: sopsFile(t, "", r)})
	if err == nil || !strings.Contains(err.Error(), "for its recipient "+r) {
		t.Errorf("err = %v", err)
	}
}

func TestAgeIdentityFromTheVault(t *testing.T) {
	id := isolateAge(t)
	for name, entry := range map[string]AgeIdentity{
		"recipient": {Recipient: id.Recipient().String(), Ref: ageRef},
		"path":      {Path: regexp.MustCompile(`/app\.sops\.yaml$`), Ref: ageRef},
	} {
		t.Run(name, func(t *testing.T) {
			f := &ageTools{identity: id.String() + "\n"}
			o := &Ops{Run: f.run, Vault: ageVault, Token: "t", Ages: []AgeIdentity{entry}}
			file := sopsFile(t, "", id.Recipient().String())
			if !o.AgeNeedsVault("", []string{"--name=x", file + "#stringData.password"}) {
				t.Error("AgeNeedsVault = false")
			}
			// a file named relative to another directory, as the broker gets it
			if !o.AgeNeedsVault(filepath.Dir(file), []string{"app.sops.yaml#stringData.password"}) {
				t.Error("AgeNeedsVault relative to its directory = false")
			}
			vs, err := o.values(context.Background(), Ref{File: file})
			if err != nil {
				t.Fatal(err)
			}
			if vs["stringData.password"] != "x" {
				t.Errorf("values = %v", vs)
			}
			if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
				t.Errorf("sops did not get the identity alone (%d entries)", len(f.env))
			}
		})
	}
}

func TestAgeIdentityFromAFile(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(t.TempDir(), "identity.txt")
	body := "# created: now\n# public key: " + other.Recipient().String() + "\n" + other.String() + "\n\n" + id.String() + "\n"
	if err := os.WriteFile(keys, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &ageTools{}
	o := &Ops{Run: f.run, Ages: []AgeIdentity{{Recipient: id.Recipient().String(), Ref: FileRef + keys}}}
	file := sopsFile(t, "", id.Recipient().String())
	args := []string{file + "#stringData.password"}
	if o.AgeNeedsVault("", args) || !o.AgeNeedsIdentity("", args) {
		t.Errorf("AgeNeedsVault = %v, AgeNeedsIdentity = %v: want the identity without the vault", o.AgeNeedsVault("", args), o.AgeNeedsIdentity("", args))
	}
	if _, err := o.values(context.Background(), Ref{File: file}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "sops ") {
		t.Errorf("calls = %v: want sops alone, no op", f.calls)
	}
	if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
		t.Errorf("sops did not get the matching identity alone (%d entries)", len(f.env))
	}

	// a file without the recipient's identity is refused, naming no identity
	if err := os.WriteFile(keys, []byte("# created: now\n"+other.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = o.values(context.Background(), Ref{File: file})
	if err == nil || !strings.Contains(err.Error(), "not of the file's recipients") || strings.Contains(err.Error(), other.String()) {
		t.Fatalf("err = %v", err)
	}
	if err := os.WriteFile(keys, []byte("AGE-SECRET-KEY-1NOTAKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = o.values(context.Background(), Ref{File: file})
	if err == nil || !strings.Contains(err.Error(), "holds no age identity") || strings.Contains(err.Error(), "NOTAKEY") {
		t.Fatalf("err = %v", err)
	}
}

// storeTools is a fake sops and person's credential store: store-read
// answers an entry's secret, store-search the entries whose name holds the
// term, and every call is recorded.
type storeTools struct {
	entries map[string]string
	fail    bool
	calls   []string
	env     []string
}

func (f *storeTools) run(_ context.Context, _ string, env []string, _ io.Reader, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	arg := args[len(args)-1]
	switch name {
	case "store-read":
		if f.fail {
			return nil, errors.New("store-read: exit 1 (locked)")
		}
		v, ok := f.entries[arg]
		if !ok {
			return nil, errors.New("store-read: exit 1 (no such entry)")
		}
		return []byte(v), nil
	case "store-search":
		var out []string
		for e := range f.entries {
			if strings.Contains(e, arg) {
				out = append(out, e)
			}
		}
		return []byte(strings.Join(out, "\n") + "\n"), nil
	}
	f.env = env
	return []byte(ageDoc), nil
}

func TestAgeIdentityFromTheStore(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	const otherEntry = "keys/other"
	store := Store{Read: []string{"store-read", "--field", "password"}, Search: []string{"store-search"}}
	file := sopsFile(t, "", r)
	args := []string{file + "#stringData.password"}

	for _, tc := range []struct {
		name, ref string
		entries   map[string]string
		wantCalls []string
	}{
		{"entry", StoreRef + "keys/age", map[string]string{"keys/age": id.String()},
			[]string{"store-read --field password keys/age"}},
		{"search", StoreRef, map[string]string{"keys/age " + r: "# public key: " + r + "\n" + id.String(), otherEntry: other.String()},
			[]string{"store-search " + r, "store-read --field password keys/age " + r}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &storeTools{entries: tc.entries}
			o := &Ops{Run: f.run, Store: store, Ages: []AgeIdentity{{Recipient: r, Ref: tc.ref}}}
			if o.AgeNeedsVault("", args) || !o.AgeNeedsIdentity("", args) {
				t.Errorf("AgeNeedsVault = %v, AgeNeedsIdentity = %v: want the identity without the vault", o.AgeNeedsVault("", args), o.AgeNeedsIdentity("", args))
			}
			if _, err := o.values(context.Background(), Ref{File: file}); err != nil {
				t.Fatal(err)
			}
			if got := f.calls[:len(f.calls)-1]; !slices.Equal(got, tc.wantCalls) || !strings.HasPrefix(f.calls[len(f.calls)-1], "sops ") {
				t.Errorf("calls = %q: want %q, then sops", f.calls, tc.wantCalls)
			}
			if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
				t.Errorf("sops did not get the matching identity alone (%d entries)", len(f.env))
			}
		})
	}

	for _, tc := range []struct {
		name, ref string
		entries   map[string]string
		fail      bool
		store     Store
		want      string
	}{
		{"no entry found", StoreRef, map[string]string{otherEntry: other.String()}, false, store, "has no entry for " + r},
		{"another identity", StoreRef + otherEntry, map[string]string{otherEntry: other.String()}, false, store, "not of the file's recipients"},
		{"no identity", StoreRef + "keys/note", map[string]string{"keys/note": "AGE-SECRET-KEY-1NOTAKEY"}, false, store, "store://keys/note holds no age identity"},
		{"store fails", StoreRef + "keys/age", map[string]string{"keys/age": id.String()}, true, store, "locked"},
		{"no store", StoreRef + "keys/age", nil, false, Store{}, "secret.store is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &storeTools{entries: tc.entries, fail: tc.fail}
			o := &Ops{Run: f.run, Store: tc.store, Ages: []AgeIdentity{{Recipient: r, Ref: tc.ref}}}
			_, err := o.values(context.Background(), Ref{File: file})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, v := range tc.entries {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the error names a value: %v", err)
				}
			}
			for _, c := range f.calls {
				if strings.HasPrefix(c, "sops ") {
					t.Errorf("sops ran: %v", f.calls)
				}
			}
		})
	}
}

func TestAgeIdentityOfAnotherRecipientIsRefused(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f := &ageTools{identity: other.String()}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t", Ages: []AgeIdentity{{Recipient: id.Recipient().String(), Ref: ageRef}}}
	_, err = o.values(context.Background(), Ref{File: sopsFile(t, "", id.Recipient().String())})
	if err == nil || !strings.Contains(err.Error(), "not of the file's recipients") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), other.String()) {
		t.Fatal("the error carries the identity")
	}
}

func TestSOPSFindsTheIdentityItself(t *testing.T) {
	// writeKeys writes an identity file holding id after another identity.
	writeKeys := func(t *testing.T, path string, id *age.X25519Identity) {
		t.Helper()
		other, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // the test's scratch directory
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# created: now\n"+other.String()+"\n"+id.String()+"\n"), 0o600); err != nil { //nolint:gosec // the test's scratch file
			t.Fatal(err)
		}
	}
	for name, setup := range map[string]func(*testing.T, *age.X25519Identity){
		"key": func(t *testing.T, id *age.X25519Identity) { t.Setenv(envAgeKey, id.String()) },
		"key file": func(t *testing.T, id *age.X25519Identity) {
			p := filepath.Join(t.TempDir(), "keys.txt")
			writeKeys(t, p, id)
			t.Setenv(envAgeKeyFile, p)
		},
		"default": func(t *testing.T, id *age.X25519Identity) {
			writeKeys(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "sops", "age", "keys.txt"), id)
		},
		"plugin": func(t *testing.T, _ *age.X25519Identity) { t.Setenv(envAgeKey, "AGE-PLUGIN-YUBIKEY-1EXAMPLE") },
		"opaque": func(t *testing.T, _ *age.X25519Identity) { t.Setenv("SOPS_AGE_KEY_CMD", "echo") },
	} {
		t.Run(name, func(t *testing.T) {
			id := isolateAge(t)
			setup(t, id)
			f := &ageTools{}
			o := &Ops{Run: f.run, Ages: []AgeIdentity{{Recipient: id.Recipient().String(), Ref: ageRef}}}
			if _, err := o.values(context.Background(), Ref{File: sopsFile(t, "", id.Recipient().String())}); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 || f.env != nil {
				t.Errorf("calls %v, env of %d entries: want sops alone, unchanged", f.calls, len(f.env))
			}
		})
	}
}

func TestNoAgeCheckBeyondX25519(t *testing.T) {
	id := isolateAge(t)
	for name, file := range map[string]string{
		"no metadata": sopsFile(t, ""),
		"kms too":     sopsFile(t, "  kms:\n    - arn: arn:aws:kms:x\n", id.Recipient().String()),
		"ssh":         sopsFile(t, "", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &ageTools{}
			o := &Ops{Run: f.run}
			if _, err := o.values(context.Background(), Ref{File: file}); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 {
				t.Errorf("calls = %v", f.calls)
			}
		})
	}
}

const (
	// ageVaultB is an age vault beside the shared one.
	ageVaultB = "Common"
	// noteField is a Secure Note's notes field.
	noteField     = "notesPlain"
	unrelatedItem = "unrelated"
	titleKey      = "title"
	opRead        = "read"
	cedarItem     = "cedar.agekey"
	commonRef     = "op://" + ageVaultB + "/" + cedarItem + "/" + noteField
)

// vaultsTools is a fake sops and several vaults: op item list answers a
// vault's titles, op item get an item's fields, op document get a document
// item's file, op read a field; every call is recorded.
type vaultsTools struct {
	// vaults map a vault to its items' titles, each to its fields (label to
	// value); the field "" is a document's file.
	vaults map[string]map[string]map[string]string
	calls  []string
	env    []string
}

func (f *vaultsTools) run(_ context.Context, _ string, env []string, _ io.Reader, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "op" {
		f.env = env
		return []byte(ageDoc), nil
	}
	item := func(id string) (map[string]string, bool) {
		vault, title, _ := strings.Cut(strings.TrimPrefix(id, "id-"), "/")
		it, ok := f.vaults[vault][title]
		return it, ok
	}
	switch strings.Join(args[:2], " ") {
	case "item list":
		out := []map[string]string{}
		for title := range f.vaults[args[3]] {
			out = append(out, map[string]string{"id": "id-" + args[3] + "/" + title, titleKey: title})
		}
		return json.Marshal(out)
	case "item get":
		it, ok := item(args[2])
		if !ok {
			return nil, errors.New("op: exit 1 (isn't an item)")
		}
		doc := map[string]any{"category": "SECURE_NOTE"}
		var fields []map[string]string
		for label, v := range it {
			if label == "" {
				doc["category"] = "DOCUMENT"
				continue
			}
			fields = append(fields, map[string]string{"label": label, "value": v})
		}
		doc["fields"] = fields
		return json.Marshal(doc)
	case "document get":
		it, _ := item(args[2])
		return []byte(it[""]), nil
	}
	if args[0] == opRead {
		parts := strings.SplitN(strings.TrimPrefix(args[len(args)-1], "op://"), "/", 3)
		if v, ok := f.vaults[parts[0]][parts[1]][parts[2]]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("op: exit 1 (isn't an item)")
	}
	return nil, errors.New("op: exit 2 (unknown)")
}

func TestAgeIdentityFromTheInstallationItem(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	keysFile := "# created: 2026-01-01\n# public key: " + r + "\n" + id.String() + "\n"
	dir := t.TempDir()
	file := sopsFileAt(t, filepath.Join(dir, "installations", "cedar", "apps", "dex-app", "secret-values.yaml.patch"), "", r)
	for name, fields := range map[string]map[string]string{
		"a note's field":         {noteField: keysFile, "username": "x"},
		"a document":             {"": keysFile},
		"two identities, one is": {noteField: other.String() + "\n", ageItemField: id.String()},
	} {
		t.Run(name, func(t *testing.T) {
			f := &vaultsTools{vaults: map[string]map[string]map[string]string{
				ageVault:  {unrelatedItem: {ageItemField: "x"}},
				ageVaultB: {cedarItem: fields, "elver.agekey": {noteField: other.String()}},
			}}
			o := &Ops{Run: f.run, Vault: ageVault, Token: "t", AgeVaults: []string{ageVaultB}}
			if !o.AgeNeedsVault("", []string{file}) {
				t.Error("AgeNeedsVault = false")
			}
			vs, err := o.values(context.Background(), Ref{File: file})
			if err != nil {
				t.Fatal(err)
			}
			if vs["stringData.password"] != "x" {
				t.Errorf("values = %v", vs)
			}
			if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
				t.Errorf("sops did not get the identity alone (%d entries)", len(f.env))
			}
			for _, c := range f.calls {
				if strings.Contains(c, "elver") {
					t.Errorf("read another installation's item: %q", c)
				}
			}

			rs, err := o.Recipients(context.Background(), file)
			if err != nil || len(rs) != 1 || rs[0].Identity != `the vault item "cedar.agekey" of Common` {
				t.Errorf("recipients = %+v, %v", rs, err)
			}
			raw, _ := json.Marshal(rs)
			if strings.Contains(string(raw), id.String()) {
				t.Fatal("the listing carries an identity")
			}
		})
	}
}

func TestNoInstallationItemFailsNamingIt(t *testing.T) {
	id := isolateAge(t)
	r := id.Recipient().String()
	file := sopsFileAt(t, filepath.Join(t.TempDir(), "installations", "cedar", "secret.yaml"), "", r)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f := &vaultsTools{vaults: map[string]map[string]map[string]string{ageVault: {}, ageVaultB: {"elver.agekey": {noteField: other.String()}}}}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t", AgeVaults: []string{ageVaultB}}
	_, err = o.values(context.Background(), Ref{File: file})
	if !errors.Is(err, ErrNoAgeIdentity) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err = %v, want one line with ErrNoAgeIdentity", err)
	}
	for _, w := range []string{"cedar's recipient " + r, `"` + AgeItemTitle(r) + `"`, `no vault of Shared, Common an item "cedar.agekey"`} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error lacks %q: %v", w, err)
		}
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "op item list") {
			t.Errorf("calls = %q: want the vaults' item listings alone", f.calls)
		}
	}
	rs, err := o.Recipients(context.Background(), file)
	if err != nil || len(rs) != 1 || !rs[0].Missing() || !strings.Contains(rs[0].Identity, `"cedar.agekey"`) {
		t.Errorf("recipients = %+v, %v", rs, err)
	}

	// the installation's item holding another recipient's identity is refused
	f.vaults[ageVaultB][cedarItem] = map[string]string{noteField: other.String()}
	if _, err := o.values(context.Background(), Ref{File: file}); err == nil || !strings.Contains(err.Error(), "holds the identity of "+other.Recipient().String()) {
		t.Errorf("err = %v", err)
	}
}

func TestAgeIdentityEntryInAnAgeVault(t *testing.T) {
	id := isolateAge(t)
	r := id.Recipient().String()
	file := sopsFile(t, "", r)
	f := &vaultsTools{vaults: map[string]map[string]map[string]string{ageVaultB: {cedarItem: {noteField: id.String()}}}}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t", Ages: []AgeIdentity{{Recipient: r, Ref: commonRef}}}
	if _, err := o.values(context.Background(), Ref{File: file}); err == nil || !strings.Contains(err.Error(), "secret.ageVaults") {
		t.Errorf("an entry outside the age vaults: err = %v", err)
	}
	o.AgeVaults = []string{ageVaultB}
	if _, err := o.values(context.Background(), Ref{File: file}); err != nil {
		t.Fatal(err)
	}
	if len(f.env) != 1 || f.env[0] != envAgeKey+"="+id.String() {
		t.Errorf("sops did not get the identity alone (%d entries)", len(f.env))
	}
	// an age vault is no shared vault for any other reference
	if _, err := o.values(context.Background(), Ref{Op: commonRef}); err == nil {
		t.Error("an op:// reference into an age vault reads")
	}
}
