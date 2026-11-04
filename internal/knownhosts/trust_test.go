package knownhosts

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func trustSigner(t *testing.T) ssh.Signer {
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

func hostCertificate(t *testing.T, ca ssh.Signer, host ssh.PublicKey, principal string) *ssh.Certificate {
	t.Helper()
	cert := &ssh.Certificate{
		Key: host, CertType: ssh.HostCert,
		ValidPrincipals: []string{principal}, ValidBefore: ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestParseablePreservesHostTrust(t *testing.T) {
	dir := t.TempDir()
	home, system, dst := filepath.Join(dir, "home"), filepath.Join(dir, "system"), filepath.Join(dir, "copy")
	hostKey, wrongKey := trustSigner(t).PublicKey(), trustSigner(t).PublicKey()
	revokedKey := trustSigner(t).PublicKey()
	ca, revokedCA, unknownCA := trustSigner(t), trustSigner(t), trustSigner(t)
	lines := []string{
		knownhosts.Line([]string{knownhosts.HashHostname("[hashed.example]:2222")}, hostKey),
		knownhosts.Line([]string{"*.wild.example", "!blocked.wild.example"}, hostKey),
		knownhosts.Line([]string{"alias.example", "mirror.example"}, hostKey),
		knownhosts.Line([]string{"revoked.example"}, revokedKey),
		"@cert-authority " + knownhosts.Line([]string{"*.cert.example", "!blocked.cert.example"}, ca.PublicKey()),
		"@cert-authority " + knownhosts.Line([]string{"revoked-ca.example"}, revokedCA.PublicKey()),
	}
	write(t, home, lines...)
	write(t, system,
		"@revoked "+knownhosts.Line([]string{"*"}, revokedKey),
		"@revoked "+knownhosts.Line([]string{"*"}, revokedCA.PublicKey()),
	)
	original, err := knownhosts.New(home, system)
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, unknownLine("pq.example"))
	write(t, home, lines...)
	dropped, err := Parseable(dst, home, system)
	if err != nil || dropped != 1 {
		t.Fatalf("dropped %d, err %v; want 1, nil", dropped, err)
	}
	filtered, err := knownhosts.New(dst)
	if err != nil {
		t.Fatal(err)
	}
	cert := hostCertificate(t, ca, hostKey, "git.cert.example")
	blockedCert := hostCertificate(t, ca, hostKey, "blocked.cert.example")
	tests := []struct {
		name, address string
		key           ssh.PublicKey
		allow         bool
		revoked       bool
	}{
		{name: "hashed host and port", address: "hashed.example:2222", key: hostKey, allow: true},
		{name: "hashed wrong port", address: "hashed.example:22", key: hostKey},
		{name: "hashed wrong key", address: "hashed.example:2222", key: wrongKey},
		{name: "wildcard", address: "node.wild.example:22", key: hostKey, allow: true},
		{name: "negated host", address: "blocked.wild.example:22", key: hostKey},
		{name: "first alias", address: "alias.example:22", key: hostKey, allow: true},
		{name: "second alias", address: "mirror.example:22", key: hostKey, allow: true},
		{name: "unknown host", address: "unknown.example:22", key: hostKey},
		{name: "revoked trusted key", address: "revoked.example:22", key: revokedKey, revoked: true},
		{name: "trusted host certificate", address: "git.cert.example:22", key: cert, allow: true},
		{name: "wrong certificate principal", address: "other.cert.example:22", key: cert},
		{name: "negated certificate authority host", address: "blocked.cert.example:22", key: blockedCert},
		{name: "unknown certificate authority", address: "git.cert.example:22", key: hostCertificate(t, unknownCA, hostKey, "git.cert.example")},
		{name: "revoked certificate authority", address: "revoked-ca.example:22", key: hostCertificate(t, revokedCA, hostKey, "revoked-ca.example")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for name, callback := range map[string]ssh.HostKeyCallback{"original": original, "filtered": filtered} {
				err := callback(tt.address, &addr{}, tt.key)
				if (err == nil) != tt.allow {
					t.Errorf("%s: allow=%t, error=%v", name, tt.allow, err)
				}
				if tt.revoked {
					var revoked *knownhosts.RevokedError
					if !errors.As(err, &revoked) {
						t.Errorf("%s: want revoked-key rejection, got %v", name, err)
					}
				}
			}
		})
	}
}
