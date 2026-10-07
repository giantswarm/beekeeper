package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/pkg/project"
)

// Hook is one entry of a hook event's list in Claude Code's settings.
type Hook struct {
	Event string          `json:"event"`
	Entry json.RawMessage `json:"entry"`
}

// hookEntry and hookCommand are an entry as Claude Code reads it.
type hookEntry struct {
	Matcher string        `json:"matcher"`
	Hooks   []hookCommand `json:"hooks"`
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Hooks are the entries install adds for the binary exe: the guard before
// every tool call it rewrites or refuses, the permission requests of the
// agents beekeeper starts, the value scan of every tool result, and the
// agent shell's prelude at every session's start.
func Hooks(exe string) []Hook {
	hook := func(event, matcher, sub string, timeout int) Hook {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(hookEntry{Matcher: matcher, Hooks: []hookCommand{{Type: "command", Command: shellQuote(exe) + " hook " + sub, Timeout: timeout}}})
		return Hook{Event: event, Entry: bytes.TrimSpace(b.Bytes())}
	}
	return []Hook{
		hook("PreToolUse", "Bash|Read|Grep|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|mcp__.*", "pretooluse", 30),
		hook("PermissionRequest", "*", "permissionrequest", 300),
		hook("PostToolUse", "*", "posttooluse", 10),
		hook("SessionStart", "", "sessionstart", 10),
	}
}

// shellQuote quotes a path a shell would split.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// command is the hook's beekeeper subcommand ("hook pretooluse") when an
// entry runs one, whatever the binary's path.
func command(entry json.RawMessage) []string {
	var e hookEntry
	if json.Unmarshal(entry, &e) != nil {
		return nil
	}
	var subs []string
	for _, h := range e.Hooks {
		f := strings.Fields(h.Command)
		if len(f) == 3 && filepath.Base(strings.Trim(f[0], `'"`)) == project.Name && f[1] == "hook" {
			subs = append(subs, f[2])
		}
	}
	return subs
}

// planHooks adds the hooks a settings file lacks.
func (e Env) planHooks(p *plan, m *Manifest) error {
	s, err := readSettings(e.Settings)
	if err != nil {
		return err
	}
	changed := false
	for _, want := range Hooks(e.Exe) {
		list, err := s.hooks(want.Event)
		if err != nil {
			return err
		}
		what := e.Settings + ": " + want.Event + " hook"
		mine := slices.IndexFunc(m.Hooks, func(h Hook) bool { return h.Event == want.Event })
		switch i, state := find(list, want, m.Hooks, mine); state {
		case fileSame:
			p.add("ok", what, "", nil)
		case fileUpdated:
			list[i] = want.Entry
			m.Hooks[mine] = want
			changed = true
			p.add("update", what, "", nil)
		case fileKept:
			p.add("keep", what, "a beekeeper hook with another command", nil)
		default:
			if !s.has("hooks") {
				m.SettingsKeys = append(m.SettingsKeys, "hooks")
			}
			if _, ok := s.hooksObj()[want.Event]; !ok {
				m.SettingsKeys = append(m.SettingsKeys, "hooks."+want.Event)
			}
			list = append(list, want.Entry)
			m.Hooks = append(m.Hooks, want)
			changed = true
			p.add("add", what, "", nil)
		}
		s.setHooks(want.Event, list)
	}
	if !changed {
		return nil
	}
	if !s.existed {
		m.SettingsCreated = true
		m.addDirs(filepath.Dir(e.Settings))
	}
	raw := s.encode()
	p.add(map[bool]string{true: "update", false: "write"}[s.existed], e.Settings, "", func() error {
		return writeFile(e.Settings, raw)
	})
	return nil
}

// find is where want stands in list: as it is, as the entry install added
// earlier (mine, an index into added, -1 for none), as another beekeeper
// hook of the same subcommand, or absent (fileNew).
func find(list []json.RawMessage, want Hook, added []Hook, mine int) (int, fileState) {
	if slices.ContainsFunc(list, func(r json.RawMessage) bool { return sameJSON(r, want.Entry) }) {
		return 0, fileSame
	}
	if mine >= 0 {
		if i := slices.IndexFunc(list, func(r json.RawMessage) bool { return sameJSON(r, added[mine].Entry) }); i >= 0 {
			return i, fileUpdated
		}
	}
	sub := command(want.Entry)
	if slices.ContainsFunc(list, func(r json.RawMessage) bool { return slices.Equal(command(r), sub) }) {
		return 0, fileKept
	}
	return 0, fileNew
}

// planUnhooks removes the hooks install added and the keys it created.
func (e Env) planUnhooks(p *plan, m Manifest, removed map[string]bool) error {
	if len(m.Hooks) == 0 {
		return nil
	}
	s, err := readSettings(e.Settings)
	if err != nil || !s.existed {
		return err
	}
	changed := false
	for _, h := range m.Hooks {
		list, err := s.hooks(h.Event)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(slices.Clone(list), func(r json.RawMessage) bool { return sameJSON(r, h.Entry) })
		if len(kept) == len(list) {
			continue
		}
		changed = true
		p.add("remove", e.Settings+": "+h.Event+" hook", "", nil)
		if len(kept) == 0 && slices.Contains(m.SettingsKeys, "hooks."+h.Event) {
			s.delHooks(h.Event)
		} else {
			s.setHooks(h.Event, kept)
		}
	}
	if slices.Contains(m.SettingsKeys, "hooks") && len(s.hooksObj()) == 0 {
		s.top.del("hooks")
	}
	switch {
	case !changed:
	case m.SettingsCreated && len(s.top) == 0:
		removed[e.Settings] = true
		p.add("remove", e.Settings, "", func() error { return os.Remove(e.Settings) })
	default:
		raw := s.encode()
		p.add("update", e.Settings, "", func() error { return writeFile(e.Settings, raw) })
	}
	return nil
}

// settings is a Claude Code settings file, its keys in their order, written
// back in its own layout.
type settings struct {
	existed bool
	newline bool
	// compact is a file on one line; indent is the others' indentation.
	compact bool
	indent  string
	top     object
}

func readSettings(path string) (*settings, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return &settings{newline: true, indent: "  "}, nil
	}
	if err != nil {
		return nil, err
	}
	s := &settings{existed: true, newline: bytes.HasSuffix(raw, []byte("\n")), indent: "  "}
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return s, nil
	}
	if _, rest, ok := bytes.Cut(body, []byte("\n")); ok {
		s.indent = string(rest[:len(rest)-len(bytes.TrimLeft(rest, " \t"))])
	} else {
		s.compact = true
	}
	if s.top, err = decodeObject(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *settings) has(key string) bool { _, ok := s.top.get(key); return ok }

