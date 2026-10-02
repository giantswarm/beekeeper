package cmd

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
)

// TestEnvtestServeConversations is the guide's Slack conversation on serve's
// side: the guide writes to its person, the person's reply (klaus-gateway's
// send_message as the person) lands in their mailbox stamped with Slack,
// and the guide's answer goes into the same thread.
func TestEnvtestServeConversations(t *testing.T) {
	e := newServeEnv(t)
	gw := &fakeGateway{gone: map[string]bool{}}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ana := e.as(t, e.token(t, anaEmail, teamGroup))
	bo := e.as(t, e.token(t, boEmail, teamGroup))
	guide := func(args map[string]any) map[string]any {
		args[paramAgent], args[paramHost] = anaAgent, lab
		return args
	}

	e.expect(t, ana, "converse", guide(map[string]any{paramText: "hello"}), true, "no klaus-gateway is configured")
	e.srv.cfg.Serve.Gateway = config.Gateway{URL: srv.URL, TokenFile: tokenFile, SendTool: "x_beekeeper_send_message"}
	e.srv.gw = newGateway(e.srv.cfg.Serve.Gateway)
	e.expect(t, ana, "converse", guide(map[string]any{paramText: "hello"}), true, "agents_register first")
	e.expect(t, ana, "converse", map[string]any{paramText: "hello"}, true, "agent and host are required")
	e.expect(t, ana, "agents_register", guide(map[string]any{}), false, "register: ana-agent idle")
	e.expect(t, bo, "converse", guide(map[string]any{paramText: "hello"}), true, "only its person converses as it")

	// The first message opens the thread, bound to the guide's address.
	got := e.expect(t, ana, "converse", guide(map[string]any{paramText: "Which lane for muster?"}), false, "opened conversation D1-1.0")
	opened, _ := gw.conversations()
	if len(opened) != 1 || got["conversation"] != "D1-1.0" {
		t.Fatalf("opened %+v, %v", opened, got)
	}
	if c := opened[0]; c.Person != anaEmail || c.From != anaAgent+" on "+lab || c.Text != "Which lane for muster?" ||
		c.Reply.Tool != "x_beekeeper_send_message" || c.Reply.Arguments["to"] != anaAddress {
		t.Fatalf("conversation %+v", c)
	}

	// The person's reply, as klaus-gateway sends it through muster as them.
	reply := a2a("slack-D1-2.0", "its own lane")
	reply["contextId"], reply["metadata"] = "D1-1.0", map[string]any{"source": "slack"}
	e.expect(t, ana, "send_message", map[string]any{"to": anaAddress, paramMessage: reply}, false, "delivery")
	msgs := e.expect(t, ana, "receive_messages", map[string]any{}, false, "slack-D1-2.0")["messages"].([]any)
	env := msgs[0].(map[string]any)["envelope"].(map[string]any)
	if from := env["from"].(map[string]any); from["person"] != anaEmail || from["source"] != "slack" ||
		env[paramMessage].(map[string]any)["contextId"] != "D1-1.0" {
		t.Fatalf("the envelope %v", env)
	}
	// Another person cannot reach ana's guide; nor anyone a guide not running.
	e.expect(t, bo, "send_message", map[string]any{"to": anaAddress, paramMessage: a2a("slack-D2-1.0", "x")}, true, "is not running")
	e.expect(t, ana, "send_message", map[string]any{"to": "local:" + laptop + "/guide", paramMessage: a2a("slack-D1-3.0", "x")}, true, "is not running")

	// The answer goes into the same thread; once the gateway dropped it, a
	// new one opens and is kept.
	e.expect(t, ana, "converse", guide(map[string]any{paramText: "Noted; I hand it to the supervisor."}), false, "posted to conversation D1-1.0")
	gw.mu.Lock()
	gw.gone["D1-1.0"] = true
	gw.mu.Unlock()
	e.expect(t, ana, "converse", guide(map[string]any{paramText: "A new question"}), false, "opened conversation D1-2.0")
	e.expect(t, ana, "converse", guide(map[string]any{paramText: "and more"}), false, "posted to conversation D1-2.0")
	if _, said := gw.conversations(); len(said) != 2 || said[0] != "D1-1.0: Noted; I hand it to the supervisor." || said[1] != "D1-2.0: and more" {
		t.Fatalf("said %q", said)
	}

	// A person Slack does not know: refused with the gateway's reason.
	gw.mu.Lock()
	gw.refuses, gw.gone["D1-2.0"] = true, true
	gw.mu.Unlock()
	e.expect(t, ana, "converse", guide(map[string]any{paramText: "x"}), true, "no Slack user has this email")
}
