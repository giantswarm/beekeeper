package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CheckLiveModel checks custom providers against their live OpenAI model list.
// Built-in providers without an endpoint remain under omp's own discovery.
func CheckLiveModel(ctx context.Context, path, selector, env string, required bool) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	var file struct {
		Providers map[string]struct {
			BaseURL string `yaml:"baseUrl"`
			APIKey  string `yaml:"apiKey"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return err
	}
	provider, model, _ := strings.Cut(selector, "/")
	p := file.Providers[provider]
	if p.BaseURL == "" {
		if required {
			return fmt.Errorf("omp provider %q needs baseUrl to check its served model", provider)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/models", nil)
	if err != nil {
		return fmt.Errorf("omp model list: invalid provider endpoint")
	}
	key := p.APIKey
	if VarName.MatchString(key) {
		key = os.Getenv(p.APIKey)
		for _, line := range strings.Split(env, "\n") {
			if v, ok := strings.CutPrefix(line, p.APIKey+"="); ok {
				key = v
			}
		}
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("omp model list unavailable for %q", selector)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading omp model list: %w", err)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &list) == nil {
		for _, m := range list.Data {
			if m.ID == model {
				return nil
			}
		}
	}
	// Do not echo credentials if an endpoint includes them in its error.
	answer := string(body)
	if key != "" {
		answer = strings.ReplaceAll(answer, key, "[redacted]")
	}
	return fmt.Errorf("omp endpoint does not serve %q (HTTP %d): %s", model, resp.StatusCode, answer)
}
