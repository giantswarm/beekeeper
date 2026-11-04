package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOllamaModelsAttributionFailure(t *testing.T) {
	// Docker is optional enrichment. An empty search path mocks its absence
	// and ensures these fixtures never reach a real Docker daemon.
	t.Setenv("PATH", t.TempDir())
	const (
		firstModel   = "first"
		secondModel  = "second"
		secondClient = "172.21.0.4"
	)
	const journal = `time=2026-11-01T12:00:00Z level=INFO msg="starting llama-server" cmd="llama-server --model /fixture/blobs/first --port 40001"
[GIN] 2026/11/01 - 12:00:10 | 200 | 11s | 172.21.0.3 | POST "/api/chat"
time=2026-11-01T12:01:00Z level=INFO msg="starting llama-server" cmd="llama-server --model /fixture/blobs/second --port 40002"
[GIN] 2026/11/01 - 12:01:10 | 200 | 11s | 172.21.0.4 | POST "/api/chat"`
	for _, tc := range []struct {
		name          string
		logErr        error
		showFailure   bool
		malformedShow bool
		clients       [2]string
	}{
		{name: "case 0: service journal unavailable", logErr: errors.New("fixture journal unavailable")},
		{name: "case 1: one model disappears before show", showFailure: true, clients: [2]string{"", secondClient}},
		{name: "case 2: one model has malformed metadata", malformedShow: true, clients: [2]string{"", secondClient}},
		{name: "case 3: Docker unavailable leaves known client addresses", clients: [2]string{"172.21.0.3", secondClient}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"models":[
{"name":"first","size":3145728,"size_vram":2097152,"expires_at":"2026-11-01T12:30:00Z"},
{"name":"second","size":6291456,"size_vram":5242880,"expires_at":"2026-11-01T12:30:00Z"}]}`))
			})
			mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Model != firstModel && req.Model != secondModel) {
					http.Error(w, "invalid fixture model request", http.StatusBadRequest)
					return
				}
				if req.Model == firstModel && tc.showFailure {
					http.NotFound(w, r)
					return
				}
				if req.Model == firstModel && tc.malformedShow {
					_, _ = w.Write([]byte(`{"modelfile":`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"modelfile": "FROM /fixture/blobs/" + req.Model + "\n"})
			})
			s := httptest.NewServer(mux)
			defer s.Close()
			log := func(_ context.Context, unit, _ string) ([]byte, error) {
				if unit != "fixture-ollama" {
					t.Errorf("service log unit = %q, want fixture-ollama", unit)
				}
				return []byte(journal), tc.logErr
			}
			got, err := OllamaModels(context.Background(), s.URL, "fixture-ollama", log)
			if err != nil || len(got) != 2 {
				t.Fatalf("loaded models = %+v, %v, want both despite attribution failure", got, err)
			}
			want := []HostModel{
				{Server: Ollama, Name: firstModel, SizeMiB: 3, GPUMiB: 2, Client: tc.clients[0]},
				{Server: Ollama, Name: secondModel, SizeMiB: 6, GPUMiB: 5, Client: tc.clients[1]},
			}
			expires := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
			for i, m := range got {
				w := want[i]
				if m.Server != w.Server || m.Name != w.Name || m.SizeMiB != w.SizeMiB || m.GPUMiB != w.GPUMiB || m.Client != w.Client || !m.ExpiresAt.Equal(expires) {
					t.Errorf("model %d = %+v, want %+v expiring %s", i, m, w, expires)
				}
			}
		})
	}
}

func TestOllamaModelsDiscoveryFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "case 0: successful empty server", status: http.StatusOK, body: `{"models":[]}`},
		{name: "case 1: unavailable server", status: http.StatusServiceUnavailable, body: `{"error":"fixture unavailable"}`, wantErr: true},
		{name: "case 2: malformed model listing", status: http.StatusOK, body: `{"models":`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			log := func(context.Context, string, string) ([]byte, error) {
				t.Error("service log called without a loaded model listing")
				return nil, nil
			}
			got, err := OllamaModels(context.Background(), s.URL, "fixture-ollama", log)
			if got != nil || (err != nil) != tc.wantErr {
				t.Errorf("model listing = %+v, %v, want no models and error %v", got, err, tc.wantErr)
			}
		})
	}
}

func TestOllamaModelsCanceledDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer s.Close()
	got, err := OllamaModels(ctx, s.URL, "", nil)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled discovery = %+v, %v, want a cancellation error", got, err)
	}
}
