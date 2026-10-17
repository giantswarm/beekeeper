package knownhosts

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
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
	blob := binary.BigEndian.AppendUint32(nil, uint32(len(typ)))
	blob = append(blob, typ...)
	blob = binary.BigEndian.AppendUint32(blob, 4)
	blob = append(blob, "keyk"...)
	return host + " " + typ + " " + base64.StdEncoding.EncodeToString(blob)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseableDropsUnknownKeyTypes(t *testing.T) {
	dir := t.TempDir()
	home, etc, dst := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "ssh_known_hosts"), filepath.Join(dir, "out", "known_hosts")
	good, other := hostLine(t, "github.com"), hostLine(t, "gitlab.com")
	write(t, home, "# comment", good, unknownLine("pq.example"), "")
	write(t, etc, other)
	before, _ := os.ReadFile(home)

	if _, err := knownhosts.New(home); err == nil {
		t.Fatal("the source must fail Go's knownhosts, or this test proves nothing")
	}
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
	after, _ := os.ReadFile(home)
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
