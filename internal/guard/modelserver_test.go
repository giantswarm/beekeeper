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
	ms := ModelServer{URL: "http://localhost:11434", LabTests: []string{"models-test"}}
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
}
