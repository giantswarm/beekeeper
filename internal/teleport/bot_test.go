package teleport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBotStatus(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bot"}, NotAfter: expiry, NotBefore: expiry.Add(-time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cert")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := BotStatus(path)
	if err != nil || p.Username != "bot" || !p.ValidUntil.Equal(expiry) {
		t.Fatalf("profile %+v, error %v", p, err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BotStatus(path); err == nil {
		t.Fatal("accepted invalid certificate")
	}
	if _, err := BotStatus(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted missing certificate")
	}
}

func TestBotRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "systemctl")
	for _, exit := range []string{"0", "1"} {
		script := "#!/bin/sh\n[ \"$*\" = '--user is-active --quiet beekeeper-teleport.service' ] || exit 2\nexit " + exit + "\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test fixture
			t.Fatal(err)
		}
		err := BotRunning(context.Background())
		if (err == nil) != (exit == "0") {
			t.Fatalf("exit %s: %v", exit, err)
		}
	}
}
