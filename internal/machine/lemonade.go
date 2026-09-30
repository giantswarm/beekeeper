package machine

import (
	"context"
	"time"
)

// LemonadeModels asks the Lemonade Server at url which models it holds
// (GET /api/v1/health) and how large each one's weights are (GET
// /api/v1/models, in GB). Lemonade logs no client address: a model's client
// is the one peer connected to url's port, and none when several are. An
// empty url means none is watched.
func LemonadeModels(ctx context.Context, url string) ([]HostModel, error) {
	if url == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var health struct {
		Loaded []struct {
			Name string `json:"model_name"`
		} `json:"all_models_loaded"`
	}
	if err := serverCall(ctx, url, "/api/v1/health", nil, &health); err != nil {
		return nil, err
	}
	if len(health.Loaded) == 0 {
		return nil, nil
	}
	var list struct {
		Data []struct {
			ID     string  `json:"id"`
			SizeGB float64 `json:"size"`
		} `json:"data"`
	}
	sizes := map[string]int{}
	if serverCall(ctx, url, "/api/v1/models", nil, &list) == nil {
		for _, m := range list.Data {
			sizes[m.ID] = int(m.SizeGB * 1024)
		}
	}
	client := clientOf(onlyPeer(url), kindNodeIPs(ctx))
	out := make([]HostModel, len(health.Loaded))
	for i, m := range health.Loaded {
		out[i] = HostModel{Server: Lemonade, Name: m.Name, SizeMiB: sizes[m.Name], Client: client}
	}
	return out, nil
}

// UnloadLemonade asks the Lemonade Server at url to drop model from memory
// now (POST /api/v1/unload); its weights stay on disk.
func UnloadLemonade(ctx context.Context, url, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out struct{}
	return serverCall(ctx, url, "/api/v1/unload", map[string]string{"model_name": model}, &out)
}
