// Package agyhooks checks agy's hooks.json against the shape agy accepts.
// agy (1.4.x) parses the whole file at once and drops every hook in it when
// any one entry is malformed, so one stale entry of someone else's leaves
// beekeeper's own PreInvocation and PreToolUse hooks unloaded: an agy worker
// then runs with no guard at all.
package agyhooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// toolEvents are the events agy fires for a tool call: their items are
// matchers, each with its command hooks. Every other event (PreInvocation and
// the other lifecycle events) lists the command hooks themselves.
var toolEvents = map[string]bool{"PreToolUse": true, "PostToolUse": true}

// EntryRejects is why agy refuses the named hook entry raw, an object of
// event lists and optional enabled metadata, or "" when agy accepts it.
// Unknown fields are ignored. A tool event's items are matchers
// with at least one hook, a lifecycle event's items are command hooks, and
// every hook of type "command" (or of no type) names its command.
func EntryRejects(raw json.RawMessage) string {
	var events *struct {
		Enabled        bool              `json:"enabled"`
		SessionStart   []json.RawMessage `json:"SessionStart"`
		PreInvocation  []json.RawMessage `json:"PreInvocation"`
		PostInvocation []json.RawMessage `json:"PostInvocation"`
		Stop           []json.RawMessage `json:"Stop"`
		PreToolUse     []json.RawMessage `json:"PreToolUse"`
		PostToolUse    []json.RawMessage `json:"PostToolUse"`
	}
	// Decode typed fields before inspecting the final event values. agy
	// rejects a type error even if a later duplicate field replaces it.
	if err := json.Unmarshal(raw, &events); err != nil {
		return "not an object of hook lists: " + err.Error()
	}
	if events == nil {
		return "not an object of hook lists"
	}
	if why := fieldTypesRejects(raw); why != "" {
		return why
	}
	for _, event := range ordered(raw) {
		var items []json.RawMessage
		switch strings.ToLower(event) {
		case "sessionstart":
			items = events.SessionStart
		case "preinvocation":
			items = events.PreInvocation
		case "postinvocation":
			items = events.PostInvocation
		case "stop":
			items = events.Stop
		case "pretooluse":
			event, items = "PreToolUse", events.PreToolUse
		case "posttooluse":
			event, items = "PostToolUse", events.PostToolUse
		default:
			// enabled is metadata; all other fields are ignored by agy.
			continue
		}
		for _, item := range items {
			if why := itemRejects(event, item); why != "" {
				return why
			}
		}
	}
	return ""
}

func itemRejects(event string, item json.RawMessage) string {
	if !toolEvents[event] {
		return commandRejects(event, item)
	}
	var e struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(item, &e); err != nil {
		return event + ": " + err.Error()
	}
	if len(e.Hooks) == 0 {
		return event + ": a matcher without hooks"
	}
	for _, h := range e.Hooks {
		if why := commandRejects(event, h); why != "" {
			return why
		}
	}
	return ""
}

func commandRejects(event string, item json.RawMessage) string {
	var h handlerFields
	if err := json.Unmarshal(item, &h); err != nil {
		return event + ": " + err.Error()
	}
	if strings.TrimSpace(h.Command) == "" {
		return event + ": command hook must specify 'command'"
	}
	return ""
}

// Rejects is why agy refuses the hooks.json raw as a whole, naming the first
// entry it rejects in file order, or "" when agy accepts every entry. An
// empty file holds no hooks and is accepted.
func Rejects(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "not a JSON object of named hook entries: " + err.Error()
	}
	if top == nil {
		return "not a JSON object of named hook entries"
	}
	for _, field := range orderedMembers(raw) {
		// agy decodes every occurrence of a named entry, even if a later
		// duplicate replaces it. Only command presence uses the final value.
		if why := fieldTypesRejects(field.value); why != "" {
			return fmt.Sprintf("the %q entry: %s", field.name, why)
		}
		if why := EntryRejects(top[field.name]); why != "" {
			return fmt.Sprintf("the %q entry: %s", field.name, why)
		}
	}
	return ""
}

type member struct {
	name  string
	value json.RawMessage
}

// ordered is the keys of the JSON object raw in their order in the text, or
// nil when raw is no object.
func ordered(raw []byte) []string {
	var keys []string
	for _, field := range orderedMembers(raw) {
		keys = append(keys, field.name)
	}
	return keys
}

// orderedMembers retains values too, so decoding checks can inspect earlier
// occurrences of a duplicate name rather than only its final value.
func orderedMembers(raw []byte) []member {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var fields []member
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return fields
		}
		k, ok := t.(string)
		if !ok {
			return fields
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return fields
		}
		fields = append(fields, member{name: k, value: value})
	}
	return fields
}
