package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keyAgeIdentities = "secret.ageIdentities"
	keyStore         = "secret.store"
	keyGrantTTL      = "grantTTL"
)

const storeConfig = `# the desk
grantTTL: 20m # short
secret:
  # the vault
  vault: V
  ageIdentities:
    - recipient: age1x
      ref: op://V/i/f
`

func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(raw), 0o640); err != nil { //nolint:gosec // a mode Set keeps
		t.Fatal(err)
	}
	return p
}

// A reference and its source set together land in one write, the file's
// comments, other keys and mode kept.
func TestSetCrossReferencedKeys(t *testing.T) {
	p := writeConfig(t, storeConfig)
	c, err := Set(p, []Setting{
		{keyAgeIdentities, `[{recipient: age1y, ref: "store://keys/age"}]`},
		{keyStore, "{read: [secret-tool, lookup, entry]}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Incomplete()) != 0 || c.Secret.AgeIdentities[0].Ref != "store://keys/age" || len(c.Secret.Store.Read) != 3 {
		t.Errorf("set = %+v, incomplete %v", c.Secret, c.Incomplete())
	}
	raw, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# the desk", "grantTTL: 20m # short", "# the vault", "vault: V", "secret-tool"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("written file lost %q:\n%s", want, raw)
		}
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("written file does not load: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".config.yaml.*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// A result that would not load is refused, the file untouched.
func TestSetRefusesInvalid(t *testing.T) {
	p := writeConfig(t, storeConfig)
	for name, s := range map[string][]Setting{
		"bad ref":       {{keyStore, "{read: [r]}"}, {keyAgeIdentities, "[{recipient: age1y, ref: ~/key}]"}},
		"bad duration":  {{keyGrantTTL, "soon"}},
		"bad value":     {{keyStore, "{read: ["}},
		"no map":        {{"grantTTL.x", "1"}},
		"empty key":     {{"secret..store", "1"}},
		"unknown shape": {{"secret.store.read", "{a: b}"}},
	} {
		_, err := Set(p, s)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if raw, _ := os.ReadFile(filepath.Clean(p)); string(raw) != storeConfig {
			t.Errorf("%s: file changed to\n%s", name, raw)
		}
	}
	if _, err := Set(p, []Setting{{keyGrantTTL, "soon"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid result = %v, want ErrInvalid", err)
	}
}

// A reference whose source is missing is written: it loads, Incomplete
// says it.
func TestSetIncomplete(t *testing.T) {
	p := writeConfig(t, storeConfig)
	c, err := Set(p, []Setting{{keyAgeIdentities, `[{recipient: age1y, ref: "store://keys/age"}]`}})
	if err != nil || len(c.Incomplete()) != 1 {
		t.Fatalf("set = %v, incomplete %v", err, c)
	}
}

// A missing or empty file is created; a linked one is replaced where it
// lives, the link kept.
func TestSetCreatesAndFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(p, []Setting{{"secret.store.read", "[r]"}}); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(p); err != nil || len(c.Secret.Store.Read) != 1 {
		t.Errorf("created = %v", err)
	}
	if err := os.WriteFile(p, []byte("# nothing yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(p, []Setting{{keyGrantTTL, "1m"}}); err != nil {
		t.Errorf("comment-only file: %v", err)
	}

	real := writeConfig(t, storeConfig)
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(link, []Setting{{keyGrantTTL, "5m"}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link replaced: %v, %v", fi.Mode(), err)
	}
	if raw, _ := os.ReadFile(filepath.Clean(real)); !strings.Contains(string(raw), "grantTTL: 5m") {
		t.Errorf("linked file not written:\n%s", raw)
	}
}
