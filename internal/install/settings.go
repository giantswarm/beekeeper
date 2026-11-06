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

	"github.com/giantswarm/beekeeper/internal/agyhooks"
	"github.com/giantswarm/beekeeper/pkg/project"
	"github.com/giantswarm/beekeeper/plugin"
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

const (
	hookTypeCommand = "command"
	hookWord        = "hook"
	// autoMemoryKey switches Claude Code's own memory: beekeeper's hook
	// refuses a session's writes to it, and this keeps Claude Code from
	// writing there itself.
	autoMemoryKey = "autoMemoryEnabled"
)

// Hooks are the entries install adds for the binary exe: the guard before
// every tool call it rewrites or refuses, the permission requests of the
// agents beekeeper starts, the value scan of every tool result, and the
// agent shell's prelude at every session's start.
func Hooks(exe string) []Hook {
	hook := func(event, matcher, sub string, timeout int) Hook {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(hookEntry{Matcher: matcher, Hooks: []hookCommand{{Type: hookTypeCommand, Command: shellQuote(exe) + " " + hookWord + " " + sub, Timeout: timeout}}})
		return Hook{Event: event, Entry: bytes.TrimSpace(b.Bytes())}
	}
	return []Hook{
		hook("PreToolUse", "Bash|Read|Grep|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|TaskStop|mcp__.*", "pretooluse", 30),
		hook("UserPromptSubmit", "", "relay", 10),
		hook("Stop", "", "relay", 10),
		hook("PermissionRequest", "*", "permissionrequest", 300),
		hook("PostToolUse", "*", "posttooluse", 10),
		hook("SessionStart", "", "sessionstart", 10),
		hook("WorktreeCreate", "", "worktreecreate", 60),
		hook("WorktreeRemove", "", "worktreeremove", 60),
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
		for _, sub := range []string{"pretooluse", "permissionrequest", "posttooluse", "sessionstart"} {
			if exe := installedHookBinary(h.Command, " "+hookWord+" "+sub); filepath.Base(exe) == project.Name {
				subs = append(subs, sub)
			}
		}
	}
	return subs
}

// sameCommand reports whether a and b are entries of one hook each that run
// the same command.
func sameCommand(a, b json.RawMessage) bool {
	var x, y hookEntry
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil &&
		len(x.Hooks) == 1 && len(y.Hooks) == 1 && x.Hooks[0].Command == y.Hooks[0].Command
}

// planHooks adds the hooks a settings file lacks and updates the entries of
// this binary's hooks in place.
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
		switch i, state := find(list, want, m.hookCandidates()); state {
		case fileSame:
			p.add("ok", what, "", nil)
		case fileUpdated:
			list[i] = want.Entry
			if mine >= 0 {
				m.Hooks[mine] = want
			} else {
				m.Hooks = append(m.Hooks, want)
			}
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
	if !s.has(autoMemoryKey) {
		s.top.set(autoMemoryKey, json.RawMessage("false"))
		m.SettingsKeys = append(m.SettingsKeys, autoMemoryKey)
		changed = true
		p.add("add", e.Settings+": "+autoMemoryKey+" false", "", nil)
	}
	if !changed {
		return nil
	}
	if !s.existed {
		m.SettingsCreated = true
		m.addDirs(filepath.Dir(e.Settings))
	}
	raw := s.encode()
	p.add(writeVerb(s.existed), e.Settings, "", func() error {
		return writeFile(e.Settings, raw)
	})
	return nil
}

func writeVerb(existed bool) string {
	if existed {
		return "update"
	}
	return "write"
}

// find is where want stands in list: as it is, as the entry install added
// earlier (one of added) or one running the same
// command with another matcher or timeout (fileUpdated), as a beekeeper hook
// of the same subcommand with another command (fileKept), or absent
// (fileNew).
func find(list []json.RawMessage, want Hook, added []Hook) (int, fileState) {
	if slices.ContainsFunc(list, func(r json.RawMessage) bool { return sameJSON(r, want.Entry) }) {
		return 0, fileSame
	}
	for _, candidate := range added {
		if candidate.Event != want.Event {
			continue
		}
		if i := slices.IndexFunc(list, func(r json.RawMessage) bool { return sameJSON(r, candidate.Entry) }); i >= 0 {
			return i, fileUpdated
		}
	}

	if i := slices.IndexFunc(list, func(r json.RawMessage) bool { return sameCommand(r, want.Entry) }); i >= 0 {
		return i, fileUpdated
	}
	sub := command(want.Entry)
	if len(sub) > 0 && slices.ContainsFunc(list, func(r json.RawMessage) bool { return slices.Equal(command(r), sub) }) {
		return 0, fileKept
	}
	return 0, fileNew
}

