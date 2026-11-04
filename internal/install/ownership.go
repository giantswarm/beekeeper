package install

import (
	"encoding/json"
	"slices"
)

// rememberPrevious keeps both possible on-disk versions while the plan runs.
// The proposed manifest is written first, so a replacement failure must not
// make the earlier version appear to be somebody else's file or hook.
func (m *Manifest) rememberPrevious(previous Manifest) {
	for path, hash := range previous.Files {
		if next, ok := m.Files[path]; !ok || next == hash || slices.Contains(m.PreviousFiles[path], hash) {
			continue
		}
		if m.PreviousFiles == nil {
			m.PreviousFiles = map[string][]string{}
		}
		m.PreviousFiles[path] = append(m.PreviousFiles[path], hash)
	}
	for _, hook := range previous.Hooks {
		matches := func(h Hook) bool { return h.Event == hook.Event && sameJSON(h.Entry, hook.Entry) }
		if !slices.ContainsFunc(m.Hooks, matches) && !slices.ContainsFunc(m.PreviousHooks, matches) {
			m.PreviousHooks = append(m.PreviousHooks, hook)
		}
	}
	if len(previous.AgyHook) > 0 && !m.ownsAgyHook(previous.AgyHook) {
		m.PreviousAgyHooks = append(m.PreviousAgyHooks, previous.AgyHook)
	}
	if len(previous.CodexHook) > 0 && !m.ownsCodexHook(previous.CodexHook) {
		m.PreviousCodexHooks = append(m.PreviousCodexHooks, previous.CodexHook)
	}
}

func (m Manifest) ownsAgyHook(raw json.RawMessage) bool {
	return sameJSON(raw, m.AgyHook) || slices.ContainsFunc(m.PreviousAgyHooks, func(h json.RawMessage) bool { return sameJSON(raw, h) })
}

func (m Manifest) ownsCodexHook(raw json.RawMessage) bool {
	return sameJSON(raw, m.CodexHook) || slices.ContainsFunc(m.PreviousCodexHooks, func(h json.RawMessage) bool { return sameJSON(raw, h) })
}

func (m Manifest) hasPrevious() bool {
	return len(m.PreviousFiles) > 0 || len(m.PreviousHooks) > 0 || len(m.PreviousAgyHooks) > 0 || len(m.PreviousCodexHooks) > 0
}
