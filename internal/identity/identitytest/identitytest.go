// Package identitytest is a Dex stand-in for tests: an OIDC issuer that
// serves its discovery document and keys and signs ID tokens.
package identitytest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Issuer is a running test issuer.
type Issuer struct {
	URL    string
	signer jose.Signer
}

// New starts an issuer for the test's lifetime.
func New(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key, KeyID: "test", Algorithm: string(jose.RS256), Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{signer: signer}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": iss.URL, "jwks_uri": iss.URL + "/keys", "authorization_endpoint": iss.URL + "/auth",
			"token_endpoint": iss.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk.Public()}})
	})
	return iss
}

// Claims are an ID token's: the standard ones are filled in by Token where
// they are unset.
type Claims struct {
	Issuer        string   `json:"iss,omitempty"`
	Subject       string   `json:"sub,omitempty"`
	Audience      []string `json:"aud,omitempty"`
	Expiry        int64    `json:"exp,omitempty"`
	IssuedAt      int64    `json:"iat,omitempty"`
	Email         string   `json:"email,omitempty"`
	EmailVerified *bool    `json:"email_verified,omitempty"`
	Groups        []string `json:"groups,omitempty"`
}

// Token signs c, with the issuer, a subject, an hour of validity and the
// audience aud filled in where unset.
func (iss *Issuer) Token(t *testing.T, aud string, c Claims) string {
	t.Helper()
	now := time.Now()
	if c.Issuer == "" {
		c.Issuer = iss.URL
	}
	if c.Subject == "" {
		c.Subject = "CgR0ZXN0EgZnaXRodWI"
	}
	if c.Audience == nil {
		c.Audience = []string{aud}
	}
	if c.Expiry == 0 {
		c.Expiry = now.Add(time.Hour).Unix()
	}
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	raw, err := jwt.Signed(iss.signer).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Verified is a true email_verified.
func Verified() *bool { v := true; return &v }
