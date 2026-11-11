package knownhosts

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostLine is a known_hosts line Go parses, for a fresh ed25519 key.
func hostLine(t *testing.T, host string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return knownhosts.Line([]string{host}, k)
}

// unknownLine is a well-formed line of a key type Go does not know, as
// OpenSSH writes a post-quantum host key.
func unknownLine(host string) string {
	typ := "ssh-mldsa44-ed25519"
	blob := ssh.Marshal(struct {
		Type string
		Key  []byte
	}{typ, []byte("key")})
	return host + " " + typ + " " + base64.StdEncoding.EncodeToString(blob)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireRejectedLine checks a fixture in isolation, as Parseable does. Since
// x/crypto v0.59.0, a valid entry in the same file masks other parse errors.
func requireRejectedLine(t *testing.T, line string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "invalid")
	write(t, src, line)
	if _, err := knownhosts.New(src); err == nil {
		t.Fatal("consumer accepted the invalid line in isolation")
	}
}

func TestParseableDropsUnknownKeyTypes(t *testing.T) {
	dir := t.TempDir()
	home, etc, dst := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "ssh_known_hosts"), filepath.Join(dir, "out", "known_hosts")
	good, other := hostLine(t, "github.com"), hostLine(t, "gitlab.com")
	write(t, home, "# comment", good, unknownLine("pq.example"), "")
	write(t, etc, other)
	before, _ := os.ReadFile(home) //nolint:gosec // the test's own temp file

	requireRejectedLine(t, unknownLine("pq.example"))
	dropped, err := Parseable(dst, home, etc, filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d lines, want 1", dropped)
	}
	cb, err := knownhosts.New(dst)
	if err != nil {
		t.Fatalf("the written file does not parse: %v", err)
	}
	for _, l := range []string{good, other} {
		_, hosts, key, _, _, err := ssh.ParseKnownHosts([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		if err := cb(hosts[0]+":22", &addr{}, key); err != nil {
			t.Errorf("%s: %v", hosts[0], err)
		}
	}
	after, _ := os.ReadFile(home) //nolint:gosec // the test's own temp file
	if string(after) != string(before) {
		t.Error("the source file changed")
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("dst mode: %v %v", fi, err)
	}
}

func TestParseableWritesNothingWhenAllParse(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "out", "known_hosts")
	write(t, src, hostLine(t, "github.com"))
	dropped, err := Parseable(dst, src)
	if err != nil || dropped != 0 {
		t.Fatalf("dropped %d, err %v", dropped, err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst written: %v", err)
	}
}

type addr struct{}

func (addr) Network() string { return "tcp" }
func (addr) String() string  { return "192.0.2.1:22" }

func TestParseableDropsInvalidHostEntries(t *testing.T) {
	good := hostLine(t, "192.0.2.1")
	key := good[strings.IndexByte(good, ' '):]
	cases := map[string]string{
		"malformed hash":           "|1|bad|bad" + key,
		"unsupported hash version": strings.Replace(knownhosts.HashHostname("192.0.2.1"), "|1|", "|2|", 1) + key,
		"broken bracket":           "[broken" + key,
		"empty negation":           "!" + key,
		"unknown marker":           "@unknown " + good,
		"unicode comment prefix":   "\u2003# comment",
		"oversized entry":          good + " " + strings.Repeat("x", 64*1024),
		"oversized comment":        "#" + strings.Repeat("x", 1024*1024),
	}
	for name, invalid := range cases {
		for _, extra := range []bool{false, true} {
			suffix := "/alone"
			if extra {
				suffix = "/with unsupported key"
			}
			t.Run(name+suffix, func(t *testing.T) {
				dir := t.TempDir()
				src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "filtered")
				lines := []string{good, invalid, "# retained", "", good}
				wantDropped := 1
				if extra {
					lines = append(lines, unknownLine("192.0.2.2"))
					wantDropped++
				}
				write(t, src, lines...)
				before, err := os.ReadFile(src) //nolint:gosec // a path of this test
				if err != nil {
					t.Fatal(err)
				}
				requireRejectedLine(t, invalid)
				if extra {
					requireRejectedLine(t, unknownLine("192.0.2.2"))
				}
				dropped, err := Parseable(dst, src)
				if err != nil || dropped != wantDropped {
					t.Fatalf("dropped %d, want %d; error: %v", dropped, wantDropped, err)
				}
				if _, err := knownhosts.New(dst); err != nil {
					t.Fatalf("consumer rejected filtered file: %v", err)
				}
				got, err := os.ReadFile(dst) //nolint:gosec // a path of this test
				if err != nil {
					t.Fatal(err)
				}
				want := strings.Join([]string{good, "# retained", "", good, ""}, "\n")
				if string(got) != want {
					t.Fatal("filtered file did not preserve usable lines")
				}
				after, err := os.ReadFile(src) //nolint:gosec // a path of this test
				if err != nil || string(after) != string(before) {
					t.Fatalf("source changed; error: %v", err)
				}
			})
		}
	}
}
