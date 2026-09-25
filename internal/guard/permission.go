package guard

import (
	"encoding/json"
)

// ModeAcceptEdits is the mode Claude Desktop turns a bypassPermissions
// session into when it imports it (claude://resume).
const ModeAcceptEdits = "acceptEdits"

// permissionAllow is the PermissionRequest hook's answer that lets the
// request run without the person's card.
var permissionAllow = []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n")

// Request is the part of a PermissionRequest event the hook reads.
type Request struct {
	Event   string `json:"hook_event_name"`
	Session string `json:"session_id"`
	Mode    string `json:"permission_mode"`
	Tool    string `json:"tool_name"`
}

// Permission decides one PermissionRequest event. It allows the request
// only when the session runs in acceptEdits and started reports it as a
// session beekeeper started in bypassPermissions; any other event, malformed
// input included, gets no decision (nil) and the person gets the normal
// card. started is asked only for an acceptEdits request, so every other
// request is decided without reading beekeeper's state.
func Permission(input []byte, started func(session string) bool) ([]byte, Request) {
	var r Request
	if json.Unmarshal(input, &r) != nil || r.Event != "PermissionRequest" || r.Mode != ModeAcceptEdits || r.Session == "" {
		return nil, r
	}
	if !started(r.Session) {
		return nil, r
	}
	return permissionAllow, r
}