// planUnhooks removes the hooks install added and the keys it created.
func (e Env) planUnhooks(p *plan, m Manifest, removed map[string]bool) error {
	if len(m.hookCandidates()) == 0 {
		return nil
	}
	s, err := readSettings(e.Settings)
	if err != nil || !s.existed {
		return err
	}
	changed := false
	for _, h := range m.hookCandidates() {
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
	if slices.Contains(m.SettingsKeys, autoMemoryKey) && s.has(autoMemoryKey) {
		s.top.del(autoMemoryKey)
		changed = true
		p.add("remove", e.Settings+": "+autoMemoryKey, "", nil)
	}
	switch {
	case !changed:
	case m.SettingsCreated && len(s.top) == 0 && !symlink(e.Settings):
		removed[e.Settings] = true
		p.add("remove", e.Settings, "", func() error { return os.Remove(e.Settings) })
	default:
		// A link introduced since install belongs to the user. Keep it and
		// remove our hooks through it, even when that leaves an empty object.
		raw := s.encode()
		p.add("update", e.Settings, "", func() error { return writeFile(e.Settings, raw) })
	}
	return nil
}

// agyHooks is a named hook entry as agy reads it: PreInvocation, an event
// of no tool, lists the commands themselves, and PreToolUse lists matchers,
// each with its commands. agy rejects the whole entry when a PreInvocation
// item is a matcher entry: "command hook must specify 'command'".
type agyHooks struct {
	PreInvocation []hookCommand `json:"PreInvocation"`
	PreToolUse    []hookEntry   `json:"PreToolUse"`
}

// AgyHook is the named hook entry install writes under project.Name in
// agy's hooks.json (~/.gemini/config/hooks.json): PreInvocation for mid-turn
// message injection and PreToolUse for the tool guards.
func AgyHook(exe string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(agyHooks{
		PreInvocation: []hookCommand{{
			Type:    hookTypeCommand,
			Command: shellQuote(exe) + " " + hookWord + " preinvocation",
			Timeout: 30,
		}},
		PreToolUse: []hookEntry{{
			Matcher: "*",
			Hooks: []hookCommand{{
				Type:    hookTypeCommand,
				Command: shellQuote(exe) + " " + hookWord + " pretooluse --format agy",
				Timeout: 30,
			}},
		}},
	})
	return bytes.TrimSpace(b.Bytes())
}

// agyHookBinary returns the binary path and timeout of an agy hook entry
// that runs "<exe> hook pretooluse --format agy", or ("", 0) when raw is
// not such an entry.
func agyHookBinary(raw json.RawMessage) (string, int) {
	var events map[string]json.RawMessage
	if json.Unmarshal(raw, &events) != nil {
		return "", 0
	}
	// Match agy's typed decoding: case variants and duplicates apply in
	// source order, and null leaves the current boolean unchanged.
	metadata := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: true}
	if json.Unmarshal(raw, &metadata) != nil || !metadata.Enabled {
		return "", 0
	}
	var list []hookEntry
	if json.Unmarshal(events["PreToolUse"], &list) != nil || len(list) != 1 || list[0].Matcher != "*" || len(list[0].Hooks) != 1 || list[0].Hooks[0].Type != hookTypeCommand {
		return "", 0
	}
	for _, suffix := range []string{" hook pretooluse --format agy", " hook pretooluse --format=agy"} {
		if exe := installedHookBinary(list[0].Hooks[0].Command, suffix); exe != "" {
			return exe, list[0].Hooks[0].Timeout
		}
	}
	return "", 0
}

// installedHookBinary recognizes only the literal executable spelling generated
// by shellQuote, so shell operators or expansions cannot masquerade as a guard.
func installedHookBinary(command, suffix string) string {
	if !strings.HasSuffix(command, suffix) {
		return ""
	}
	quoted := strings.TrimSuffix(command, suffix)
	exe := quoted
	if strings.HasPrefix(quoted, "'") && strings.HasSuffix(quoted, "'") && len(quoted) >= 2 {
		exe = strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'")
	}
	if shellQuote(exe) != quoted {
		return ""
	}
	return exe
}

// agyPreInvocationBinary returns the binary path and timeout of an agy hook
// entry whose PreInvocation list is the one command "<exe> hook
// preinvocation", or ("", 0) when raw is not such an entry.
func agyPreInvocationBinary(raw json.RawMessage) (string, int) {
	var events struct {
		PreInvocation []hookCommand `json:"PreInvocation"`
	}
	if json.Unmarshal(raw, &events) != nil {
		return "", 0
	}
	list := events.PreInvocation
	if len(list) != 1 || list[0].Type != hookTypeCommand {
		return "", 0
	}
	if exe := installedHookBinary(list[0].Command, " hook preinvocation"); exe != "" {
		return exe, list[0].Timeout
	}
	return "", 0
}

// planAgyHooks writes or updates the project.Name entry in agy's hooks.json
// when e.AgyHooks is set, leaving any other named hooks in that file
// untouched. An entry of an earlier install that agy rejects (PreInvocation
// as matcher entries) is replaced, and a hooks.json that is a link is kept.
// It returns why agy would reject the file as it stands after install, ""
// when agy accepts every entry: agy drops the whole file for one bad entry,
// beekeeper's hooks with it.
func (e Env) planAgyHooks(p *plan, m *Manifest) (string, error) {
	if e.AgyHooks == "" {
		return "", nil
	}
	if symlink(e.AgyHooks) {
		p.add("keep", e.AgyHooks, "a symlink to "+resolve(e.AgyHooks)+": install never writes through a link; "+
			"replace the link with a regular file for install to manage it, or edit the target yourself", nil)
		s, err := readSettings(e.AgyHooks)
		if err != nil {
			return "agy cannot read it either: " + err.Error(), nil
		}
		return agyFileRejects(s), nil
	}
	s, err := readSettings(e.AgyHooks)
	if err != nil {
		return "", err
	}
	want := AgyHook(e.Exe)
	what := e.AgyHooks + ": " + project.Name + " PreToolUse hook"
	cur, ok := s.top.get(project.Name)
	changed := false
	switch {
	case ok && sameJSON(cur, want):
		p.add("ok", what, "", nil)
	case ok && (m.ownsHook(cur, false) || func() bool { exe, _ := agyHookBinary(cur); return exe == e.Exe }()):
		s.top.set(project.Name, want)
		m.AgyHook = want
		changed = true
		p.add("update", what, "", nil)
	case ok:
		p.add("keep", what, "a beekeeper hook with another command", nil)
	default:
		s.top.set(project.Name, want)
		m.AgyHook = want
		changed = true
		p.add("add", what, "", nil)
	}
	if !changed {
		return agyFileRejects(s), nil
	}
	if !s.existed {
		m.AgyHooksCreated = true
		m.addDirs(filepath.Dir(e.AgyHooks))
	}
	raw := s.encode()
	p.add(writeVerb(s.existed), e.AgyHooks, "", func() error {
		return writeFile(e.AgyHooks, raw)
	})
	return agyFileRejects(s), nil
}

