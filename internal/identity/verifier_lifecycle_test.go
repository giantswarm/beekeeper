package identity_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/giantswarm/beekeeper/internal/identity"
)

const lifecycleIssuer = "https://issuer.example"

type issuerTransport func(*http.Request) (*http.Response, error)

func (f issuerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func issuerResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func lifecycleKey(t *testing.T, id string) (jose.Signer, jose.JSONWebKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key, KeyID: id, Algorithm: string(jose.RS256), Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	return signer, jwk.Public()
}

func lifecycleToken(t *testing.T, signer jose.Signer) string {
	t.Helper()
	raw, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": lifecycleIssuer, "sub": "synthetic-person", "aud": "muster", "exp": time.Now().Add(time.Hour).Unix(),
		"email": ana, "email_verified": true, "groups": []string{org, teamGroup},
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// stalledIssuerBody models an issuer that sends headers but never finishes
// the response. Both its read and the header-only fixture honor HTTP cancellation.
type stalledIssuerBody struct {
	ctx     context.Context
	release <-chan struct{}
	aborted chan struct{}
	closed  atomic.Bool
}

func (b *stalledIssuerBody) Read([]byte) (int, error) {
	return 0, waitForIssuer(b.ctx, b.release, b.aborted)
}
func (b *stalledIssuerBody) Close() error { b.closed.Store(true); return nil }

func waitForIssuer(ctx context.Context, release <-chan struct{}, aborted chan struct{}) error {
	select {
	case <-ctx.Done():
		close(aborted)
		return ctx.Err()
	case <-release:
		return errors.New("fixture released")
	}
}

func TestNewBoundsIssuerDiscoveryRequests(t *testing.T) {
	for _, tc := range []struct {
		name                string
		body, defaultClient bool
		timeout             time.Duration
	}{
		{name: "default client headers", defaultClient: true},
		{name: "default client body", body: true, defaultClient: true},
		{name: "context client headers"},
		{name: "context client body", body: true},
		{name: "longer client timeout", timeout: 2 * time.Minute},
		{name: "shorter client timeout", timeout: 5 * time.Second, body: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release, aborted := make(chan struct{}), make(chan struct{})
				defer close(release)
				var body *stalledIssuerBody
				client := &http.Client{Timeout: tc.timeout, Transport: issuerTransport(func(r *http.Request) (*http.Response, error) {
					if !tc.body {
						return nil, waitForIssuer(r.Context(), release, aborted)
					}
					body = &stalledIssuerBody{ctx: r.Context(), release: release, aborted: aborted}
					response := issuerResponse("")
					response.Body = body
					return response, nil
				})}
				ctx := context.Background()
				if tc.defaultClient {
					previous := http.DefaultClient
					http.DefaultClient = client
					defer func() { http.DefaultClient = previous }()
				} else {
					ctx = oidc.ClientContext(ctx, client)
				}
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				result := make(chan error, 1)
				start := time.Now()
				go func() { _, err := identity.New(ctx, serveConfig(lifecycleIssuer)); result <- err }()
				select {
				case err := <-result:
					if err == nil {
						t.Fatal("stalled discovery succeeded")
					}
				case <-time.After(time.Minute):
					cancel()
					<-result
					t.Error("discovery did not bound the issuer request")
				}
				if tc.timeout > 0 && tc.timeout < time.Minute && time.Since(start) > tc.timeout {
					t.Error("New extended the caller's shorter timeout")
				}
				if client.Timeout != tc.timeout {
					t.Error("New changed the caller's HTTP client")
				}
				select {
				case <-aborted:
				default:
					t.Error("issuer request was not cancelled")
				}
				if tc.body && (body == nil || !body.closed.Load()) {
					t.Error("issuer response body was not closed")
				}
			})
		})
	}
}

func TestVerifyRecoversAfterStalledKeyFetch(t *testing.T) {
	oldSigner, oldKey := lifecycleKey(t, "old")
	newSigner, newKey := lifecycleKey(t, "new")
	oldKeys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{oldKey}})
	if err != nil {
		t.Fatal(err)
	}
	newKeys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{oldKey, newKey}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stallBody := range []bool{false, true} {
		name := "headers"
		if stallBody {
			name = "body"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				aborted := make(chan struct{})
				var body *stalledIssuerBody
				var keyRequests atomic.Int32
				client := &http.Client{Transport: issuerTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/.well-known/openid-configuration" {
						return issuerResponse(`{"issuer":"https://issuer.example","jwks_uri":"https://issuer.example/keys","id_token_signing_alg_values_supported":["RS256"]}`), nil
					}
					switch keyRequests.Add(1) {
					case 1:
						return issuerResponse(string(oldKeys)), nil
					case 2:
						if !stallBody {
							return nil, waitForIssuer(r.Context(), release, aborted)
						}
						body = &stalledIssuerBody{ctx: r.Context(), release: release, aborted: aborted}
						response := issuerResponse("")
						response.Body = body
						return response, nil
					default:
						return issuerResponse(string(newKeys)), nil
					}
				})}
				v, err := identity.New(oidc.ClientContext(context.Background(), client), serveConfig(lifecycleIssuer))
				if err != nil {
					t.Fatal(err)
				}
				oldToken, newToken := lifecycleToken(t, oldSigner), lifecycleToken(t, newSigner)
				if _, err := v.Verify(context.Background(), oldToken); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := v.Verify(ctx, newToken); result <- err }()
				synctest.Wait()
				cancel()
				var refusal *identity.RefusedError
				if err := <-result; !errors.As(err, &refusal) {
					t.Fatalf("cancelled verification: %v, want refusal", err)
				}
				if _, err := v.Verify(context.Background(), oldToken); err != nil {
					t.Fatalf("cached key during outage: %v", err)
				}
				// Time is virtual: let the upstream deadline expire after the original
				// caller has gone, without releasing the fixture's stalled response.
				<-time.After(time.Minute)
				select {
				case <-aborted:
				default:
					t.Error("cancelled verification left an unbounded key fetch: rotated tokens cannot recover")
					return
				}
				if stallBody && (body == nil || !body.closed.Load()) {
					t.Error("stalled key response body was not closed")
				}
				who, err := v.Verify(context.Background(), newToken)
				if err != nil || who.Email != ana || who.Team != bumblebee {
					t.Fatalf("after issuer recovery: caller %+v, error %v", who, err)
				}
				if got := keyRequests.Load(); got != 3 {
					t.Errorf("key requests = %d, want old key, stalled refresh, successful refresh", got)
				}
				if client.Timeout != 0 {
					t.Error("New changed the caller's HTTP client")
				}
			})
		})
	}
}
