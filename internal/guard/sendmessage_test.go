package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decideSend(t *testing.T, h Hook, to string) *decision {
	t.Helper()
	raw, _ := json.Marshal(struct {
		Tool  string         `json:"tool_name"`
		Input map[string]any `json:"tool_input"`
	}{SendMessageTool, map[string]any{"to": to, "message": "hi", "summary": "s"}})
	var ev map[string]any
	_ = json.Unmarshal(raw, &ev)
	return decideEvent(t, h, ev)
}

func TestSendMessageToARunningDesktopSessionGoesByName(t *testing.T) {
	var asked string
	h := Hook{Peer: func(host string) (string, error) { asked = host; return "gpm worker", nil }}
	d := decideSend(t, h, "local_abc")
	if d == nil || d.PermissionDecision != decisionAllow {
		t.Fatalf("want a redirect, got %+v", d)
	}
	if asked != "local_abc" || d.UpdatedInput["to"] != "gpm worker" || d.UpdatedInput["message"] != "hi" || d.UpdatedInput["summary"] != "s" {
		t.Fatalf("redirect %+v (asked %q)", d.UpdatedInput, asked)
	}
}

func TestSendMessagePasses(t *testing.T) {
	called := false
	h := Hook{Peer: func(string) (string, error) { called = true; return "", nil }}
	if d := decideSend(t, h, "local_stopped"); d != nil {
		t.Fatalf("a stopped desktop session is the desktop's to start: %+v", d)
	}
	called = false
	if d := decideSend(t, h, "gpm worker"); d != nil || called {
		t.Fatalf("a send by name passes without a lookup: %+v, looked up %v", d, called)
	}
	if d := decideSend(t, Hook{}, "local_x"); d != nil {
		t.Fatalf("no peer lookup: pass, got %+v", d)
	}
}

func TestSendMessageRefusedWhenTheNameIsAmbiguous(t *testing.T) {
	h := Hook{Peer: func(string) (string, error) { return "", errors.New("2 running CLIs are named \"w\"") }}
	d := decideSend(t, h, "local_x")
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "2 running CLIs") {
		t.Fatalf("want a refusal, got %+v", d)
	}
}

const guideHost = "local_g"

func TestSendMessageToARoleGoesToItsHolder(t *testing.T) {
	holders := map[string]string{RoleSupervisor: "Supervisor run 60", RoleGuide: guideHost}
	var peered string
	h := Hook{
		Role: func(r string) (string, error) { return holders[r], nil },
		Peer: func(host string) (string, error) { peered = host; return "", nil },
	}
	for _, to := range []string{"the supervisor", "Supervisor", "  The  Supervisor "} {
		d := decideSend(t, h, to)
		if d == nil || d.PermissionDecision != decisionAllow || d.UpdatedInput["to"] != "Supervisor run 60" || d.UpdatedInput["message"] != "hi" {
			t.Fatalf("%q: want the holder, got %+v", to, d)
		}
		if !strings.Contains(d.Reason, "the supervisor is Supervisor run 60 now") {
			t.Errorf("%q: reason %q", to, d.Reason)
		}
	}
	// A holder whose CLI is not running is its desktop session, which the
	// desktop starts.
	d := decideSend(t, h, "the guide")
	if d == nil || d.UpdatedInput["to"] != guideHost || peered != guideHost {
		t.Fatalf("guide: %+v (peered %q)", d, peered)
	}
	// A session's own name is no role.
	if d := decideSend(t, h, "supervisor run 59"); d != nil {
		t.Fatalf("a name passes: %+v", d)
	}
}

func TestSendMessageToARoleWithoutAHolderIsRefused(t *testing.T) {
	h := Hook{Role: func(string) (string, error) { return "", errors.New("no session holds the supervisor's role") }}
	d := decideSend(t, h, "the supervisor")
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "no session holds") {
		t.Fatalf("want a refusal, got %+v", d)
	}
	if d := decideSend(t, Hook{}, "the supervisor"); d != nil {
		t.Fatalf("no role lookup: pass, got %+v", d)
	}
}

func TestSendMessageByNameToAnAgentWithoutACLIIsRefused(t *testing.T) {
	h := Hook{Absent: func(name string) string {
		if name == "Rotation login" {
			return `no CLI of "Rotation login" runs; its import is pending`
		}
		return ""
	}}
	d := decideSend(t, h, "Rotation login")
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "its import is pending") {
		t.Fatalf("want a refusal saying the import is pending, got %+v", d)
	}
	if d := decideSend(t, h, "gpm worker"); d != nil {
		t.Fatalf("a running name passes: %+v", d)
	}
}

