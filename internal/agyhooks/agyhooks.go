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
// event lists, or "" when agy accepts it: a tool event's items are matchers
// with at least one hook, a lifecycle event's items are command hooks, and
// every hook uses the supported command type and names its command.
func EntryRejects(raw json.RawMessage) string {
	var events map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return "not an object of hook lists: " + err.Error()
	}
	if events == nil {
		return "not an object of hook lists"
	}
	for _, event := range ordered(raw) {
		for _, item := range events[event] {
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
		Matcher string            `json:"matcher"`
		Hooks   []json.RawMessage `json:"hooks"`
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
	var h struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		// Decode even unused fields: agy discards the whole file on a type
		// mismatch. Its timeout accepts null, zero and signed int32 values.
		Timeout int32 `json:"timeout"`
	}
	if err := json.Unmarshal(item, &h); err != nil {
		return event + ": " + err.Error()
	}
	if h.Type != "" && h.Type != "command" {
		return fmt.Sprintf("%s: unsupported hook type %q", event, h.Type)
	}
	// agy defaults a missing, null or empty type to command.
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
	for _, name := range ordered(raw) {
		if why := EntryRejects(top[name]); why != "" {
			return fmt.Sprintf("the %q entry: %s", name, why)
		}
	}
	return ""
}

// ordered is the keys of the JSON object raw in their order in the text, or
// nil when raw is no object.
func ordered(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return keys
		}
		k, ok := t.(string)
		if !ok {
			return keys
		}
		keys = append(keys, k)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}
