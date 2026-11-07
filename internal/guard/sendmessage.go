package guard

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// SendMessageTool is the tool one Claude session messages another with.
const SendMessageTool = "SendMessage"

// desktopPrefix starts a Claude Desktop session id, the target shape the
// desktop routes (and caps) itself.
const desktopPrefix = "local_"

// SocketPrefix starts a SendMessage target that is a CLI's peer socket.
const SocketPrefix = "uds:"

// sendMessage decides a SendMessage call. A message to "the guide" goes to
// the session holding the role now: Role names it, so no brief has to name
// a run that a relay makes stale. One to "the supervisor" is refused by
// Role: the supervisor is the supervisor loop, which no session holds. A
// message by a name the holder carries or carried on the roster (Holds) goes
// the same way, whatever its desktop titles the session now. A
// message to a desktop session id (local_…) goes through Claude Desktop,
// which starts a CLI of its own for the session when it has none: while a
// headless turn of the session runs (a first turn of agents start, a wake),
// that is a second copy of the session beside it, and every such send
// counts against the desktop's cap on messages between sessions. peer
// returns the name the session's running CLI takes messages under, "" when
// none runs; an error is a send the hook refuses. A running session's
// message is redirected to its name, which queues it in that CLI. A message
// by name to a roster agent whose CLI does not run is refused with what
// Absent says: no CLI runs, and whether an import is pending. A message by
// a name several running CLIs carry goes to the roster agent's of that name
// through its own socket (Agent), so a hand-over's follow-up gets it while
// the session it replaces still runs. A message by name to a roster agent
// whose harness reads an inbox is delivered there by Inbox. A running Claude
// session can likewise receive a queued instruction through its boundary hooks.
// The native call is then refused with the delivery status to avoid a second
// send. A peer socket (uds:…) that no longer exists is
// refused with the sender's roster name to reply by: a relay turn's socket
// ends with the turn. Every other call passes unchanged.
func (h Hook) sendMessage(session string, input map[string]any) []byte {
	to, _ := input["to"].(string)
	to = strings.TrimSpace(to)
	var why []string
	role, ok := RoleOf(to)
	if !ok && h.Holds != nil {
		role = h.Holds(to)
		ok = role != ""
	}
	if ok && h.Role != nil {
		holder, err := h.Role(role)
		if err != nil {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + err.Error()})
		}
		why = append(why, "the "+role+" is "+holder+" now")
		to = holder
	}
	if path, sock := strings.CutPrefix(to, SocketPrefix); sock && path != "" {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: the peer socket " + path +
				" is gone: it was a turn's own, which ended (a message sent by beekeeper agents wake comes from a relay turn). Reply by the sender's roster name, which its message names"})
		}
	}
	if h.Inbox != nil {
		msg, _ := input["message"].(string)
		where, err := h.Inbox(session, to, msg)
		if err != nil {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + err.Error()})
		}
		if where != "" {
			prefix := "Handled by beekeeper: "
			if strings.HasPrefix(where, "queued") {
				prefix = "Queued by beekeeper: "
			}
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: prefix + where +
				". Nothing more to send; do not repeat the message"})
		}
	}
	if !ok && h.Absent != nil && !strings.HasPrefix(to, desktopPrefix) {
		if r := h.Absent(to); r != "" {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + r})
		}
	}
	if !ok && h.Agent != nil && !strings.HasPrefix(to, desktopPrefix) {
		addr, err := h.Agent(to)
		if err != nil {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + err.Error()})
		}
		if addr != "" {
			why = append(why, "several running CLIs are named "+to+"; sent to the roster agent's own CLI at "+addr)
			to = addr
		}
	}
	if h.Peer != nil && strings.HasPrefix(to, desktopPrefix) {
		name, err := h.Peer(to)
		if err != nil {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + err.Error()})
		}
		if name != "" {
			why = append(why, to+" runs a CLI; sent by its name "+name+" so no second copy starts and the desktop's cap is not spent")
			to = name
		}
	}
	if len(why) == 0 {
		return nil
	}
	updated := make(map[string]any, len(input))
	for k, v := range input {
		updated[k] = v
	}
	updated["to"] = to
	return answer(hookOutput{
		PermissionDecision: decisionAllow,
		Reason:             "beekeeper: " + strings.Join(why, "; "),
		UpdatedInput:       updated,
	})
}

// Roles a message can address by name instead of by session.
const (
	RoleSupervisor = "supervisor"
	RoleGuide      = "guide"
)

// RoleOf reads a SendMessage target that names a role: "the supervisor",
// "supervisor", "The Guide".
func RoleOf(to string) (string, bool) {
	r := strings.ToLower(strings.Join(strings.Fields(to), " "))
	r = strings.TrimPrefix(r, "the ")
	switch r {
	case RoleSupervisor, RoleGuide:
		return r, true
	}
	return "", false
}
