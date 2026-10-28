package execution

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

type Receipt struct {
	Ref         Ref       `json:"ref"`
	Coordinator string    `json:"coordinator"`
	Digest      string    `json:"digest"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Signature   string    `json:"signature"`
}

// SigningKey creates a coordinator key once. Only its public half is ever
// returned to an operator; provision that half on the host independently.
func SigningKey(root string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(root, "key.lock"))
	if err := l.Lock(); err != nil {
		return nil, err
	}
	defer func() { _ = l.Unlock() }()
	p := filepath.Join(root, "signing.key")
	b, err := readRegular(p, ed25519.PrivateKeySize)
	if errors.Is(err, os.ErrNotExist) {
		_, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		if e := atomicFile(p, k); e != nil {
			return nil, e
		}
		return k, nil
	}
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid coordinator signing key")
	}
	return ed25519.PrivateKey(b), nil
}

func PublicKey(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func Sign(r StartRequest, key ed25519.PrivateKey, now time.Time) string {
	p := Receipt{Ref: r.Ref, Coordinator: r.Coordinator, Digest: r.Digest(), ExpiresAt: now.Add(time.Minute)}
	p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, mustJSON(p)))
	return base64.StdEncoding.EncodeToString(mustJSON(p))
}

func Verify(r StartRequest, key string, now time.Time) error {
	pub, err := publicKey(key)
	if err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(r.RegistrationReceipt)
	if err != nil {
		return errors.New("invalid registration receipt")
	}
	var p Receipt
	if Decode(b, &p) != nil || p.Ref != r.Ref || p.Coordinator != r.Coordinator || p.Digest != r.Digest() || !p.ExpiresAt.After(now) || p.ExpiresAt.After(now.Add(2*time.Minute)) {
		return errors.New("missing, expired or mismatched registration receipt")
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil {
		return errors.New("invalid receipt signature")
	}
	p.Signature = ""
	if !ed25519.Verify(pub, mustJSON(p), sig) {
		return errors.New("untrusted registration receipt")
	}
	return nil
}
