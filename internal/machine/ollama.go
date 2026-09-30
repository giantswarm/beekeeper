package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OllamaModel is a model the host's ollama holds in memory.
type OllamaModel struct {
	Name    string `json:"name"`
	SizeMiB int    `json:"sizeMiB"`
	// GPUMiB is the part on the GPU: on an APU, GTT.
	GPUMiB    int       `json:"gpuMiB"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Client is who sent the request that loaded it, as the ollama
	// journal shows it or, while that request still runs, as the one
	// client connected to the server: an address, with the kind node's
	// name when one has it.
	Client string `json:"client,omitempty"`
}

// OllamaModels asks the ollama at url which models it holds (GET /api/ps)
// and finds each one's loading client in the journal of unit. A load whose
// request still runs is not in the journal yet: its client is the one peer
// connected to url's port, and none when several are. An empty url means
// none is watched.
func OllamaModels(ctx context.Context, url, unit string) ([]OllamaModel, error) {
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
	if err := ollamaCall(ctx, url, "/api/ps", nil, &ps); err != nil {
		return nil, err
	}
	var out []OllamaModel
	for _, m := range ps.Models {
		out = append(out, OllamaModel{Name: m.Name, SizeMiB: int(m.Size >> 20), GPUMiB: int(m.SizeVRAM >> 20), ExpiresAt: m.ExpiresAt})
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
		if ollamaCall(ctx, url, "/api/show", map[string]string{"model": out[i].Name}, &show) != nil {
			continue
		}
		ip, loaded := loaders[modelBlob(show.Modelfile)]
		if loaded && ip == "" {
			ip = running()
		}
		if ip != "" {
			out[i].Client = ip
			if n := names[ip]; n != "" {
				out[i].Client = ip + " " + n
			}
		}
	}
	return out, nil
}

// UnloadOllama asks the ollama at url to drop model from memory now; its
// weights stay on disk and the next request loads it again.
func UnloadOllama(ctx context.Context, url, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out struct{}
	return ollamaCall(ctx, url, "/api/generate", map[string]any{"model": model, "keep_alive": 0, "stream": false}, &out)
}

// onlyPeer is the address of the one client connected to the port of the
// server at url, "" when none or several are.
func onlyPeer(url string) string {
	u, err := neturl.Parse(url)
	if err != nil {
		return ""
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return ""
	}
	if peers := establishedPeers(port); len(peers) == 1 {
		return peers[0]
	}
	return ""
}

// Node is the kind node's name in Client, "" when the client is none.
func (m OllamaModel) Node() string {
	_, name, _ := strings.Cut(m.Client, " ")
	return name
}

// ollamaClient keeps no idle connection to the server: one would read as a
// client of a running load, to this process and every other beekeeper.
var ollamaClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

func ollamaCall(ctx context.Context, url, path string, body, into any) error {
	method, payload := http.MethodGet, []byte(nil)
	if body != nil {
		method = http.MethodPost
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(url, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	resp, err := ollamaClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

var (
	modelFrom = regexp.MustCompile(`(?m)^FROM (\S*/blobs/\S+)`)
	loadLine  = regexp.MustCompile(`^time=(\S+) .*msg="starting llama-server" cmd=".*?--model (\S+)`)
	ginLine   = regexp.MustCompile(`^\[GIN\] (\d{4}/\d\d/\d\d - \d\d:\d\d:\d\d) \|\s*\d+ \|\s*(\S+) \|\s*(\S+) \| POST `)
)

// modelBlob is the weights blob a model's modelfile starts FROM.
func modelBlob(modelfile string) string {
	if m := modelFrom.FindStringSubmatch(modelfile); m != nil {
		return m[1]
	}
	return ""
}

// ParseOllamaLoads maps each model blob to the address of the client whose
// request loaded it last: the first POST logged after the load that started
// before it (gin logs a request when it ends, with its latency). A blob
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
		if m == nil || len(open) == 0 {
			continue
		}
		// gin prints the server's local time without a zone: the load
		// lines carry it.
		end, err := time.ParseInLocation("2006/01/02 - 15:04:05", m[1], open[0].at.Location())
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

// kindNodeIPs maps the kind network's addresses to their containers' names.
func kindNodeIPs(ctx context.Context) map[string]string {
	out := map[string]string{}
	raw, err := exec.CommandContext(ctx, "docker", "network", "inspect", "kind",
		"-f", `{{range .Containers}}{{.IPv4Address}} {{.Name}}{{"\n"}}{{end}}`).Output()
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if addr, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			ip, _, _ := strings.Cut(addr, "/")
			out[ip] = name
		}
	}
	return out
}