// A name the role's holder carries on the roster goes where a message to the
// role goes, even when its desktop titles the session differently, and is
// never refused as an absent agent.
func TestSendMessageByAHoldersRunNameGoesToTheHolder(t *testing.T) {
	absent := false
	h := Hook{
		Role:   func(r string) (string, error) { return "fixture-lab-14", nil },
		Holds:  func(name string) string { return map[string]string{"Supervisor run 82": RoleSupervisor}[name] },
		Absent: func(string) string { absent = true; return "no CLI of it runs" },
	}
	d := decideSend(t, h, "Supervisor run 82")
	if d == nil || d.PermissionDecision != decisionAllow || d.UpdatedInput["to"] != "fixture-lab-14" || absent {
		t.Fatalf("want the holder's CLI, got %+v (absent asked %v)", d, absent)
	}
	if !strings.Contains(d.Reason, "the supervisor is fixture-lab-14 now") {
		t.Errorf("reason %q", d.Reason)
	}
	if d := decideSend(t, h, "worker one"); d == nil || d.PermissionDecision != decisionDeny || !absent {
		t.Fatalf("a name no holder carries still meets Absent: %+v", d)
	}
}

// A message by a name several running CLIs carry goes where Agent sends it
// (the roster agent's own socket), and Agent's refusal refuses the send.
func TestSendMessageByASharedNameGoesToTheAgent(t *testing.T) {
	const worker, sock = "KG worker", "uds:/run/user/1000/cc-socks/8.sock"
	h := Hook{
		Agent: func(name string) (string, error) {
			if name == worker {
				return sock, nil
			}
			return "", nil
		},
	}
	d := decideSend(t, h, worker)
	if d == nil || d.PermissionDecision != decisionAllow || d.UpdatedInput["to"] != sock {
		t.Fatalf("want a redirect to the agent's socket, got %+v", d)
	}
	if d := decideSend(t, h, "another"); d != nil {
		t.Fatalf("a name Agent leaves passes: %+v", d)
	}
	h.Agent = func(string) (string, error) { return "", errors.New("no CLI of \"KG worker\" runs") }
	if d := decideSend(t, h, worker); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "no CLI of") {
		t.Fatalf("want a refusal, got %+v", d)
	}
}

func TestSendMessageToAnInboxAgentIsDeliveredThere(t *testing.T) {
	const codexWorker = "codex worker"
	var gotSession, gotName, gotMsg string
	h := Hook{
		Inbox: func(session, name, msg string) (string, error) {
			gotSession, gotName, gotMsg = session, name, msg
			if name != codexWorker {
				return "", nil
			}
			return "written to the codex inbox of codex worker", nil
		},
		Absent: func(string) string { t.Error("a delivered send is not looked up as absent"); return "" },
	}
	raw, _ := json.Marshal(map[string]any{sessionKey: "guide-session", executionToolName: SendMessageTool,
		executionToolInput: map[string]any{"to": codexWorker, messageKey: "go ahead", "summary": "s"}})
	var ev map[string]any
	_ = json.Unmarshal(raw, &ev)
	d := decideEvent(t, h, ev)
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "Delivered by beekeeper") || !strings.Contains(d.Reason, "codex inbox of codex worker") {
		t.Fatalf("want the delivery said, got %+v", d)
	}
	if gotSession != "guide-session" || gotName != codexWorker || gotMsg != "go ahead" {
		t.Fatalf("Inbox(%q, %q, %q)", gotSession, gotName, gotMsg)
	}
	if d := decideSend(t, Hook{Inbox: h.Inbox}, "claude worker"); d != nil {
		t.Fatalf("a name no inbox agent carries passes as written: %+v", d)
	}
	gone := Hook{Inbox: func(string, string, string) (string, error) {
		return "", errors.New("codex worker: its codex agent no longer runs")
	}}
	if d := decideSend(t, gone, codexWorker); d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "no longer runs") {
		t.Fatalf("want the inbox's refusal, got %+v", d)
	}
	if d := decideSend(t, Hook{Inbox: func(_, name, _ string) (string, error) {
		if name != "guide name" {
			t.Errorf("role inbox target %q", name)
		}
		return "", nil
	}, Role: func(string) (string, error) { return "guide name", nil }}, "the guide"); d == nil || d.UpdatedInput["to"] != "guide name" {
		t.Fatalf("a role still goes to its holder: %+v", d)
	}
}

func TestSendMessageToAGoneSocketNamesTheRosterName(t *testing.T) {
	gone := SocketPrefix + filepath.Join(t.TempDir(), "relay-turn.sock")
	d := decideSend(t, Hook{}, gone)
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "is gone") || !strings.Contains(d.Reason, "roster name") {
		t.Fatalf("want a refusal naming the roster name, got %+v", d)
	}
	live := filepath.Join(t.TempDir(), "cli.sock")
	if err := os.WriteFile(live, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if d := decideSend(t, Hook{}, SocketPrefix+live); d != nil {
		t.Fatalf("a socket that exists passes: %+v", d)
	}
}

func TestSendMessageReportsQueuedInstruction(t *testing.T) {
	h := Hook{Inbox: func(_, _, _ string) (string, error) {
		return "queued; instruction reaches the session chat at its next tool or turn boundary", nil
	}}
	d := decideSend(t, h, "waiting worker")
	if d == nil || d.PermissionDecision != decisionDeny || !strings.Contains(d.Reason, "Queued by beekeeper") || strings.Contains(d.Reason, "Delivered") || !strings.Contains(d.Reason, "do not repeat") {
		t.Fatalf("queued relay result: %+v", d)
	}
}
