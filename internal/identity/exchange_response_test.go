package identity_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const (
	responseCredential = "synthetic-response-credential"
	responseSubject    = "synthetic-sa"
)

func TestExchangerRejectsMalformedSuccess(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"array token type", `{"access_token":"synthetic-response-credential","issued_token_type":["urn:ietf:params:oauth:token-type:access_token"],"expires_in":1800}`},
		{"object token type", `{"issued_token_type":{"value":"synthetic-response-credential"},"access_token":"stand-in","expires_in":1800}`},
		{"number token type", `{"access_token":"stand-in","issued_token_type":123,"expires_in":1800}`},
		{"invalid lifetime", `{"access_token":"stand-in","expires_in":"synthetic-response-credential"}`},
		{"invalid token", `{"access_token":{"value":"synthetic-response-credential"},"expires_in":1800}`},
		{"truncated JSON", `{"access_token":"synthetic-response-credential","expires_in":1800`},
		{"trailing JSON", `{"access_token":"stand-in","expires_in":1800} {"extra":"synthetic-response-credential"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDex{t: t, subject: responseSubject, scope: plainScopes, status: http.StatusOK, body: tc.body}
			e, _ := newExchange(t, d, "")
			for range 2 {
				token, err := e.Token(context.Background())
				if err == nil || token != "" {
					t.Fatalf("malformed token response accepted: token returned = %t, error = %v", token != "", err)
				}
				if !strings.Contains(err.Error(), "not a valid token response") || strings.Contains(err.Error(), responseCredential) {
					t.Fatalf("want a safe response-validation error, got %v", err)
				}
			}
			if got := d.n.Load(); got != 2 {
				t.Fatalf("%d requests, want both malformed responses retried", got)
			}
			// The endpoint recovers. No partially decoded response may suppress the
			// next exchange, and only this valid response may enter the cache.
			d.status = 0
			for range 2 {
				token, err := e.Token(context.Background())
				if err != nil || token != "dex-3" {
					t.Fatalf("after recovery: token returned = %t, error = %v", token != "", err)
				}
			}
			if got := d.n.Load(); got != 3 {
				t.Errorf("%d requests, want recovery cached after the third request", got)
			}
		})
	}
}

func TestExchangerMalformedErrorResponse(t *testing.T) {
	for _, body := range []string{
		"an upstream error page: " + responseCredential,
		`{"error":["synthetic-response-credential"]}`,
		`{"error":"temporarily_unavailable","expires_in":"synthetic-response-credential"}`,
	} {
		d := &fakeDex{t: t, subject: responseSubject, scope: plainScopes, status: http.StatusServiceUnavailable, body: body}
		e, _ := newExchange(t, d, "")
		token, err := e.Token(context.Background())
		if err == nil || token != "" {
			t.Fatal("an upstream error returned a token or no error")
		}
		if !strings.Contains(err.Error(), "503 Service Unavailable") || strings.Contains(err.Error(), responseCredential) {
			t.Fatalf("want a safe HTTP-status diagnostic, got %v", err)
		}
		if strings.Contains(body, "temporarily_unavailable") && !strings.Contains(err.Error(), "temporarily_unavailable") {
			t.Errorf("recognized protocol code was lost: %v", err)
		}
	}
}

func TestExchangerAcceptsOptionalResponseFields(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"stand-in","expires_in":1800}`,
		`{"access_token":"stand-in","issued_token_type":"urn:ietf:params:oauth:token-type:id_token","expires_in":1800,"extension":{"items":[1,true,null]}}`,
	} {
		d := &fakeDex{t: t, subject: responseSubject, scope: plainScopes, status: http.StatusOK, body: body}
		e, _ := newExchange(t, d, "")
		token, err := e.Token(context.Background())
		if err != nil || token != "stand-in" {
			t.Fatalf("valid response: token returned = %t, error = %v", token != "", err)
		}
	}
}