// agyFileRejects is why agy refuses the hooks.json s as a whole, naming the
// first entry it rejects in file order, or "" when agy accepts them all.
func agyFileRejects(s *settings) string {
	for _, m := range s.top {
		if why := agyhooks.EntryRejects(m.value); why != "" {
			return fmt.Sprintf("agy rejects the %s entry, so none of the file's hooks runs, %s's neither (%s)", m.key, project.Name, why)
		}
	}
	return ""
}

// failAgyHooks is the step that fails install, after every other step ran,
// when agy would reject the hooks.json at path for the reason why.
func failAgyHooks(p *plan, path, why string) {
	if why == "" {
		return
	}
	target := path
	if symlink(path) {
		target = resolve(path)
	}
	p.add("fail", path, why+"; fix "+target+": agy loads none of its hooks until then", func() error {
		return errors.New("agy would reject " + path + ": " + why)
	})
}

// planAgyUnhooks removes the project.Name entry install added to agy's
// hooks.json, leaving any other named hooks in place.
func (e Env) planAgyUnhooks(p *plan, m Manifest, removed map[string]bool) error {
	if len(m.AgyHook) == 0 || e.AgyHooks == "" || symlink(e.AgyHooks) {
		return nil
	}
	s, err := readSettings(e.AgyHooks)
	if err != nil || !s.existed {
		return err
	}
	cur, ok := s.top.get(project.Name)
	if !ok || !m.ownsHook(cur, false) {
		return nil
	}
	s.top.del(project.Name)
	p.add("remove", e.AgyHooks+": "+project.Name+" PreToolUse hook", "", nil)
	if m.AgyHooksCreated && len(s.top) == 0 {
		removed[e.AgyHooks] = true
		p.add("remove", e.AgyHooks, "", func() error { return os.Remove(e.AgyHooks) })
	} else {
		raw := s.encode()
		p.add("update", e.AgyHooks, "", func() error { return writeFile(e.AgyHooks, raw) })
	}
	return nil
}

// CheckAgyHooks checks that agy's hooks.json at path holds the PreInvocation
// and PreToolUse entry for project.Name in a shape agy accepts (running exe
// when exe is non-empty), and that agy accepts every other entry too: it
// drops the whole file for one it rejects. It returns a one-line finding for
// `beekeeper doctor`, or "" when healthy or path is empty. A hooks.json that
// is a link install never writes through, so its finding says to edit the
// target.
func CheckAgyHooks(path, exe, home string) string {
	if path == "" {
		return ""
	}
	short := (&plan{home: home}).tilde
	fix := "run `beekeeper install`"
	if symlink(path) {
		fix = "edit " + short(resolve(path)) + ", which install never writes through the link"
	}
	s, err := readSettings(path)
	if err != nil {
		return fmt.Sprintf("agy hooks: %s: %v; %s", short(path), err, fix)
	}
	if !s.existed {
		return fmt.Sprintf("agy hooks: %s is missing; run `beekeeper install`", short(path))
	}
	for _, m := range s.top {
		if m.key == project.Name {
			continue
		}
		if why := agyhooks.EntryRejects(m.value); why != "" {
			target := path
			if symlink(path) {
				target = resolve(path)
			}
			return fmt.Sprintf("agy hooks: %s: agy rejects the %s entry, so none of the file's hooks runs, %s's neither (%s); fix or remove that entry in %s",
				short(path), m.key, project.Name, why, short(target))
		}
	}
	cur, ok := s.top.get(project.Name)
	if !ok {
		return fmt.Sprintf("agy hooks: %s lacks the %s PreToolUse hook; %s", short(path), project.Name, fix)
	}
	if exe != "" && sameJSON(cur, AgyHook(exe)) {
		return ""
	}
	if why := agyhooks.EntryRejects(cur); why != "" {
		return fmt.Sprintf("agy hooks: %s: agy rejects the %s entry, so none of its hooks runs (%s); %s", short(path), project.Name, why, fix)
	}
	gotExe, timeout := agyHookBinary(cur)
	if gotExe == "" || timeout <= 0 {
		return fmt.Sprintf("agy hooks: %s: the %s PreToolUse hook differs from what install writes; %s", short(path), project.Name, fix)
	}
	preExe, preTimeout := agyPreInvocationBinary(cur)
	if preExe == "" || preTimeout <= 0 || preExe != gotExe {
		return fmt.Sprintf("agy hooks: %s: the %s PreInvocation hook differs from what install writes; %s", short(path), project.Name, fix)
	}
	if exe != "" && gotExe != exe {
		return fmt.Sprintf("agy hooks: %s runs %s instead of %s; %s", short(path), short(gotExe), short(exe), fix)
	}
	return ""
}

// AgyHooksUnguarded is why an agy started now would run without beekeeper's
// guard, or "" when its hooks.json at path loads with the project.Name
// PreToolUse hook: the file is missing or unreadable, agy rejects one of its
// entries and so drops them all, or it lacks beekeeper's entry or its guard.
func AgyHooksUnguarded(path string) string {
	if path == "" {
		return "agy's hooks.json has no path"
	}
	s, err := readSettings(path)
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	if !s.existed {
		return path + " is missing; run `beekeeper install`"
	}
	if why := agyFileRejects(s); why != "" {
		return path + ": " + why
	}
	cur, ok := s.top.get(project.Name)
	if !ok {
		return fmt.Sprintf("%s lacks the %s entry; run `beekeeper install`", path, project.Name)
	}
	if exe, _ := agyHookBinary(cur); exe == "" {
		return fmt.Sprintf("%s: the %s entry runs no `%s pretooluse --format agy` guard; run `beekeeper install`", path, project.Name, hookWord)
	}
	return ""
}

