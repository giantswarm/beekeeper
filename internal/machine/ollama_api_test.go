package machine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
)

func TestOllamaModelsPartialAttribution(t *testing.T) {
	// Prevent kind network lookup from invoking Docker on the test machine.
	t.Setenv("PATH", t.TempDir())
	expires := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	const discovery = `{"models":[
 {"name":"first:latest","size":10485760,"size_vram":6291456,"expires_at":"2026-10-01T12:30:00Z"},
 {"name":"missing:latest","size":20971520,"size_vram":12582912,"expires_at":"2026-10-01T12:30:00Z"},
 {"name":"last:latest","size":31457280,"size_vram":18874368,"expires_at":"2026-10-01T12:30:00Z"}]}`
	const journal = `time=2026-10-01T12:00:00Z level=INFO msg="starting llama-server" cmd="runner --model /models/blobs/sha256-known --port 40001"
[GIN] 2026/10/01 - 12:00:02 | 200 | 3s | 192.0.2.1 | POST "/api/chat"`
	for _, tc := range []struct {
		name          string
		journalErr    error
		malformedShow bool
	}{
		{name: "journal unavailable", journalErr: os.ErrPermission},
		{name: "model disappeared"},
		{name: "malformed model metadata", malformedShow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var shown []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/ps":
					if r.Method != http.MethodGet {
						t.Errorf("discovery method = %s", r.Method)
					}
					fmt.Fprint(w, discovery)
				case "/api/show":
					if r.Method != http.MethodPost {
						t.Errorf("metadata method = %s", r.Method)
					}
					var request struct {
						Model string `json:"model"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						http.Error(w, "invalid request", 400)
						return
					}
					shown = append(shown, request.Model)
					if request.Model == "missing:latest" {
						if tc.malformedShow {
							fmt.Fprint(w, `{`)
						} else {
							http.Error(w, "model unavailable", http.StatusNotFound)
						}
						return
					}
					fmt.Fprint(w, `{"modelfile":"FROM /models/blobs/sha256-known\n"}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			logCalls := 0
			got, err := machine.OllamaModels(context.Background(), server.URL, "ollama.service", func(_ context.Context, unit, filter string) ([]byte, error) {
				logCalls++
				if unit != "ollama.service" || filter == "" {
					t.Errorf("journal arguments = %q, %q", unit, filter)
				}
				return []byte(journal), tc.journalErr
			})
			// Wait for all handlers before inspecting their recorded requests.
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := []machine.HostModel{
				{Server: machine.Ollama, Name: "first:latest", SizeMiB: 10, GPUMiB: 6, ExpiresAt: expires},
				{Server: machine.Ollama, Name: "missing:latest", SizeMiB: 20, GPUMiB: 12, ExpiresAt: expires},
				{Server: machine.Ollama, Name: "last:latest", SizeMiB: 30, GPUMiB: 18, ExpiresAt: expires},
			}
			if tc.journalErr == nil {
				want[0].Client = "192.0.2.1"
				want[2].Client = "192.0.2.1"
			}
			if !slices.Equal(got, want) {
				t.Errorf("models = %+v, want %+v", got, want)
			}
			if logCalls != 1 {
				t.Errorf("journal calls = %d, want 1", logCalls)
			}
			var wantShown []string
			if tc.journalErr == nil {
				wantShown = []string{"first:latest", "missing:latest", "last:latest"}
			}
			if !slices.Equal(shown, wantShown) {
				t.Errorf("metadata requests = %v, want %v", shown, wantShown)
			}
		})
	}
}

func TestOllamaModelsDiscovery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"unavailable", "http error", "malformed", "canceled", "empty"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/ps" || r.Method != http.MethodGet {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				switch name {
				case "http error":
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case "malformed":
					fmt.Fprint(w, `{"models":[`)
				default:
					fmt.Fprint(w, `{"models":[]}`)
				}
			}))
			defer server.Close()
			if name == "unavailable" {
				server.Close()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			got, err := machine.OllamaModels(ctx, server.URL, "ollama.service", func(context.Context, string, string) ([]byte, error) {
				t.Error("journal must not be read without loaded models")
				return nil, os.ErrPermission
			})
			if name == "empty" {
				if err != nil || len(got) != 0 {
					t.Fatalf("empty discovery = %+v, %v", got, err)
				}
			} else {
				if err == nil || got != nil {
					t.Fatalf("failed discovery = %+v, %v; want nil models and an error", got, err)
				}
				if name == "canceled" && !errors.Is(err, context.Canceled) {
					t.Errorf("error = %v, want context.Canceled", err)
				}
			}
		})
	}
}
