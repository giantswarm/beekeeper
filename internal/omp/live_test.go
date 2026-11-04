package omp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
)

func TestLiveModel(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"served", `{"data":[{"id":"wanted"}]}`, 200, false},
		{"missing", `{"data":[{"id":"other"}]}`, 200, true},
		{"failure", `{"error":"temporarily unavailable"}`, 503, true},
		{"invalid", `not json`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("missing credential")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "models.yml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("providers:\n  local:\n    baseUrl: %s/v1\n    apiKey: TEST_KEY\n", srv.URL)), 0600); err != nil {
				t.Fatal(err)
			}
			err := CheckLiveModel(t.Context(), path, "local/wanted", "TEST_KEY=test-key\n", true)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("missing endpoint answer: %v", err)
			}
		})
	}
}

func TestServedModelFromReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	raw := `{"type":"session","id":"test","cwd":"/work"}
{"type":"model_change","model":"configured"}
{"type":"message","message":{"role":"assistant","model":"served","content":[]}}
{"type":"model_change","model":"next"}
{"type":"message","message":{"role":"assistant","model":"failed","isError":true}}
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	s := &claude.Session{}
	fillFromFile(s, path, true)
	if s.Model != "served" {
		t.Fatalf("model = %q", s.Model)
	}
}

func TestLiveModelUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	path := filepath.Join(t.TempDir(), "models.yml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("providers:\n  local:\n    baseUrl: %s/v1\n", srv.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLiveModel(t.Context(), path, "local/wanted", "", true); err == nil {
		t.Fatal("unreachable endpoint admitted")
	}
	if err := CheckLiveModel(t.Context(), path, "unknown/wanted", "", true); err == nil {
		t.Fatal("serving resource without endpoint admitted")
	}
}
