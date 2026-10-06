package sandbox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// caLifetime is how long the egress proxy's CA is valid: it lives in the
// broker's memory and is made anew at every start.
const caLifetime = 365 * 24 * time.Hour

// CA is the egress proxy's certificate authority: made in the broker's
// memory, its key never written anywhere, and name-constrained to the
// hosts it terminates TLS for, so a client that trusts it trusts it for
// GitHub's hosts alone.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// leafKey is the key of every leaf the CA issues.
	leafKey *ecdsa.PrivateKey
	pem     []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// NewCA makes a CA for hosts, valid from now.
func NewCA(hosts []string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	name, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: strings.TrimSpace("beekeeper sandbox egress " + name)},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caLifetime),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, leafKey: leafKey, leaves: map[string]*tls.Certificate{},
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// PEM is the CA's certificate.
func (c *CA) PEM() []byte { return c.pem }

// Leaf is the CA's certificate for host, made once.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leaves[host]; ok {
		return l, nil
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    c.cert.NotBefore,
		NotAfter:     c.cert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: c.leafKey}
	c.leaves[host] = l
	return l, nil
}

// serial is a random certificate serial number.
func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}
