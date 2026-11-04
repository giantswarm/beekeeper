package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type issuerTransport func(*http.Request) (*http.Response, error)

func (f issuerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}
}

// The fixture blocks either before headers or while reading the body. Both
// must observe the HTTP client's deadline, even after Verify is cancelled.
type stalledBody struct {
	ctx     context.Context
	release <-chan struct{}
}

func (b stalledBody) Read([]byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.release:
		return 0, io.EOF
	}
}
func (stalledBody) Close() error { return nil }
func stall(r *http.Request, release <-chan struct{}, body bool) (*http.Response, error) {
	if body {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: stalledBody{r.Context(), release}}, nil
	}
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case <-release:
		return nil, errors.New("fixture released")
	}
}

func TestNewBoundsDiscovery(t *testing.T) {
	for _, body := range []bool{false, true} {
		for _, timeout := range []time.Duration{0, 5 * time.Second, 2 * time.Minute} {
			t.Run(timeout.String()+map[bool]string{false: "/headers", true: "/body"}[body], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					release := make(chan struct{})
					defer close(release)
					transport := issuerTransport(func(r *http.Request) (*http.Response, error) { return stall(r, release, body) })
					client := &http.Client{Transport: transport, Timeout: timeout}
					start := time.Now()
					_, err := identity.New(oidc.ClientContext(context.Background(), client), serveConfig("https://issuer.invalid"))
					if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
						t.Fatalf("discovery error = %v, want deadline", err)
					}
					want := 30 * time.Second
					if timeout > 0 && timeout < want {
						want = timeout
					}
					if elapsed := time.Since(start); elapsed != want {
						t.Fatalf("discovery took %v, want %v", elapsed, want)
					}
					if client.Timeout != timeout {
						t.Fatal("caller client was modified")
					}
				})
			})
		}
	}
}

func TestVerifyRecoversAfterStalledKeyFetch(t *testing.T) {
	for _, body := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "body"}[body], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const issuer = "https://issuer.invalid"
				keys := make([]jose.JSONWebKey, 2)
				tokens := make([]string, 2)
				for i, id := range []string{"old", "new"} {
					_, key, err := ed25519.GenerateKey(rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
					keys[i] = jose.JSONWebKey{Key: key, KeyID: id, Algorithm: string(jose.EdDSA), Use: "sig"}
					signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: keys[i]}, nil)
					if err != nil {
						t.Fatal(err)
					}
					tokens[i], err = jwt.Signed(signer).Claims(map[string]any{"iss": issuer, "aud": "muster", "exp": time.Now().Add(time.Hour).Unix(), "email": "member@example.invalid", "email_verified": true, "groups": []string{org, teamGroup}}).Serialize()
					if err != nil {
						t.Fatal(err)
					}
				}
				release := make(chan struct{})
				defer close(release)
				started := make(chan struct{})
				fetches := 0
				ready := false
				client := &http.Client{Transport: issuerTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "openid-configuration") {
						return jsonResponse(map[string]any{"issuer": issuer, "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"EdDSA"}}), nil
					}
					fetches++
					if fetches == 1 {
						return jsonResponse(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{keys[0].Public()}}), nil
					}
					if !ready {
						close(started)
						return stall(r, release, body)
					}
					return jsonResponse(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{keys[1].Public()}}), nil
				})}
				v, err := identity.New(oidc.ClientContext(context.Background(), client), serveConfig(issuer))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := v.Verify(context.Background(), tokens[0]); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { _, err := v.Verify(ctx, tokens[1]); done <- err }()
				<-started
				cancel()
				if err := <-done; err == nil {
					t.Fatal("cancelled verification succeeded")
				}
				if _, err := v.Verify(context.Background(), tokens[0]); err != nil {
					t.Fatalf("cached key: %v", err)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				ready = true
				retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
				defer retryCancel()
				if _, err := v.Verify(retryCtx, tokens[1]); err != nil {
					t.Fatalf("cancelled verification left an unbounded key fetch: rotated tokens cannot recover: %v", err)
				}
				if fetches != 3 {
					t.Fatalf("key fetches = %d, want 3", fetches)
				}
				if client.Timeout != 0 {
					t.Fatal("caller client was modified")
				}
			})
		})
	}
}
