package guard

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

func TestModelServerRefusal(t *testing.T) {
	held := func(session string) func() []lease.Holder {
		return func() []lease.Holder {
			return []lease.Holder{{Env: "agentlab-1", Session: "s1"}, {Env: modelServerLease, Session: session}}
		}
	}
	ms := ModelServer{URL: "http://localhost:11434", LemonadeURL: "http://localhost:13305", LabTests: []string{"models-test"}}
	for name, tc := range map[string]struct {
		cmd     string
		leases  func() []lease.Holder
		refused bool
	}{
		"ollama run without the lease":    {"ollama run qwen3.5:9b hi", held("other"), true},
		"ollama run under the lease":      {"ollama run qwen3.5:9b hi", held("s1"), false},
		"no lease held at all":            {"ollama pull qwen3:30b", func() []lease.Holder { return nil }, true},
		"ollama ps passes":                {"ollama ps", held("other"), false},
		"ollama stop unloads, passes":     {"ollama stop qwen3:30b", held("other"), false},
		"a generate call":                 {`curl -s http://localhost:11434/api/generate -d '{"model":"x"}'`, held("other"), true},
		"a chat call from a lab pod":      {"kubectl exec p -- curl -s http://172.21.0.1:11434/v1/chat/completions -d @b", held("other"), true},
		"reading the loaded models":       {"curl -s localhost:11434/api/ps", held("other"), false},
		"a lab test on the host models":   {"agentlab models-test --config lab.yaml", held("other"), true},
		"a lab test without model turns":  {"agentlab platform-test", held("other"), false},
		"a mention is no command":         {"grep -n 'ollama run' README.md", held("other"), false},
		"a call quoted in a comment body": {"gh issue comment 1 --body \"the hook refuses `curl -s localhost:11434/api/generate`\"", held("other"), false},
		"a chat call from a kind node":    {"docker exec agentlab-2-control-plane curl -s http://172.21.0.1:11434/api/chat -d @b", held("other"), true},
		"a generate call after a list":    {"cd /tmp && curl localhost:11434/api/generate -d @b", held("other"), true},
		"lemonade run without the lease":  {"lemonade run gemma3-4b-FLM", held("other"), true},
		"lemonade load under the lease":   {"lemonade load gemma3-4b-FLM", held("s1"), false},
		"lemonade list passes":            {"lemonade list --downloaded", held("other"), false},
		"lemonade unload passes":          {"lemonade unload gemma3-4b-FLM", held("other"), false},
		"a lemonade chat call":            {`curl -s http://localhost:13305/api/v1/chat/completions -d @b`, held("other"), true},
		"a lemonade load call":            {`curl -s -X POST localhost:13305/api/v1/load -d '{"model_name":"x"}'`, held("other"), true},
		"reading lemonade's health":       {"curl -s localhost:13305/api/v1/health", held("other"), false},
		"another port's chat call":        {"curl -s localhost:8080/v1/chat/completions -d @b", held("other"), false},
		"lease held under the desktop id": {"ollama run x", func() []lease.Holder { return []lease.Holder{{Env: modelServerLease, HostSession: "local_s1"}} }, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := Hook{Leases: tc.leases, ModelServer: func() ModelServer { return ms }}.modelServerRefusal(tc.cmd, "s1")
			if (r != "") != tc.refused {
				t.Errorf("modelServerRefusal(%q) = %q, want refused %v", tc.cmd, r, tc.refused)
			}
			if tc.refused && !strings.Contains(r, "beekeeper lease claim model-server") {
				t.Errorf("the refusal does not say how to get the lease: %q", r)
			}
		})
	}
	if r := (Hook{Leases: held("other")}).modelServerRefusal("ollama run x", "s1"); r != "" {
		t.Errorf("no model server configured, refused: %q", r)
	}
	ollamaOnly := Hook{Leases: held("other"), ModelServer: func() ModelServer { return ModelServer{URL: "http://localhost:11434"} }}
	if r := ollamaOnly.modelServerRefusal("lemonade run x", "s1"); r != "" {
		t.Errorf("no Lemonade configured, refused: %q", r)
	}
}
