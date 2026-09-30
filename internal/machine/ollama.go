package machine

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// OllamaModels asks the ollama at url which models it holds (GET /api/ps)
// and finds each one's loading client in the journal of unit. A load whose
// request still runs is not in the journal yet: its client is the one peer
// connected to url's port, and none when several are. An empty url means
// none is watched.
func OllamaModels(ctx context.Context, url, unit string) ([]HostModel, error) {
	if url == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var ps struct {
		Models []struct {
			Name      string    `json:"name"`
			Size      int64     `json:"size"`
			SizeVRAM  int64     `json:"size_vram"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := serverCall(ctx, url, "/api/ps", nil, &ps); err != nil {
		return nil, err
	}
	var out []HostModel
	for _, m := range ps.Models {
		out = append(out, HostModel{Server: Ollama, Name: m.Name, SizeMiB: int(m.Size >> 20), GPUMiB: int(m.SizeVRAM >> 20), ExpiresAt: m.ExpiresAt})
	}
	if len(out) == 0 || unit == "" {
		return out, nil
	}
	journal, err := exec.CommandContext(ctx, "journalctl", "-u", unit, "--no-pager", "-o", "cat", "--since", "-24h", "-g", `starting llama-server|\[GIN\].*POST`).Output() //nolint:gosec // the configured unit name
	if err != nil {
		return out, nil
	}
	loaders := ParseOllamaLoads(string(journal))
	names := kindNodeIPs(ctx)
	running := sync.OnceValue(func() string { return onlyPeer(url) })
	for i := range out {
		var show struct {
			Modelfile string `json:"modelfile"`
		}
		if serverCall(ctx, url, "/api/show", map[string]string{"model": out[i].Name}, &show) != nil {
			continue
		}
		ip, loaded := loaders[modelBlob(show.Modelfile)]
		if loaded && ip == "" {
			ip = running()
		}
		out[i].Client = clientOf(ip, names)
	}
	return out, nil
}

// UnloadOllama asks the ollama at url to drop model from memory now; its
// weights stay on disk and the next request loads it again.
func UnloadOllama(ctx context.Context, url, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out struct{}
	return serverCall(ctx, url, "/api/generate", map[string]any{"model": model, "keep_alive": 0, "stream": false}, &out)
}

var (
	modelFrom = regexp.MustCompile(`(?m)^FROM (\S*/blobs/\S+)`)
	loadLine  = regexp.MustCompile(`^time=(\S+) .*msg="starting llama-server" cmd=".*?--model (\S+)`)
	ginLine   = regexp.MustCompile(`^\[GIN\] (\d{4}/\d\d/\d\d - \d\d:\d\d:\d\d) \|\s*\d+ \|\s*(\S+) \|\s*(\S+) \| POST\s+"([^"?]*)`)
	// loadPath is a request that runs a model and so can load one; the
	// others (/api/show, which beekeeper itself sends, /api/pull, ...) never
	// do.
	loadPath = regexp.MustCompile(`^/(?:api/(?:generate|chat|embed|embeddings)|v1/.+)$`)
)

// modelBlob is the weights blob a model's modelfile starts FROM.
func modelBlob(modelfile string) string {
	if m := modelFrom.FindStringSubmatch(modelfile); m != nil {
		return m[1]
	}
	return ""
}

// ParseOllamaLoads maps each model blob to the address of the client whose
// request loaded it last: the first model request logged after the load
// that started before it (gin logs a request when it ends, with its
// latency). A blob
// whose last load's request is still running maps to "": an earlier load's
// client is not its.
func ParseOllamaLoads(journal string) map[string]string {
	out := map[string]string{}
	type load struct {
		blob string
		at   time.Time
	}
	var open []load
	for line := range strings.SplitSeq(journal, "\n") {
		if m := loadLine.FindStringSubmatch(line); m != nil {
			if at, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				open = append(open, load{blob: m[2], at: at})
				out[m[2]] = ""
			}
			continue
		}
		m := ginLine.FindStringSubmatch(line)
		if m == nil || len(open) == 0 || !loadPath.MatchString(m[4]) {
			continue
		}
		// gin prints the server's local time without a zone: the latest
		// load line carries the zone the server runs in now, which an
		// older one may not (the machine's zone changed since).
		end, err := time.ParseInLocation("2006/01/02 - 15:04:05", m[1], open[len(open)-1].at.Location())
		if err != nil {
			continue
		}
		took, err := time.ParseDuration(m[2])
		if err != nil {
			continue
		}
		// gin truncates to the second: end is the earliest the request ended.
		start := end.Add(-took)
		rest := open[:0]
		for _, l := range open {
			if !start.After(l.at) {
				out[l.blob] = m[3]
			} else {
				rest = append(rest, l)
			}
		}
		open = rest
	}
	return out
}
