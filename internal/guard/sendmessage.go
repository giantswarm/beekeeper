package guard

import (
	"strings"
)

// SendMessageTool is the tool one Claude session messages another with.
const SendMessageTool = "SendMessage"

// desktopPrefix starts a Claude Desktop session id, the target shape the
// desktop routes (and caps) itself.
const desktopPrefix = "local_"

// sendMessage decides a SendMessage call. A message to a role ("the
// supervisor", "the guide") goes to the session holding the role now: Role
// names it, so no brief has to name a run that a relay makes stale. A
// message to a desktop session id (local_…) goes through Claude Desktop,
// which starts a CLI of its own for the session when it has none: while a
// headless turn of the session runs (a first turn of agents start, a wake),
// that is a second copy of the session beside it, and every such send
// counts against the desktop's cap on messages between sessions. peer
// returns the name the session's running CLI takes messages under, "" when
// none runs; an error is a send the hook refuses. A running session's
// message is redirected to its name, which queues it in that CLI. A message
// by name to a roster agent whose CLI does not run is refused with what
// Absent says: no CLI runs, and whether an import is pending. Every other
// call passes unchanged.
func (h Hook) sendMessage(input map[string]any) []byte {
	to, _ := input["to"].(string)
	to = strings.TrimSpace(to)
	var why []string
	if role, ok := RoleOf(to); ok && h.Role != nil {
		holder, err := h.Role(role)
		if err != nil {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + err.Error()})
		}
		why = append(why, "the "+role+" is "+holder+" now")
		to = holder
	}
	if len(why) == 0 && h.Absent != nil && !strings.HasPrefix(to, desktopPrefix) {
		if r := h.Absent(to); r != "" {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + r})
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
