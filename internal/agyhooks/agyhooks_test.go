package agyhooks

import (
	"strings"
	"testing"
)

const guard = `{
  "PreInvocation": [{"type": "command", "command": "/opt/bin/beekeeper hook preinvocation", "timeout": 30}],
  "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "/opt/bin/beekeeper hook pretooluse --format agy", "timeout": 30}]}]
}`

// agy refuses an entry whose lifecycle event lists a matcher wrapper, whose
// tool event lists a command without a matcher's hooks, or whose command hook
// names no command; it accepts flat lifecycle commands and matchers with
// commands under the tool events.
func TestEntryRejects(t *testing.T) {
	const invalidHookLists = "not an object of hook lists"
	for _, c := range []struct {
		name, raw, want string
	}{
		{"beekeeper's entry", guard, ""},
		{"a lifecycle event of flat commands", `{"PostInvocation": [{"command": "/usr/bin/true"}]}`, ""},
		{"a hook of another type", `{"PreInvocation": [{"type": "http", "url": "https://example.invalid/hook"}]}`, `invalid hook fields: unsupported hook type: "http"`},
		{"a PostToolUse matcher", `{"PostToolUse": [{"matcher": "*", "hooks": [{"command": "/usr/bin/true"}]}]}`, ""},
		{"a matcher under PreInvocation", `{"PreInvocation": [{"matcher": "*", "hooks": [{"type": "command", "command": "/usr/bin/true"}]}]}`,
			"PreInvocation: command hook must specify 'command'"},
		{"a matcher under another lifecycle event", `{"PostInvocation": [{"hooks": [{"type": "command", "command": "/usr/bin/true"}]}]}`,
			"PostInvocation: command hook must specify 'command'"},
		{"a blank command", `{"PreInvocation": [{"type": "command", "command": "  "}]}`, "PreInvocation: command hook must specify 'command'"},
		{"a matcher without hooks", `{"PreToolUse": [{"matcher": "*"}]}`, "PreToolUse: a matcher without hooks"},
		{"a flat command under PreToolUse", `{"PreToolUse": [{"type": "command", "command": "/usr/bin/true"}]}`, "PreToolUse: a matcher without hooks"},
		{"a matcher hook without its command", `{"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command"}]}]}`, "PreToolUse: command hook must specify 'command'"},
		{"an event that is no list", `{"PreToolUse": {"matcher": "*"}}`, invalidHookLists},
		{"an entry that is no object", `"/usr/bin/true"`, invalidHookLists},
		{"null", `null`, invalidHookLists},
	} {
		got := EntryRejects([]byte(c.raw))
		if c.want == "" && got != "" || c.want != "" && !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: EntryRejects = %q, want %q", c.name, got, c.want)
		}
	}
}

// One entry agy rejects rejects the whole file, beekeeper's valid entry with
// it: Rejects names the first such entry in file order, whoever owns it.
func TestRejectsTheWholeFileForAnyEntry(t *testing.T) {
	stale := `{"PreInvocation": [{"matcher": "*", "hooks": [{"type": "command", "command": "/usr/local/bin/stale"}]}]}`
	broken := `{"PreToolUse": [{"matcher": "*"}]}`
	for _, c := range []struct {
		name, raw, want string
	}{
		{"empty", "", ""},
		{"no entries", "{}", ""},
		{"beekeeper's entry alone", `{"beekeeper": ` + guard + `}`, ""},
		{"a stale entry after beekeeper's", `{"beekeeper": ` + guard + `, "stale": ` + stale + `}`,
			`the "stale" entry: PreInvocation: command hook must specify 'command'`},
		{"the first rejected entry in file order", `{"zz": ` + stale + `, "beekeeper": ` + guard + `, "aa": ` + broken + `}`,
			`the "zz" entry: PreInvocation`},
		{"no object", `[]`, "not a JSON object of named hook entries"},
		{"no JSON", `{"beekeeper": `, "not a JSON object of named hook entries"},
	} {
		got := Rejects([]byte(c.raw))
		if c.want == "" && got != "" || c.want != "" && !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: Rejects = %q, want %q", c.name, got, c.want)
		}
	}
}
