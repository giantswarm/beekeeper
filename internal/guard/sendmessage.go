package guard

import (
	"strings"
)

// SendMessageTool is the tool one Claude session messages another with.
const SendMessageTool = "SendMessage"

// desktopPrefix starts a Claude Desktop session id, the target shape the
// desktop routes (and caps) itself.
const desktopPrefix = "local_"

// sendMessage decides a SendMessage call. A message to a desktop session id
// (local_…) goes through Claude Desktop, which starts a CLI of its own for
// the session when it has none: while a headless turn of the session runs
// (a first turn of agents start, a wake), that is a second copy of the
// session beside it, and every such send counts against the desktop's cap
// on messages between sessions. peer returns the name the session's running
// CLI takes messages under, "" when none runs; an error is a send the hook
// refuses. A running session's message is redirected to its name, which
// queues it in that CLI; every other call passes unchanged.
func (h Hook) sendMessage(input map[string]any) []byte {
	to, _ := input["to"].(string)
	to = strings.TrimSpace(to)
	if h.Peer == nil || !strings.HasPrefix(to, desktopPrefix) {
		return nil
	}
	name, err := h.Peer(to)
	switch {
	case err != nil:
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused by beekeeper: " + err.Error()})
	case name == "":
		return nil
	}
	updated := make(map[string]any, len(input))
	for k, v := range input {
		updated[k] = v
	}
	updated["to"] = name
	return answer(hookOutput{
		PermissionDecision: decisionAllow,
		Reason:             "beekeeper: " + to + " runs a CLI; sent by its name " + name + " so no second copy starts and the desktop's cap is not spent",
		UpdatedInput:       updated,
	})
}
