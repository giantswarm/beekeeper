package knownhosts

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const trustCertHost = "cert.invalid:22"

func syntheticSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func syntheticCertificate(t *testing.T, ca ssh.Signer, principal string) *ssh.Certificate {
	t.Helper()
	cert := &ssh.Certificate{
		Key:             syntheticSigner(t).PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{principal},
		ValidAfter:      0,
		ValidBefore:     ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return cert
}

type trustDecision struct {
	name    string
	host    string
	key     ssh.PublicKey
	allow   bool
	revoked bool
}

// Check the consumer's decisions before introducing an unsupported entry and
// again using only the copy that replaces both source files.
func checkFilteredTrust(t *testing.T, first, second []string, decisions []trustDecision) {
	t.Helper()
	dir := t.TempDir()
	sources := []string{filepath.Join(dir, "first"), filepath.Join(dir, "second")}
	dst := filepath.Join(dir, "filtered")
	write(t, sources[0], first...)
	write(t, sources[1], second...)
	check := func(t *testing.T, paths ...string) {
		t.Helper()
		callback, err := knownhosts.New(paths...)
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range decisions {
			t.Run(decision.name, func(t *testing.T) {
				err := callback(decision.host, &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}, decision.key)
				if (err == nil) != decision.allow {
					t.Fatalf("allowed = %v, want %v (error: %v)", err == nil, decision.allow, err)
				}
				if decision.revoked {
					var revoked *knownhosts.RevokedError
					if !errors.As(err, &revoked) {
						t.Fatalf("want revoked key error, got %v", err)
					}
				}
			})
		}
	}
	t.Run("original", func(t *testing.T) { check(t, sources...) })
	write(t, sources[0], append(append([]string{}, first...), unknownLine("unsupported.invalid"))...)
	before := make([][]byte, len(sources))
	for i, source := range sources {
		var err error
		before[i], err = os.ReadFile(source) //nolint:gosec // the test's own temp file
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := knownhosts.New(sources...); err == nil {
		t.Fatal("unsupported entry must force filtering")
	}
	dropped, err := Parseable(dst, sources...)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped %d lines, want 1", dropped)
	}
	t.Run("filtered", func(t *testing.T) { check(t, dst) })
	for i, source := range sources {
		after, err := os.ReadFile(source) //nolint:gosec // the test's own temp file
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before[i]) {
			t.Error("source file changed")
		}
	}
}

func TestParseablePreservesRawKeyRevocation(t *testing.T) {
	key := syntheticSigner(t).PublicKey()
	line := knownhosts.Line([]string{"raw.invalid"}, key)
	t.Run("trusted", func(t *testing.T) {
		checkFilteredTrust(t, []string{line}, nil, []trustDecision{{name: "trusted key", host: "raw.invalid:22", key: key, allow: true}})
	})
	t.Run("revoked in second source", func(t *testing.T) {
		checkFilteredTrust(t, []string{line}, []string{"@revoked " + line}, []trustDecision{{name: "revoked key", host: "raw.invalid:22", key: key, revoked: true}})
	})
}

func TestParseablePreservesCertificateAuthority(t *testing.T) {
	ca := syntheticSigner(t)
	valid := syntheticCertificate(t, ca, "cert.invalid")
	wrongPrincipal := syntheticCertificate(t, ca, "other.invalid")
	unknownSigner := syntheticCertificate(t, syntheticSigner(t), "cert.invalid")
	authority := "@cert-authority " + knownhosts.Line([]string{"cert.invalid"}, ca.PublicKey())
	t.Run("authority", func(t *testing.T) {
		checkFilteredTrust(t, []string{authority}, nil, []trustDecision{
			{name: "signed certificate", host: trustCertHost, key: valid, allow: true},
			{name: "wrong principal", host: trustCertHost, key: wrongPrincipal},
			{name: "unknown signer", host: trustCertHost, key: unknownSigner},
			{name: "outside authority scope", host: "other.invalid:22", key: wrongPrincipal},
		})
	})
	t.Run("revoked authority in second source", func(t *testing.T) {
		checkFilteredTrust(t, []string{authority}, []string{"@revoked " + knownhosts.Line([]string{"*"}, ca.PublicKey())}, []trustDecision{
			{name: "revoked certificate authority", host: trustCertHost, key: valid},
		})
	})
}

func TestParseablePreservesHostMatching(t *testing.T) {
	hashed, aliases, wildcard, wrong := syntheticSigner(t).PublicKey(), syntheticSigner(t).PublicKey(), syntheticSigner(t).PublicKey(), syntheticSigner(t).PublicKey()
	checkFilteredTrust(t,
		[]string{knownhosts.Line([]string{knownhosts.HashHostname("[hashed.invalid]:2222")}, hashed)},
		[]string{knownhosts.Line([]string{"alias-one.invalid", "alias-two.invalid"}, aliases), knownhosts.Line([]string{"*.wild.invalid", "!excluded.wild.invalid"}, wildcard)},
		[]trustDecision{
			{name: "hashed custom port", host: "hashed.invalid:2222", key: hashed, allow: true},
			{name: "hashed default port", host: "hashed.invalid:22", key: hashed},
			{name: "hashed other port", host: "hashed.invalid:2223", key: hashed},
			{name: "hashed wrong key", host: "hashed.invalid:2222", key: wrong},
			{name: "first alias", host: "alias-one.invalid:22", key: aliases, allow: true},
			{name: "second alias", host: "alias-two.invalid:22", key: aliases, allow: true},
			{name: "alias wrong key", host: "alias-two.invalid:22", key: wrong},
			{name: "alias other port", host: "alias-one.invalid:2222", key: aliases},
			{name: "wildcard", host: "allowed.wild.invalid:22", key: wildcard, allow: true},
			{name: "excluded host", host: "excluded.wild.invalid:22", key: wildcard},
			{name: "wildcard wrong key", host: "allowed.wild.invalid:22", key: wrong},
			{name: "wildcard other port", host: "allowed.wild.invalid:2222", key: wildcard},
			{name: "unknown host", host: "unknown.invalid:22", key: wildcard},
		})
}
