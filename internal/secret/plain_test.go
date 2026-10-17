package secret_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	accessToken = "eyJplain-file-access-token-value" //nolint:gosec // a test's fake token
	authJSON    = `{"OPENAI_API_KEY": null, "tokens": {"access_token": "` + accessToken + `", "refresh_token": "rt-value"}}`
	tokenPath   = "tokens.access_token"
)

// plainRoot writes auth.json into a root of a configured file source and
// answers the root, the file and the operations reading under it.
func plainRoot(t *testing.T) (string, string, *secret.Ops) {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "auth.json")
	if err := os.WriteFile(file, []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	o := ops(secrettest.New(nil))
	o.FileRoots = []string{root}
	return root, file, o
}

func TestParseRefPlainFile(t *testing.T) {
	r, err := secret.ParseRef("file:///home/u/.codex/auth.json#" + tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if r.Plain != "/home/u/.codex/auth.json" || r.Path != tokenPath || r.File != "" || !r.Single() {
		t.Errorf("parsed %+v", r)
	}
	if s := r.String(); s != "file:///home/u/.codex/auth.json#"+tokenPath {
		t.Errorf("String() = %q", s)
	}
	for _, bad := range []string{"file://auth.json#a", "file:///x/auth.json", "file:///x/auth.json#", "file://#a"} {
		if _, err := secret.ParseRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestPlainFileSourceCopiesAndFingerprints reads one value of a plain JSON
// file under a configured root into a lab's Secret, its fingerprint the
// Secret's, and leaves the file as it was.
func TestPlainFileSourceCopiesAndFingerprints(t *testing.T) {
	_, file, o := plainRoot(t)
	before, _ := os.Stat(file)
	src, err := secret.ParseRef("file://" + file + "#" + tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var applied string
	o.Apply = func(_ context.Context, _ []byte, _ secret.KubeTarget, v []byte) error {
		applied = string(v)
		return nil
	}
	tg := secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: "codex", Key: "auth"}
	n, err := o.CopyToSecret(context.Background(), src, tg)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(accessToken) || applied != accessToken {
		t.Errorf("copied %d bytes, the value applied matches: %t", n, applied == accessToken)
	}
	ps, err := o.Fingerprints(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Fingerprint != o.Fingerprint(accessToken) || ps[0].Key != src.String() {
		t.Errorf("fingerprints %+v", ps)
	}
	same, err := o.Compare(context.Background(), src, src)
	if err != nil || len(same) != 1 || same[0].State != secret.Equal {
		t.Errorf("compare %+v, %v", same, err)
	}
	raw, _ := os.ReadFile(file) //nolint:gosec // the test's scratch file
	after, _ := os.Stat(file)
	if string(raw) != authJSON || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Error("the source file changed")
	}
}

func TestPlainFileSourceReadsYAMLAndDotenv(t *testing.T) {
	root, _, o := plainRoot(t)
	for name, content := range map[string]string{
		"creds.yaml": "tokens:\n  access_token: " + accessToken + "\n",
		"creds.env":  "export ACCESS_TOKEN=\"" + accessToken + "\"\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"file://" + root + "/creds.yaml#" + tokenPath, "file://" + root + "/creds.env#ACCESS_TOKEN"} {
		r, err := secret.ParseRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		err = o.Use(context.Background(), r, func(v string) error {
			if v != accessToken {
				t.Errorf("%s: the value read differs", ref)
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

// TestPlainFileSourceRefusals refuses a path outside the roots, through a
// symlink too, a missing file and a missing key, each in one line naming
// it and never the value.
func TestPlainFileSourceRefusals(t *testing.T) {
	root, file, o := plainRoot(t)
	outside := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(outside, []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"file://" + outside + "#" + tokenPath:             {outside, root, "secret.fileRoots"},
		"file://" + link + "#" + tokenPath:                {link, root},
		"file://" + root + "/../" + filepath.Base(root):   {"file://"},
		"file://" + root + "/absent.json#" + tokenPath:    {root + "/absent.json", "no such file"},
		"file://" + file + "#tokens.id_token":             {file, "tokens.id_token"},
		"file://" + root + "/../x/auth.json#" + tokenPath: {"secret.fileRoots"},
	}
	for ref, want := range cases {
		r, err := secret.ParseRef(ref)
		if err == nil {
			err = o.Use(context.Background(), r, func(string) error { return nil })
		}
		if err == nil {
			t.Errorf("%s: read", ref)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, accessToken) || strings.Contains(msg, "rt-value") || strings.Contains(msg, "\n") {
			t.Errorf("%s: the refusal %q carries a value or several lines", ref, msg)
		}
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: the refusal %q does not name %q", ref, msg, w)
			}
		}
	}
	o.FileRoots = nil
	r, _ := secret.ParseRef("file://" + file + "#" + tokenPath)
	err := o.Use(context.Background(), r, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "secret.fileRoots") {
		t.Errorf("no roots configured: %v", err)
	}
}

// TestPlainFileIsNoDestination refuses a file:// reference written to.
func TestPlainFileIsNoDestination(t *testing.T) {
	_, file, o := plainRoot(t)
	dst, err := secret.ParseRef("file://" + file + "#" + tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.CopyValue(context.Background(), secret.Ref{Op: vaultRef}, dst); err == nil {
		t.Error("a value copied into a plain file")
	}
	if raw, _ := os.ReadFile(file); string(raw) != authJSON { //nolint:gosec // the test's scratch file
		t.Error("the source file changed")
	}
}
