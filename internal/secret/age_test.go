package secret

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"
)

const (
	ageVault = "Shared"
	ageRef   = "op://Shared/age/identity"
	// ageDoc is a decrypted document the fake sops answers.
	ageDoc = "stringData:\n  password: x\n"
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
	var b strings.Builder
	b.WriteString("stringData:\n  password: ENC[AES256_GCM,data:x,type:str]\nsops:\n")
	if len(recipients) > 0 {
		b.WriteString("  age:\n")
		for _, r := range recipients {
			b.WriteString("    - recipient: " + r + "\n      enc: |\n        -----BEGIN AGE ENCRYPTED FILE-----\n")
		}
	}
	b.WriteString(extra)
	p := filepath.Join(t.TempDir(), "app.sops.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNoAgeIdentityFailsBeforeSOPS(t *testing.T) {
	id := isolateAge(t)
	f := &ageTools{}
	o := &Ops{Run: f.run, Vault: ageVault, Token: "t"}
	file := sopsFile(t, "", id.Recipient().String())
	_, err := o.values(context.Background(), Ref{File: file})
	if !errors.Is(err, ErrNoAgeIdentity) {
		t.Fatalf("err = %v, want ErrNoAgeIdentity", err)
	}
	for _, w := range []string{id.Recipient().String(), "SOPS_AGE_KEY (unset)", "SOPS_AGE_KEY_FILE (unset)", "keys.txt (absent)", "secret.ageIdentities"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the error lacks %q: %v", w, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v before failing", f.calls)
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
