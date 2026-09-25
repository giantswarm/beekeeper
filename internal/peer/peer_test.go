package peer

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestVerdict(t *testing.T) {
	for reply, want := range map[string]error{
		`{"success":true,"message":"… queued there","msg_id":"1"}`: nil,
		"No agent named \"x\" is reachable":                        ErrUnreachable,
	} {
		if err := verdict("x", reply); !errors.Is(err, want) && (want != nil || err != nil) {
			t.Errorf("verdict(%q) = %v, want %v", reply, err, want)
		}
	}
	if verdict("x", `{"success":false,"error":"refused"}`) == nil {
		t.Error("an unknown reply counts as delivered")
	}
}

func TestParse(t *testing.T) {
	out := []byte(`{"type":"system"}
{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"delivered"}]}]}}
{"type":"result","total_cost_usd":0.01}
`)
	r, err := parse(out)
	if err != nil || r.Reply != "delivered" || r.CostUSD != 0.01 {
		t.Fatalf("parse = %+v, %v", r, err)
	}
	if _, err := parse([]byte(`{"type":"result"}`)); err == nil {
		t.Error("a turn without SendMessage parsed")
	}
}

func TestSenderEnv(t *testing.T) {
	env := senderEnv([]string{"HOME=/h", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=s"})
	if len(env) != 2 || env[0] != "HOME=/h" || env[1] != "MAX_THINKING_TOKENS=0" {
		t.Fatalf("senderEnv = %v", env)
	}
}

// TestLive sends a message to the running session BEEKEEPER_PEER_TEST names
// (its ListAgents name) and BEEKEEPER_PEER_TEST_MESSAGE (default: the
// keep-awake); it runs only when set.
func TestLive(t *testing.T) {
	to := os.Getenv("BEEKEEPER_PEER_TEST")
	if to == "" {
		t.Skip("BEEKEEPER_PEER_TEST names no session")
	}
	msg := os.Getenv("BEEKEEPER_PEER_TEST_MESSAGE")
	if msg == "" {
		msg = "beekeeper keep-awake: no action, reply ok"
	}
	r, err := Sender{Dir: t.TempDir()}.Send(context.Background(), to, msg)
	t.Logf("reply %q cost $%.4f in %s", r.Reply, r.CostUSD, r.Elapsed.Round(100_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Sender{Dir: t.TempDir()}).Send(context.Background(), to+" (absent)", msg); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("an absent session: %v, want unreachable", err)
	}
}
