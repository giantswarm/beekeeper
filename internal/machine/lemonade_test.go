package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeLemonade serves loaded as Lemonade's loaded models and records the
// models it was asked to unload.
func fakeLemonade(t *testing.T, loaded ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var unloaded []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		ms := []map[string]any{}
		for _, m := range loaded {
			ms = append(ms, map[string]any{"model_name": m, "type": "llm", "recipe": "flm"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "all_models_loaded": ms})
	})
	mux.HandleFunc("GET /api/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma3-4b-FLM","size":4.5},{"id":"qwen3-4b-FLM","size":3.1}]}`))
	})
	mux.HandleFunc("POST /api/v1/unload", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		unloaded = append(unloaded, body.Model)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, &unloaded
}

func TestLemonadeModels(t *testing.T) {
	s, unloaded := fakeLemonade(t, "gemma3-4b-FLM")
	got, err := LemonadeModels(context.Background(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Server != Lemonade || got[0].Name != "gemma3-4b-FLM" || got[0].SizeMiB != 4608 {
		t.Fatalf("LemonadeModels = %+v", got)
	}
	if err := got[0].Unload(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	if len(*unloaded) != 1 || (*unloaded)[0] != "gemma3-4b-FLM" {
		t.Errorf("unloaded %v", *unloaded)
	}
}

func TestLemonadeModelsNone(t *testing.T) {
	s, _ := fakeLemonade(t)
	if got, err := LemonadeModels(context.Background(), s.URL); err != nil || got != nil {
		t.Errorf("LemonadeModels without a loaded model = %v, %v", got, err)
	}
	if got, err := LemonadeModels(context.Background(), ""); err != nil || got != nil {
		t.Errorf("LemonadeModels unconfigured = %v, %v", got, err)
	}
	s.Close()
	if _, err := LemonadeModels(context.Background(), s.URL); err == nil {
		t.Error("a server that does not answer reads as none")
	}
}
