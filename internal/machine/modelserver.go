package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The host's model servers, which load models into the iGPU's memory.
const (
	Ollama   = "ollama"
	Lemonade = "lemonade"
)

// HostModel is a model one of the host's model servers holds in memory.
type HostModel struct {
	// Server is the model server holding it: Ollama or Lemonade.
	Server  string `json:"server"`
	Name    string `json:"name"`
	SizeMiB int    `json:"sizeMiB"`
	// GPUMiB is the part on the GPU: on an APU, GTT.
	GPUMiB    int       `json:"gpuMiB,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// Client is who sent the request that loaded it, as the server's log
	// shows it or, while that request still runs, as the one client
	// connected to the server: an address, with the kind node's name when
	// one has it.
	Client string `json:"client,omitempty"`
}

// Label is the model's name after its server's, as the watch prints it.
func (m HostModel) Label() string {
	return strings.TrimSpace(m.Server + " " + m.Name)
}

// Node is the kind node's name in Client, "" when the client is none.
func (m HostModel) Node() string {
	_, name, _ := strings.Cut(m.Client, " ")
	return name
}

// Unload asks the model server holding m to drop it from memory now; its
// weights stay on disk and the next request loads it again. url is that
// server's API.
func (m HostModel) Unload(ctx context.Context, url string) error {
	if m.Server == Lemonade {
		return UnloadLemonade(ctx, url, m.Name)
	}
	return UnloadOllama(ctx, url, m.Name)
}

// clientOf is addr with the kind node's name when names has one for it.
func clientOf(addr string, names map[string]string) string {
	if n := names[addr]; n != "" && addr != "" {
		return addr + " " + n
	}
	return addr
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

// serverClient keeps no idle connection to a model server: one would read
// as a client of a running load, to this process and every other beekeeper.
var serverClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// serverCall sends one request to the model server at url: a GET, or a
// POST of body's JSON when body is set. It decodes the answer into into.
func serverCall(ctx context.Context, url, path string, body, into any) error {
	method, payload := http.MethodGet, []byte(nil)
	if body != nil {
		method = http.MethodPost
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(url, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	resp, err := serverClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
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