// hooksObj is the hooks object by event, nil when absent or not an object.
func (s *settings) hooksObj() map[string]json.RawMessage {
	raw, ok := s.top.get("hooks")
	if !ok {
		return nil
	}
	o, err := decodeObject(raw)
	if err != nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, mb := range o {
		out[mb.key] = mb.value
	}
	return out
}

// hooks is the event's list.
func (s *settings) hooks(event string) ([]json.RawMessage, error) {
	raw, ok := s.top.get("hooks")
	if !ok {
		return nil, nil
	}
	o, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("settings hooks: %w", err)
	}
	raw, ok = o.get(event)
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("settings hooks.%s: %w", event, err)
	}
	return list, nil
}

func (s *settings) setHooks(event string, list []json.RawMessage) {
	s.editHooks(func(o *object) { o.set(event, encodeArray(list)) })
}

func (s *settings) delHooks(event string) { s.editHooks(func(o *object) { o.del(event) }) }

func (s *settings) editHooks(edit func(*object)) {
	var o object
	if raw, ok := s.top.get("hooks"); ok {
		o, _ = decodeObject(raw)
	}
	edit(&o)
	s.top.set("hooks", o.encode())
}

// encode is the file's content in its layout; a new file is indented by
// two spaces as Claude Code writes it.
func (s *settings) encode() []byte {
	var b bytes.Buffer
	if s.compact {
		b.Write(s.top.encode())
	} else {
		_ = json.Indent(&b, s.top.encode(), "", s.indent)
	}
	if s.newline {
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// object is a JSON object that keeps its keys' order and its values' text.
type object []member

type member struct {
	key   string
	value json.RawMessage
}

func decodeObject(raw []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, member{t.(string), v})
	}
	return o, nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	i := slices.IndexFunc(o, func(m member) bool { return m.key == key })
	if i < 0 {
		return nil, false
	}
	return o[i].value, true
}

func (o *object) set(key string, v json.RawMessage) {
	if i := slices.IndexFunc(*o, func(m member) bool { return m.key == key }); i >= 0 {
		(*o)[i].value = v
		return
	}
	*o = append(*o, member{key, v})
}

func (o *object) del(key string) {
	*o = slices.DeleteFunc(*o, func(m member) bool { return m.key == key })
}

// encode is o compact, its values' text unescaped.
func (o object) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		_ = json.Compact(&b, m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func encodeArray(list []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, v := range list {
		if i > 0 {
			b.WriteByte(',')
		}
		_ = json.Compact(&b, v)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// sameJSON reports whether a and b are the same JSON text but for spacing.
func sameJSON(a, b json.RawMessage) bool {
	var x, y bytes.Buffer
	return json.Compact(&x, a) == nil && json.Compact(&y, b) == nil && bytes.Equal(x.Bytes(), y.Bytes())
}