// agySkillsEntry is one path entry in agy's skills.json
// (~/.gemini/config/skills.json).
type agySkillsEntry struct {
	Path        string   `json:"path"`
	IncludeOnly []string `json:"include_only,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
}

// AgySkillsDir is the directory under stateDir where install writes the
// embedded skills that ~/.gemini/config/skills.json points at.
func AgySkillsDir(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, plugin.SkillsDir)
}

// AgySkillsEntry is the JSON object install adds to the "entries" list of
// agy's skills.json (~/.gemini/config/skills.json).
func AgySkillsEntry(skillsDir string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(agySkillsEntry{Path: skillsDir})
	return bytes.TrimSpace(b.Bytes())
}

// agySkillsEntryPath returns the path and whether raw is a plain
// {"path": "..."} object without include_only, exclude or extra keys.
func agySkillsEntryPath(raw json.RawMessage) (string, bool) {
	o, err := decodeObject(raw)
	if err != nil {
		return "", false
	}
	var e agySkillsEntry
	if json.Unmarshal(raw, &e) != nil || e.Path == "" {
		return "", false
	}
	return e.Path, len(o) == 1 && len(e.IncludeOnly) == 0 && len(e.Exclude) == 0
}

func expandHome(p, home string) string {
	if home != "" && strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// isBeekeeperSkillsEntry reports whether raw is a skills.json entry for
// beekeeper's skills, at wantDir or at another beekeeper skills folder.
func isBeekeeperSkillsEntry(raw json.RawMessage, wantDir, home string) bool {
	p, _ := agySkillsEntryPath(raw)
	if p == "" {
		return false
	}
	clean := filepath.Clean(expandHome(p, home))
	if wantDir != "" && clean == filepath.Clean(wantDir) {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasSuffix(clean, sep+filepath.Join(project.Name, plugin.SkillsDir)) ||
		strings.HasSuffix(clean, sep+filepath.Join(project.Name, "plugin", plugin.SkillsDir)) {
		return true
	}
	// Skill names are shared across unrelated collections and do not prove ownership.
	return false
}

// staleAgySkills returns the names of the embedded skills whose files under
// dir are missing or differ from what this binary embeds.
func staleAgySkills(dir string) []string {
	var stale []string
	for _, name := range plugin.SkillNames() {
		root := plugin.SkillsDir + "/" + name
		ok := true
		_ = fs.WalkDir(plugin.Skills, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			want, rerr := plugin.Skills.ReadFile(p)
			rel := strings.TrimPrefix(p, plugin.SkillsDir+"/")
			got, gerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // beekeeper's own skills directory
			if rerr != nil || gerr != nil || !bytes.Equal(got, want) {
				ok = false
			}
			return nil
		})
		if !ok {
			stale = append(stale, name)
		}
	}
	return stale
}

// writeEmbeddedSkills writes every file in plugin.Skills under
// plugin.SkillsDir into dir.
func writeEmbeddedSkills(dir string) error {
	return fs.WalkDir(plugin.Skills, plugin.SkillsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := plugin.Skills.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, plugin.SkillsDir+"/")
		return writeFile(filepath.Join(dir, filepath.FromSlash(rel)), raw)
	})
}

// planAgySkills writes the embedded skills into AgySkillsDir(e.StateDir) and
// registers that directory in agy's skills.json (~/.gemini/config/skills.json)
// when e.AgySkills is set, preserving every other entry and key in that file.
func (e Env) planAgySkills(p *plan, m *Manifest) error {
	if e.AgySkills == "" || e.StateDir == "" {
		return nil
	}
	if symlink(e.AgySkills) {
		p.add("keep", e.AgySkills, "a symlink to "+resolve(e.AgySkills)+": install never writes through a link; "+
			"replace the link with a regular file for install to manage it, or edit the target yourself", nil)
		return nil
	}
	s, err := readSettings(e.AgySkills)
	if err != nil {
		return err
	}
	var list []json.RawMessage
	hasEntries := s.has("entries")
	if raw, ok := s.top.get("entries"); ok {
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("%s entries: %w", e.AgySkills, err)
		}
	}
	skillsDir := AgySkillsDir(e.StateDir)
	want := AgySkillsEntry(skillsDir)
	what := e.AgySkills + ": " + project.Name + " skills"
	idx := slices.IndexFunc(list, func(r json.RawMessage) bool {
		return sameJSON(r, want) || len(m.AgySkills) > 0 && sameJSON(r, m.AgySkills) || isBeekeeperSkillsEntry(r, skillsDir, e.Spec.Home)
	})
	skillsCurrent := len(staleAgySkills(skillsDir)) == 0
	verb := "add"
	switch {
	case idx >= 0 && sameJSON(list[idx], want) && skillsCurrent:
		m.AgySkills = want
		m.AgySkillsDir = skillsDir
		p.add("ok", what, "", nil)
		return nil
	case idx >= 0:
		verb = writeVerb(true)
		list[idx] = want
	case !s.existed:
		verb = writeVerb(false)
		m.AgySkillsCreated = true
		m.addDirs(filepath.Dir(e.AgySkills))
		list = append(list, want)
	default:
		list = append(list, want)
	}
	if !hasEntries {
		m.AgySkillsEntries = true
	}
	s.top.set("entries", encodeArray(list))
	m.AgySkills = want
	m.AgySkillsDir = skillsDir
	raw := s.encode()
	p.add(verb, what, "", func() error {
		if err := writeEmbeddedSkills(skillsDir); err != nil {
			return err
		}
		return writeFile(e.AgySkills, raw)
	})
	return nil
}

// planAgyUnskills removes the skills.json entry and embedded skills
// directory that install wrote.
func (e Env) planAgyUnskills(p *plan, m Manifest, removed map[string]bool, purge bool) error {
	if len(m.AgySkills) == 0 && m.AgySkillsDir == "" {
		return nil
	}
	codexStillUses := func() bool {
		return m.CodexSkillsDir == m.AgySkillsDir && (e.CodexSkills != "" || CheckCodexSkills(filepath.Join(e.Spec.Home, ".codex", "skills"), e.StateDir, e.Spec.Home) == "")
	}
	rmDir := func() error {
		if m.AgySkillsDir == "" || purge || codexStillUses() {
			return nil
		}
		return os.RemoveAll(m.AgySkillsDir)
	}
	if e.AgySkills == "" || symlink(e.AgySkills) {
		if m.AgySkillsDir != "" && !purge && exists(m.AgySkillsDir) {
			p.add("remove", m.AgySkillsDir, "", rmDir)
		}
		return nil
	}
	s, err := readSettings(e.AgySkills)
	if err != nil || !s.existed {
		if m.AgySkillsDir != "" && !purge && exists(m.AgySkillsDir) {
			p.add("remove", m.AgySkillsDir, "", rmDir)
		}
		return err
	}
	rawEntries, ok := s.top.get("entries")
	if !ok {
		if m.AgySkillsDir != "" && !purge && exists(m.AgySkillsDir) {
			p.add("remove", m.AgySkillsDir, "", rmDir)
		}
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(rawEntries, &list); err != nil {
		return fmt.Errorf("%s entries: %w", e.AgySkills, err)
	}
	kept := slices.DeleteFunc(slices.Clone(list), func(r json.RawMessage) bool {
		return len(m.AgySkills) > 0 && sameJSON(r, m.AgySkills)
	})
	if len(kept) == len(list) {
		if m.AgySkillsDir != "" && !purge && exists(m.AgySkillsDir) {
			p.add("remove", m.AgySkillsDir, "", rmDir)
		}
		return nil
	}
	if len(kept) == 0 && m.AgySkillsEntries {
		s.top.del("entries")
	} else {
		s.top.set("entries", encodeArray(kept))
	}
	if m.AgySkillsCreated && len(s.top) == 0 {
		removed[e.AgySkills] = true
		p.add("remove", e.AgySkills, "", func() error {
			if err := rmDir(); err != nil {
				return err
			}
			return os.Remove(e.AgySkills)
		})
		return nil
	}
	raw := s.encode()
	p.add("remove", e.AgySkills+": "+project.Name+" skills", "", func() error {
		if err := rmDir(); err != nil {
			return err
		}
		return writeFile(e.AgySkills, raw)
	})
	return nil
}

// CheckAgySkills checks that agy's skills.json at path registers
// AgySkillsDir(stateDir) and that the skill files there match what this
// binary embeds. It returns a one-line finding for `beekeeper doctor`, or ""
// when healthy or path is empty.
func CheckAgySkills(path, stateDir, home string) string {
	if path == "" || stateDir == "" {
		return ""
	}
	short := (&plan{home: home}).tilde
	s, err := readSettings(path)
	if err != nil {
		return fmt.Sprintf("agy skills: %s: %v; run `beekeeper install`", short(path), err)
	}
	if !s.existed {
		return fmt.Sprintf("agy skills: %s is missing; run `beekeeper install`", short(path))
	}
	rawEntries, ok := s.top.get("entries")
	if !ok {
		return fmt.Sprintf("agy skills: %s lacks the %s skills entry; run `beekeeper install`", short(path), project.Name)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(rawEntries, &list); err != nil {
		return fmt.Sprintf("agy skills: %s: entries: %v; run `beekeeper install`", short(path), err)
	}
	wantDir := AgySkillsDir(stateDir)
	idx := slices.IndexFunc(list, func(r json.RawMessage) bool {
		return isBeekeeperSkillsEntry(r, wantDir, home)
	})
	if idx < 0 {
		return fmt.Sprintf("agy skills: %s lacks the %s skills entry; run `beekeeper install`", short(path), project.Name)
	}
	gotPath, plain := agySkillsEntryPath(list[idx])
	if !plain {
		return fmt.Sprintf("agy skills: %s: the %s skills entry differs from what install writes; run `beekeeper install`", short(path), project.Name)
	}
	gotDir := filepath.Clean(expandHome(gotPath, home))
	if gotDir != filepath.Clean(wantDir) {
		return fmt.Sprintf("agy skills: %s points to %s instead of %s; run `beekeeper install`", short(path), short(gotDir), short(wantDir))
	}
	if !exists(wantDir) {
		return fmt.Sprintf("agy skills: %s is missing; run `beekeeper install`", short(wantDir))
	}
	if stale := staleAgySkills(wantDir); len(stale) > 0 {
		return fmt.Sprintf("agy skills: %s serves the skills %s unlike %s %s; run `beekeeper install`",
			short(wantDir), strings.Join(stale, ", "), project.Name, project.Version())
	}
	return ""
}

// CodexHook is the PreToolUse entry install writes under hooks.PreToolUse in
// Codex's hooks.json (~/.codex/hooks.json).
func CodexHook(exe string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(hookEntry{
		Matcher: ".*",
		Hooks: []hookCommand{{
			Type:    hookTypeCommand,
			Command: shellQuote(exe) + " " + hookWord + " pretooluse --format codex",
			Timeout: 30,
		}},
	})
	return bytes.TrimSpace(b.Bytes())
}

// codexHookBinary returns the binary path, matcher and timeout of a Codex
// PreToolUse entry that runs "<exe> hook pretooluse --format codex", or
// ("", "", 0) when raw is not such an entry.
func codexHookBinary(raw json.RawMessage) (bin, matcher string, timeout int) {
	var e hookEntry
	if json.Unmarshal(raw, &e) != nil || len(e.Hooks) != 1 || e.Hooks[0].Type != hookTypeCommand {
		return "", "", 0
	}
	for _, suffix := range []string{" hook pretooluse --format codex", " hook pretooluse --format=codex"} {
		if exe := installedHookBinary(e.Hooks[0].Command, suffix); exe != "" {
			return exe, e.Matcher, e.Hooks[0].Timeout
		}
	}
	return "", "", 0
}

func isBeekeeperCodexHook(raw json.RawMessage) bool {
	bin, _, _ := codexHookBinary(raw)
	return filepath.Base(bin) == project.Name
}

// CodexWorkerHooks returns the hooks.json path inside beekeeper's isolated
// worker CODEX_HOME (<stateDir>/codex/home/hooks.json), or "" when stateDir is
// empty.
func CodexWorkerHooks(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "codex", "home", "hooks.json")
}

// CodexWorkerSkills returns the skills path inside beekeeper's isolated worker
// CODEX_HOME (<stateDir>/codex/home/skills), or "" when stateDir is empty.
func CodexWorkerSkills(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "codex", "home", "skills")
}

// planCodexHooks writes or updates the PreToolUse entry in Codex's hooks.json
// (~/.codex/hooks.json) and in the isolated worker CODEX_HOME
// (<stateDir>/codex/home/hooks.json) when e.CodexHooks is set, preserving any
// other hooks in those files.
func (e Env) planCodexHooks(p *plan, m *Manifest) error {
	if e.CodexHooks == "" {
		return nil
	}
	// Keep the prior ownership entry stable while planning both files: the first
	// file may update the manifest to the new executable path.
	previousHook := m.CodexHook
	if err := e.planCodexHooksFile(p, m, previousHook, e.CodexHooks, &m.CodexHooksKeys, &m.CodexHooksCreated); err != nil {
		return err
	}
	if workerHooks := CodexWorkerHooks(e.StateDir); workerHooks != "" && filepath.Clean(workerHooks) != filepath.Clean(e.CodexHooks) {
		if err := e.planCodexHooksFile(p, m, previousHook, workerHooks, &m.CodexWorkerHooksKeys, &m.CodexWorkerHooksCreated); err != nil {
			return err
		}
	}
	return nil
}

func (e Env) planCodexHooksFile(p *plan, m *Manifest, previousHook json.RawMessage, path string, keys *[]string, created *bool) error {
	if symlink(path) {
		p.add("keep", path, "a symlink to "+resolve(path)+": install never writes through a link; "+
			"replace the link with a regular file for install to manage it, or edit the target yourself", nil)
		return nil
	}
	s, err := readSettings(path)
	if err != nil {
		return err
	}
	list, err := s.hooks("PreToolUse")
	if err != nil {
		return err
	}
	want := CodexHook(e.Exe)
	what := path + ": " + project.Name + " PreToolUse hook"
	idx := slices.IndexFunc(list, func(r json.RawMessage) bool {
		if sameJSON(r, want) || m.ownsHook(r, true) || len(previousHook) > 0 && sameJSON(r, previousHook) {
			return true
		}
		bin, _, _ := codexHookBinary(r)
		return bin == e.Exe
	})
	otherIdx := slices.IndexFunc(list, isBeekeeperCodexHook)
	changed := false
	switch {
	case idx >= 0 && sameJSON(list[idx], want):
		m.CodexHook = want
		p.add("ok", what, "", nil)
	case idx >= 0:
		list[idx] = want
		m.CodexHook = want
		changed = true
		p.add("update", what, "", nil)
	case otherIdx >= 0:
		p.add("keep", what, "a beekeeper hook with another command", nil)
	default:
		if !s.has("hooks") && !slices.Contains(*keys, "hooks") {
			*keys = append(*keys, "hooks")
		}
		if _, ok := s.hooksObj()["PreToolUse"]; !ok && !slices.Contains(*keys, "hooks.PreToolUse") {
			*keys = append(*keys, "hooks.PreToolUse")
		}
		list = append(list, want)
		m.CodexHook = want
		changed = true
		p.add("add", what, "", nil)
	}
	if !changed {
		return nil
	}
	s.setHooks("PreToolUse", list)
	if !s.existed {
		*created = true
		m.addDirs(filepath.Dir(path))
	}
	raw := s.encode()
	p.add(writeVerb(s.existed), path, "", func() error {
		return writeFile(path, raw)
	})
	return nil
}

// planCodexUnhooks removes the PreToolUse entry install added to Codex's
// hooks.json files, leaving any other hooks in place.
func (e Env) planCodexUnhooks(p *plan, m Manifest, removed map[string]bool) error {
	if len(m.CodexHook) == 0 || e.CodexHooks == "" {
		return nil
	}
	if err := planCodexUnhooksFile(p, m, e.CodexHooks, m.CodexHooksKeys, m.CodexHooksCreated, removed); err != nil {
		return err
	}
	if workerHooks := CodexWorkerHooks(e.StateDir); workerHooks != "" && filepath.Clean(workerHooks) != filepath.Clean(e.CodexHooks) {
		if err := planCodexUnhooksFile(p, m, workerHooks, m.CodexWorkerHooksKeys, m.CodexWorkerHooksCreated, removed); err != nil {
			return err
		}
	}
	return nil
}

func planCodexUnhooksFile(p *plan, m Manifest, path string, keys []string, created bool, removed map[string]bool) error {
	if path == "" || symlink(path) {
		return nil
	}
	s, err := readSettings(path)
	if err != nil || !s.existed {
		return err
	}
	list, err := s.hooks("PreToolUse")
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(slices.Clone(list), func(r json.RawMessage) bool {
		return m.ownsHook(r, true)
	})
	if len(kept) == len(list) {
		return nil
	}
	p.add("remove", path+": "+project.Name+" PreToolUse hook", "", nil)
	if len(kept) == 0 && slices.Contains(keys, "hooks.PreToolUse") {
		s.delHooks("PreToolUse")
	} else {
		s.setHooks("PreToolUse", kept)
	}
	if slices.Contains(keys, "hooks") && len(s.hooksObj()) == 0 {
		s.top.del("hooks")
	}
	if created && len(s.top) == 0 {
		removed[path] = true
		p.add("remove", path, "", func() error { return os.Remove(path) })
	} else {
		raw := s.encode()
		p.add("update", path, "", func() error { return writeFile(path, raw) })
	}
	return nil
}

// CheckCodexHooks checks that Codex's hooks.json at path holds the PreToolUse
// entry for project.Name (running exe when exe is non-empty); it returns a
// one-line finding for `beekeeper doctor`, or "" when healthy or path is
// empty.
func CheckCodexHooks(path, exe, home string) string {
	if path == "" {
		return ""
	}
	short := (&plan{home: home}).tilde
	s, err := readSettings(path)
	if err != nil {
		return fmt.Sprintf("codex hooks: %s: %v; run `beekeeper install`", short(path), err)
	}
	if !s.existed {
		return fmt.Sprintf("codex hooks: %s is missing; run `beekeeper install`", short(path))
	}
	list, err := s.hooks("PreToolUse")
	if err != nil {
		return fmt.Sprintf("codex hooks: %s: %v; run `beekeeper install`", short(path), err)
	}
	want := CodexHook(exe)
	if exe != "" && slices.ContainsFunc(list, func(r json.RawMessage) bool { return sameJSON(r, want) }) {
		return ""
	}
	for _, r := range list {
		gotExe, matcher, timeout := codexHookBinary(r)
		if gotExe == "" {
			continue
		}
		if timeout <= 0 || (matcher != ".*" && matcher != "") {
			return fmt.Sprintf("codex hooks: %s: the %s PreToolUse hook differs from what install writes; run `beekeeper install`", short(path), project.Name)
		}
		if exe != "" && gotExe != exe {
			return fmt.Sprintf("codex hooks: %s runs %s instead of %s; run `beekeeper install`", short(path), short(gotExe), short(exe))
		}
		return ""
	}
	return fmt.Sprintf("codex hooks: %s lacks the %s PreToolUse hook; run `beekeeper install`", short(path), project.Name)
}

// planCodexSkills writes the embedded skills into AgySkillsDir(e.StateDir) and
// symlinks each embedded skill into e.CodexSkills (~/.codex/skills/<name>)
// when e.CodexSkills is set, leaving any other skill directories in place.
func (e Env) planCodexSkills(p *plan, m *Manifest) error {
	if e.CodexSkills == "" || e.StateDir == "" {
		return nil
	}
	skillsDir := AgySkillsDir(e.StateDir)
	what := e.CodexSkills + ": " + project.Name + " skills"
	if symlink(e.CodexSkills) {
		target := filepath.Clean(resolve(e.CodexSkills))
		if target == filepath.Clean(skillsDir) && len(staleAgySkills(skillsDir)) == 0 {
			m.CodexSkillsDir = skillsDir
			p.add("ok", what, "", nil)
			return nil
		}
		p.add("keep", e.CodexSkills, "a symlink to "+resolve(e.CodexSkills)+": install never writes through a link; "+
			"replace the link with a directory for install to manage it, or edit the target yourself", nil)
		return nil
	}
	var names []string
	linksOK := true
	anyExisted := exists(e.CodexSkills)
	var links []string
	for _, name := range plugin.SkillNames() {
		dst := filepath.Join(e.CodexSkills, name)
		wantTarget := filepath.Join(skillsDir, name)
		gotTarget, err := os.Readlink(dst)
		if err != nil || filepath.Clean(gotTarget) != filepath.Clean(wantTarget) {
			if exists(dst) {
				p.add("keep", dst, "an existing skill install did not write", nil)
				continue
			}
			linksOK = false
		}
		names = append(names, name)
		links = append(links, dst)
	}
	workerSkills := CodexWorkerSkills(e.StateDir)
	if workerSkills != "" && filepath.Clean(workerSkills) != filepath.Clean(e.CodexSkills) {
		links = append(links, workerSkills)
		gotTarget, err := os.Readlink(workerSkills)
		if err != nil || filepath.Clean(gotTarget) != filepath.Clean(skillsDir) {
			linksOK = false
		}
	}
	userAgentsMD := filepath.Join(filepath.Dir(e.CodexSkills), "AGENTS.md")
	workerAgentsMD := filepath.Join(e.StateDir, "codex", "home", "AGENTS.md")
	hasUserAgentsMD := exists(userAgentsMD) && filepath.Clean(resolve(userAgentsMD)) != filepath.Clean(workerAgentsMD)
	if hasUserAgentsMD {
		links = append(links, workerAgentsMD)
		gotTarget, err := os.Readlink(workerAgentsMD)
		if err != nil || filepath.Clean(gotTarget) != filepath.Clean(userAgentsMD) {
			linksOK = false
		}
	}
	skillsCurrent := len(staleAgySkills(skillsDir)) == 0
	if linksOK && skillsCurrent {
		m.CodexSkillLinks = links
		m.CodexSkillsDir = skillsDir
		p.add("ok", what, "", nil)
		return nil
	}
	if !anyExisted {
		m.addDirs(e.CodexSkills)
	}
	if workerSkills != "" {
		m.addDirs(filepath.Dir(workerSkills))
	}
	m.CodexSkillLinks = links
	m.CodexSkillsDir = skillsDir
	verb := "add"
	if anyExisted {
		verb = "update"
	}
	p.add(verb, what, "", func() error {
		if err := writeEmbeddedSkills(skillsDir); err != nil {
			return err
		}
		if err := os.MkdirAll(e.CodexSkills, 0o750); err != nil {
			return err
		}
		for _, name := range names {
			dst := filepath.Join(e.CodexSkills, name)
			wantTarget := filepath.Join(skillsDir, name)
			if gotTarget, err := os.Readlink(dst); err == nil && filepath.Clean(gotTarget) == filepath.Clean(wantTarget) {
				continue
			}
			if err := os.Symlink(wantTarget, dst); err != nil {
				return err
			}
		}
		if workerSkills != "" && filepath.Clean(workerSkills) != filepath.Clean(e.CodexSkills) {
			if err := os.MkdirAll(filepath.Dir(workerSkills), 0o700); err != nil {
				return err
			}
			if gotTarget, err := os.Readlink(workerSkills); err != nil || filepath.Clean(gotTarget) != filepath.Clean(skillsDir) {
				if fi, err := os.Lstat(workerSkills); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
					_ = os.Remove(workerSkills)
				}
				if _, err := os.Lstat(workerSkills); errors.Is(err, fs.ErrNotExist) {
					if err := os.Symlink(skillsDir, workerSkills); err != nil {
						return err
					}
				}
			}
		}
		if hasUserAgentsMD {
			if err := os.MkdirAll(filepath.Dir(workerAgentsMD), 0o700); err != nil {
				return err
			}
			if gotTarget, err := os.Readlink(workerAgentsMD); err != nil || filepath.Clean(gotTarget) != filepath.Clean(userAgentsMD) {
				if fi, err := os.Lstat(workerAgentsMD); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
					_ = os.Remove(workerAgentsMD)
				}
				if _, err := os.Lstat(workerAgentsMD); errors.Is(err, fs.ErrNotExist) {
					if err := os.Symlink(userAgentsMD, workerAgentsMD); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	return nil
}

// planCodexUnskills removes the skill symlinks install added under
// e.CodexSkills (and the unpacked skills directory if not shared with Agy).
func (e Env) planCodexUnskills(p *plan, m Manifest, removed map[string]bool, purge bool) error {
	if len(m.CodexSkillLinks) == 0 && m.CodexSkillsDir == "" {
		return nil
	}
	var toRemove []string
	for _, dst := range m.CodexSkillLinks {
		// Reconstruct the target install wrote, including the two worker
		// links. Readlink also works when the original target is now missing.
		wantTarget := filepath.Join(m.CodexSkillsDir, filepath.Base(dst))
		switch dst {
		case CodexWorkerSkills(e.StateDir):
			wantTarget = m.CodexSkillsDir
		case filepath.Join(e.StateDir, "codex", "home", "AGENTS.md"):
			if e.CodexSkills == "" {
				continue // The original user rules location is unknown.
			}
			wantTarget = filepath.Join(filepath.Dir(e.CodexSkills), "AGENTS.md")
		}
		if target, err := os.Readlink(dst); err == nil && m.CodexSkillsDir != "" && filepath.Clean(target) == filepath.Clean(wantTarget) {
			toRemove = append(toRemove, dst)
			removed[dst] = true
		} else if symlink(dst) {
			p.add("keep", dst, "changed since beekeeper install wrote it", nil)
		}
	}
	agyStillUses := func() bool {
		return m.CodexSkillsDir == m.AgySkillsDir && (e.AgySkills == "" || CheckAgySkills(e.AgySkills, e.StateDir, e.Spec.Home) == "")
	}
	rmDir := func() error {
		for _, dst := range toRemove {
			if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if m.CodexSkillsDir != "" && !purge && !agyStillUses() {
			return os.RemoveAll(m.CodexSkillsDir)
		}
		return nil
	}
	if len(toRemove) > 0 {
		what := e.CodexSkills
		if what == "" {
			what = filepath.Dir(toRemove[0])
		}
		p.add("remove", what+": "+project.Name+" skills", "", rmDir)
		return nil
	}
	if m.CodexSkillsDir != "" && !purge && !agyStillUses() && exists(m.CodexSkillsDir) {
		p.add("remove", m.CodexSkillsDir, "", rmDir)
	}
	return nil
}

// CheckCodexSkills checks that Codex's skills directory at path links all
// embedded skills in AgySkillsDir(stateDir) and that the skill files there
// match what this binary embeds. Returns a one-line finding for
// `beekeeper doctor`, or "" when healthy or path is empty.
func CheckCodexSkills(path, stateDir, home string) string {
	if path == "" || stateDir == "" {
		return ""
	}
	short := (&plan{home: home}).tilde
	wantDir := AgySkillsDir(stateDir)
	if !exists(path) {
		return fmt.Sprintf("codex skills: %s is missing; run `beekeeper install`", short(path))
	}
	if symlink(path) {
		gotDir := filepath.Clean(resolve(path))
		if gotDir != filepath.Clean(wantDir) {
			return fmt.Sprintf("codex skills: %s points to %s instead of %s; run `beekeeper install`", short(path), short(gotDir), short(wantDir))
		}
	} else {
		for _, name := range plugin.SkillNames() {
			dst := filepath.Join(path, name)
			wantTarget := filepath.Join(wantDir, name)
			gotTarget, err := os.Readlink(dst)
			if err != nil {
				return fmt.Sprintf("codex skills: %s lacks the %s skill link; run `beekeeper install`", short(path), name)
			}
			if !filepath.IsAbs(gotTarget) {
				gotTarget = filepath.Join(path, gotTarget)
			}
			if filepath.Clean(gotTarget) != filepath.Clean(wantTarget) {
				return fmt.Sprintf("codex skills: %s/%s points to %s instead of %s; run `beekeeper install`", short(path), name, short(gotTarget), short(wantTarget))
			}
		}
	}
	if !exists(wantDir) {
		return fmt.Sprintf("codex skills: %s is missing; run `beekeeper install`", short(wantDir))
	}
	if stale := staleAgySkills(wantDir); len(stale) > 0 {
		return fmt.Sprintf("codex skills: %s serves the skills %s unlike %s %s; run `beekeeper install`",
			short(wantDir), strings.Join(stale, ", "), project.Name, project.Version())
	}
	return ""
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
	// Validate the entire document, including its closing delimiter and
	// trailing input, before trusting any hook entries it contains.
	if !json.Valid(raw) {
		return nil, errors.New("not valid JSON")
	}
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
