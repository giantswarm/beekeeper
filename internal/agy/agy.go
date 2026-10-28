package agy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// The session states: busy while a turn runs, idle while it waits for a
// message, ended when no process runs the session.
const (
	StateBusy  = "busy"
	StateIdle  = "idle"
	StateEnded = "ended"
)

// helpers are the agy subcommands that run no session.
var helpers = map[string]bool{
	"agent": true, "agents": true, "changelog": true, "help": true,
	"install": true, "mcp": true, "mic-serve": true, "models": true,
	"plugin": true, "plugins": true, "remote-control": true, "update": true,
}

// valueFlags are the agy flags that consume a following argument on the
// command line instead of naming a subcommand.
var valueFlags = map[string]bool{
	"--conversation": true, "-r": true, "--resume": true,
	"--input-format": true, "--json-schema": true, "--log-file": true,
	"--mode": true, "--model": true, "--output-format": true,
	"--print-timeout": true, "--project": true,
	"-p": true, "--print": true, "--prompt": true, "--prompt-interactive": true,
}

// runsSession reports whether p is an agy process that runs a session: no
// helper subcommand, no --help/--version, and no one-shot `/usage` probe.
func runsSession(p *proc.Process) bool {
	if p.Comm != Comm {
		return false
	}
	args := p.Args[min(1, len(p.Args)):]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help" || a == "-h" || a == "--version" || a == "-v":
			return false
		case a == "-p" || a == "--print" || a == "--prompt":
			if i+1 < len(args) && strings.HasPrefix(strings.TrimSpace(args[i+1]), "/") {
				return false
			}
			i++
		case strings.HasPrefix(a, "--print=") || strings.HasPrefix(a, "--prompt="):
			_, v, _ := strings.Cut(a, "=")
			if strings.HasPrefix(strings.TrimSpace(v), "/") {
				return false
			}
		case valueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"):
			continue
		default:
			return !helpers[a]
		}
	}
	return true
}

func underAgy(t *proc.Table, p *proc.Process) bool {
	for _, a := range t.Ancestors(p.PID) {
		if a.PID != p.PID && runsSession(a) {
			return true
		}
	}
	return false
}

func modelArg(args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--model="); ok {
			return v
		}
		if a == "--model" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func convArg(args []string) string {
	for i, a := range args {
		for _, pfx := range []string{"--conversation=", "--resume="} {
			if v, ok := strings.CutPrefix(a, pfx); ok {
				return cleanID(v)
			}
		}
		if (a == "--conversation" || a == "--resume" || a == "-r") && i+1 < len(args) {
			return cleanID(args[i+1])
		}
	}
	return ""
}

// resolveAgentConv returns the conversationId for an agent start id from its
// .conv link file or from the init event in its stream log.
func resolveAgentConv(stateDir, id string) string {
	id = cleanID(id)
	if id == "" || stateDir == "" {
		return ""
	}
	if b, err := os.ReadFile(AgentConvPath(stateDir, id)); err == nil { //nolint:gosec // cleaned agent id under stateDir
		if c := cleanID(string(b)); c != "" {
			return c
		}
	}
	if c := streamConvID(StreamPath(stateDir, id)); c != "" {
		return c
	}
	return ""
}

func streamConvID(logPath string) string {
	f, err := os.Open(filepath.Clean(logPath)) //nolint:gosec // stream log under stateDir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			if ev.ConversationID != "" {
				return cleanID(ev.ConversationID)
			}
			if ev.StepUpdate != nil && ev.StepUpdate.ConversationID != "" {
				return cleanID(ev.StepUpdate.ConversationID)
			}
		}
	}
	return ""
}

// streamState inspects the stream log for the latest turn state when the
// transcript has not been written yet.
func streamState(logPath string) (state string, ok bool) {
	buf, _ := claude.ReadWindow(logPath)
	if len(buf) == 0 {
		return "", false
	}
	for len(buf) > 0 {
		line := buf
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			line, buf = buf[:i], buf[i+1:]
		} else {
			buf = nil
		}
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil || ev.Event == "" {
			continue
		}
		switch ev.Event {
		case eventInit:
			state, ok = StateIdle, true
		case "step_update":
			state, ok = StateBusy, true
		case "result":
			state, ok = StateIdle, true
		}
	}
	return state, ok
}

const startSlack = 5 * time.Second

type tfile struct {
	path, id, cwd, title, model string
	created, mod                time.Time
}

func transcriptFiles(brainDir string, since time.Time) []tfile {
	if brainDir == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(brainDir, "*", ".system_generated", "logs", "transcript.jsonl"))
	var out []tfile
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || fi.ModTime().Before(since) {
			continue
		}
		convID := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))
		h, err := readHead(p, convID)
		if err != nil || h.id == "" {
			continue
		}
		out = append(out, tfile{
			path:    p,
			id:      h.id,
			cwd:     h.cwd,
			title:   h.title,
			model:   h.model,
			created: h.created,
			mod:     fi.ModTime(),
		})
	}
	slices.SortFunc(out, func(a, b tfile) int { return b.mod.Compare(a.mod) })
	return out
}

func assignTranscripts(files []tfile, procs []*proc.Process, taken map[string]bool, cwd func(pid int) string) map[int]tfile {
	out := map[int]tfile{}
	pick := func(p *proc.Process, matchCwd bool, ok func(f tfile, since time.Time) bool, better func(f, best tfile) bool) {
		since := p.Start.Add(-startSlack)
		var best *tfile
		for i, f := range files {
			if taken[f.path] || f.mod.Before(since) || !ok(f, since) {
				continue
			}
			if matchCwd && f.cwd != "" && f.cwd != cwd(p.PID) {
				continue
			}
			if best == nil || better(f, *best) {
				best = &files[i]
			}
		}
		if best != nil {
			taken[best.path] = true
			out[p.PID] = *best
		}
	}
	for _, p := range procs {
		pick(p, true, func(f tfile, since time.Time) bool { return !f.created.Before(since) },
			func(f, best tfile) bool { return f.created.Before(best.created) })
	}
	for _, p := range procs {
		if _, ok := out[p.PID]; !ok {
			pick(p, true, func(tfile, time.Time) bool { return true },
				func(f, best tfile) bool { return f.mod.After(best.mod) })
		}
	}
	return out
}

// Discover returns the running agy sessions, newest first.
func Discover(brainDir, stateDir string, t *proc.Table, now time.Time) []*claude.Session {
	var procs []*proc.Process
	for _, p := range t.ByPID {
		if runsSession(p) && !underAgy(t, p) {
			procs = append(procs, p)
		}
	}
	if len(procs) == 0 {
		return nil
	}
	slices.SortFunc(procs, func(a, b *proc.Process) int { return b.Start.Compare(a.Start) })

	files := transcriptFiles(brainDir, procs[len(procs)-1].Start.Add(-startSlack))
	byConv := make(map[string]tfile, len(files))
	for _, f := range files {
		byConv[f.id] = f
	}
	taken := map[string]bool{}
	explicit := map[int]tfile{}
	envByPID := map[int]map[string]string{}
	var unassigned []*proc.Process

	for _, p := range procs {
		env, _ := t.Environ(p.PID)
		envByPID[p.PID] = env
		var convID string
		if env != nil {
			if id := env[EnvAgent]; id != "" {
				convID = resolveAgentConv(stateDir, id)
			}
		}
		if convID == "" {
			convID = convArg(p.Args)
		}
		if convID != "" {
			if f, ok := byConv[convID]; ok {
				taken[f.path] = true
				explicit[p.PID] = f
				continue
			}
			tp := TranscriptPath(brainDir, convID)
			if fi, err := os.Stat(tp); err == nil {
				h, _ := readHead(tp, convID)
				f := tfile{path: tp, id: convID, cwd: h.cwd, title: h.title, model: h.model, created: h.created, mod: fi.ModTime()}
				taken[tp] = true
				explicit[p.PID] = f
				continue
			}
			explicit[p.PID] = tfile{id: convID}
			continue
		}
		unassigned = append(unassigned, p)
	}
	assigned := assignTranscripts(files, unassigned, taken, t.Cwd)

	var out []*claude.Session
	for _, p := range procs {
		s := &claude.Session{
			PID:     p.PID,
			Started: p.Start,
			Cwd:     t.Cwd(p.PID),
			Harness: Harness,
			State:   StateIdle,
			Model:   modelArg(p.Args),
		}
		var startID string
		if env := envByPID[p.PID]; env != nil {
			if id := env[EnvAgent]; id != "" {
				startID = id
				s.HostID = HostPrefix + id
			}
			s.Name = env[EnvName]
		}
		f, ok := explicit[p.PID]
		if !ok {
			f, ok = assigned[p.PID]
		}
		if ok {
			if f.id != "" {
				s.ID = f.id
			}
			if f.path != "" {
				fillFromFile(s, f.path, true)
			}
		}
		if s.Transcript == "" && startID != "" && stateDir != "" {
			if st, sok := streamState(StreamPath(stateDir, startID)); sok {
				s.State = st
			}
		}
		if s.Name == "" {
			s.Name = "agy in " + filepath.Base(s.Cwd)
		}
		s.Repo, s.Branch = claude.GitInfo(s.Cwd)
		s.MemMiB, s.Commands = claude.ProcessTree(t, p.PID, now)
		out = append(out, s)
	}
	return out
}

// Ended returns the agy sessions written to since since that no running
// session runs, most recently active first.
func Ended(brainDir string, running []*claude.Session, since time.Time) []*claude.Session {
	var out []*claude.Session
	for _, f := range transcriptFiles(brainDir, since) {
		if slices.ContainsFunc(running, func(s *claude.Session) bool {
			return s.Transcript == f.path || (s.ID != "" && s.ID == f.id)
		}) {
			continue
		}
		s := &claude.Session{ID: f.id, Cwd: f.cwd, Harness: Harness}
		fillFromFile(s, f.path, false)
		if s.Name == "" {
			if s.Cwd != "" {
				s.Name = "agy in " + filepath.Base(s.Cwd)
			} else {
				s.Name = "agy " + f.id[:min(8, len(f.id))]
			}
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *claude.Session) int { return b.LastActive.Compare(a.LastActive) })
	return out
}

type step struct {
	StepIndex       int        `json:"step_index"`
	Source          string     `json:"source"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	Content         string     `json:"content"`
	Thinking        string     `json:"thinking"`
	Model           string     `json:"model"`
	Cwd             string     `json:"cwd"`
	InputTokens     int64      `json:"input_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	ThinkingTokens  int64      `json:"thinking_tokens"`
	CacheReadTokens int64      `json:"cache_read_tokens"`
	CacheWrite      int64      `json:"cache_write_tokens"`
	ToolCalls       []toolCall `json:"tool_calls"`
}

type toolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type head struct {
	id, cwd, title, model string
	created               time.Time
}

const headBytes = 64 << 10

func readHead(path, convID string) (head, error) {
	f, err := os.Open(filepath.Clean(path)) //nolint:gosec // transcript file under brainDir
	if err != nil {
		return head{}, err
	}
	defer func() { _ = f.Close() }()
	h := head{id: convID}
	sc := bufio.NewScanner(io.LimitReader(f, headBytes))
	sc.Buffer(make([]byte, 0, headBytes), headBytes)
	for i := 0; i < 5 && sc.Scan(); i++ {
		var e step
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if h.created.IsZero() && !e.CreatedAt.IsZero() {
			h.created = e.CreatedAt
		}
		if h.cwd == "" && e.Cwd != "" {
			h.cwd = e.Cwd
		}
		if h.model == "" {
			if e.Model != "" {
				h.model = e.Model
			} else if m := extractModelSetting(e.Content); m != "" {
				h.model = m
			}
		}
		if h.title == "" && e.Type == stepUserInput {
			if txt := extractUserText(e.Content); txt != "" {
				first, _, _ := strings.Cut(txt, "\n")
				h.title = strings.TrimSpace(first)
				if len(h.title) > 80 {
					h.title = h.title[:80]
				}
			}
		}
	}
	return h, sc.Err()
}

func fillFromFile(s *claude.Session, path string, running bool) {
	convID := s.ID
	if convID == "" {
		convID = filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	}
	h, err := readHead(path, convID)
	if err == nil {
		if s.ID == "" {
			s.ID = h.id
		}
		if s.Cwd == "" && h.cwd != "" {
			s.Cwd = h.cwd
		}
		if s.Name == "" && h.title != "" {
			s.Name = h.title
		}
		if s.Model == "" && h.model != "" {
			s.Model = h.model
		}
	}
	s.Transcript = path
	if fi, err := os.Stat(path); err == nil {
		s.LastActive = fi.ModTime()
	}
	buf, _ := claude.ReadWindow(path)
	var last *step
	eachStep(buf, func(e *step) {
		if e.Model != "" {
			s.Model = e.Model
		} else if s.Model == "" {
			if m := extractModelSetting(e.Content); m != "" {
				s.Model = m
			}
		}
		last = e
	})
	if !running {
		s.State = StateEnded
		return
	}
	s.State = StateIdle
	if last != nil && stepBusy(last) {
		s.State = StateBusy
	}
}

func stepBusy(e *step) bool {
	switch e.Type {
	case stepUserInput, "GENERIC", "TOOL_RESPONSE":
		return true
	case stepPlannerResponse:
		if e.Status != "" && e.Status != "DONE" {
			return true
		}
		return len(e.ToolCalls) > 0
	}
	return false
}

func eachStep(buf []byte, fn func(*step)) {
	for len(buf) > 0 {
		line := buf
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			line, buf = buf[:i], buf[i+1:]
		} else {
			buf = nil
		}
		var e step
		if json.Unmarshal(line, &e) == nil && e.Type != "" {
			fn(&e)
		}
	}
}

// extractUserText returns the user request text from an agy USER_INPUT content
// field, unwrapping <USER_REQUEST>...</USER_REQUEST> when present and stripping
// trailing <ADDITIONAL_METADATA> or <USER_SETTINGS_CHANGE> blocks.
func extractUserText(content string) string {
	if _, rest, ok := strings.Cut(content, "<USER_REQUEST>"); ok {
		if body, _, ok := strings.Cut(rest, "</USER_REQUEST>"); ok {
			return strings.TrimSpace(body)
		}
	}
	for _, tag := range []string{"<ADDITIONAL_METADATA>", "<USER_SETTINGS_CHANGE>"} {
		if before, _, ok := strings.Cut(content, tag); ok {
			content = before
		}
	}
	return strings.TrimSpace(content)
}

var modelSettingRe = regexp.MustCompile("Model Selection` from [^\n]+ to ([^\n]+?)\\.(?:\n|$)")

// extractModelSetting converts an agy <USER_SETTINGS_CHANGE> display name
// (such as "Gemini 4 Argon (Medium)" or "Claude Opus 5.5 (High)") into its
// model selector ("gemini-4-argon-medium", "claude-opus-5-5-high").
func extractModelSetting(content string) string {
	m := modelSettingRe.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(m[1]))
	if strings.HasPrefix(s, "claude ") {
		s = strings.ReplaceAll(s, ".", "-")
	}
	s = strings.NewReplacer("(", "", ")", "").Replace(s)
	return strings.Join(strings.Fields(s), "-")
}

func unquoteString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var u string
		if json.Unmarshal([]byte(s), &u) == nil {
			return u
		}
	}
	return s
}

// normalizeArgs unquotes stringified agy tool arguments and maps agy's field
// names (CommandLine, toolSummary, Path) onto the standard keys (command,
// description, file_path) that claude.ToolGist and claude.Acts inspect.
func normalizeArgs(raw json.RawMessage) json.RawMessage {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil || len(in) == 0 {
		return raw
	}
	out := make(map[string]any, len(in)+3)
	for k, v := range in {
		if u := unquoteString(v); u != "" {
			out[k] = u
		} else {
			out[k] = v
		}
	}
	if cmd := unquoteString(in["CommandLine"]); cmd != "" && out["command"] == nil {
		out["command"] = cmd
	}
	if desc := unquoteString(in["toolSummary"]); desc != "" && out["description"] == nil {
		out["description"] = desc
	}
	if p := unquoteString(in["Path"]); p != "" && out["file_path"] == nil {
		out["file_path"] = p
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

// Tail returns the last n turns of an agy transcript: user messages and the
// model's text responses, without thinking, tool calls or tool results.
func Tail(path string, n int) ([]claude.Turn, error) { return tail(path, n, false) }

// Follow is Tail with the model's tool calls among the turns, as claude.Follow
// returns them.
func Follow(path string, n int) ([]claude.Turn, error) { return tail(path, n, true) }

func tail(path string, n int, tools bool) ([]claude.Turn, error) {
	buf, whole := claude.ReadWindow(path)
	if buf == nil && !whole {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	var turns []claude.Turn
	eachStep(buf, func(e *step) {
		switch e.Type {
		case stepUserInput:
			if txt := extractUserText(e.Content); txt != "" {
				turns = append(turns, claude.Turn{At: e.CreatedAt, Role: roleUser, Text: txt})
			}
		case stepPlannerResponse:
			if txt := strings.TrimSpace(e.Content); txt != "" {
				turns = append(turns, claude.Turn{At: e.CreatedAt, Role: "assistant", Text: txt})
			}
			if !tools {
				return
			}
			for _, tc := range e.ToolCalls {
				if tc.Name != "" {
					turns = append(turns, claude.Turn{
						At:   e.CreatedAt,
						Role: claude.RoleTool,
						Text: claude.ToolGist(tc.Name, normalizeArgs(tc.Args)),
					})
				}
			}
		}
	})
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return turns, nil
}

const busyGap = 5 * time.Minute

func hasToolError(e *step) bool {
	if e.Status == "ERROR" {
		return true
	}
	if _, rest, ok := strings.Cut(e.Content, "Command exited with code "); ok {
		code, _, _ := strings.Cut(strings.TrimSpace(rest), ".")
		code, _, _ = strings.Cut(code, "\n")
		return code != "" && code != "0"
	}
	return false
}

// ReadTranscript reads an agy transcript's window for the issues and
// repositories it acts on and its activity: turns, tool calls and errors,
// GitHub CLI calls, token counts, context and model requests for pricing.
func ReadTranscript(path string, now time.Time) (claude.Work, claude.Activity) {
	buf, whole := claude.ReadWindow(path)
	a := claude.Activity{Whole: whole}
	hourStart := now.Add(-time.Hour)
	acts := claude.NewActs()
	var cwd, model string
	var prevAt time.Time
	var pendingIDs []string

	eachStep(buf, func(e *step) {
		if e.Cwd != "" {
			cwd = e.Cwd
		}
		if e.Model != "" {
			model = e.Model
		} else if m := extractModelSetting(e.Content); m != "" {
			model = m
		}
		if a.Since.IsZero() && !e.CreatedAt.IsZero() {
			a.Since = e.CreatedAt
		}
		counts := []*claude.Counts{&a.Total}
		if !e.CreatedAt.IsZero() && !e.CreatedAt.Before(hourStart) {
			counts = append(counts, &a.LastHour)
		}
		if !prevAt.IsZero() && !e.CreatedAt.IsZero() {
			if d := e.CreatedAt.Sub(prevAt); d > 0 && d < busyGap {
				for _, c := range counts {
					c.Busy += d
				}
			}
		}
		if !e.CreatedAt.IsZero() {
			prevAt = e.CreatedAt
		}

		switch e.Type {
		case stepUserInput:
			for _, c := range counts {
				c.Turns++
			}
		case stepPlannerResponse:
			pendingIDs = nil
			for i, tc := range e.ToolCalls {
				norm := normalizeArgs(tc.Args)
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("%d:%d", e.StepIndex, i)
				}
				pendingIDs = append(pendingIDs, id)
				acts.Call(id, tc.Name, norm, cwd)
				gh := (tc.Name == "run_command" || tc.Name == "bash" || tc.Name == "Bash") && invokesGitHub(norm)
				for _, c := range counts {
					c.ToolCalls++
					if gh {
						c.GitHubCalls++
					}
				}
			}
			tok := claude.Tokens{
				Input:        e.InputTokens,
				Output:       e.OutputTokens + e.ThinkingTokens,
				CacheRead:    e.CacheReadTokens,
				CacheWrite5m: e.CacheWrite,
			}
			if tok.Sum() > 0 {
				for _, c := range counts {
					c.Tokens.Input += tok.Input
					c.Tokens.Output += tok.Output
					c.Tokens.CacheRead += tok.CacheRead
					c.Tokens.CacheWrite5m += tok.CacheWrite5m
				}
				a.Context = tok.Sum()
				reqModel := model
				if reqModel == "" {
					reqModel = Harness
				}
				a.AddRequest(e.CreatedAt, reqModel, tok, hourStart)
			}
			if model != "" {
				a.Model = model
			}
		case "GENERIC", "TOOL_RESPONSE":
			for _, id := range pendingIDs {
				acts.Result(id, e.Content)
			}
			if hasToolError(e) {
				for _, c := range counts {
					c.ToolErrors++
				}
			}
		}
	})
	if a.Model == "" && model != "" {
		a.Model = model
	}
	return acts.Work(string(buf)), a
}

func invokesGitHub(args json.RawMessage) bool {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &in) != nil {
		return false
	}
	return claude.InvokesGitHub(in.Command)
}

// SpendTranscripts reads the requests from from up to to of every agy
// transcript under brainDir. A file last written before from is skipped;
// transcript_full.jsonl is skipped so requests are not double-counted.
func SpendTranscripts(brainDir string, from, to time.Time) []claude.SpendTranscript {
	if brainDir == "" {
		return nil
	}
	var out []claude.SpendTranscript
	_ = filepath.WalkDir(brainDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || filepath.Ext(p) != ".jsonl" {
			return nil //nolint:nilerr // unreadable entry or absent brainDir is passed over
		}
		if filepath.Base(p) == "transcript_full.jsonl" {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.ModTime().Before(from) {
			return nil //nolint:nilerr // as above
		}
		out = append(out, claude.SpendTranscript{
			Subagent: filepath.Base(filepath.Dir(p)) == "subagents",
			Requests: spendRequests(p, from, to),
		})
		return nil
	})
	return out
}

func spendRequests(p string, from, to time.Time) []claude.SpendRequest {
	f, err := os.Open(filepath.Clean(p)) //nolint:gosec // transcript file under brainDir
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	convID := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))
	r := bufio.NewReaderSize(f, 1<<20)
	var out []claude.SpendRequest
	var model string
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e step
			if json.Unmarshal(line, &e) == nil {
				if e.Model != "" {
					model = e.Model
				} else if m := extractModelSetting(e.Content); m != "" {
					model = m
				}
				if e.Type == stepPlannerResponse {
					tok := claude.Tokens{
						Input:        e.InputTokens,
						Output:       e.OutputTokens + e.ThinkingTokens,
						CacheRead:    e.CacheReadTokens,
						CacheWrite5m: e.CacheWrite,
					}
					if tok.Sum() > 0 {
						reqModel := model
						if reqModel == "" {
							reqModel = Harness
						}
						out = append(out, claude.SpendRequest{
							ID:     fmt.Sprintf("%s:%d@%s", convID, e.StepIndex, e.CreatedAt.Format(time.RFC3339Nano)),
							At:     e.CreatedAt,
							Model:  reqModel,
							Tokens: tok,
						})
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return claude.InWindow(out, from, to)
}

type projectFile struct {
	ID               string `json:"id"`
	ProjectResources struct {
		Resources []struct {
			FolderURI string `json:"folderUri"`
			GitFolder struct {
				FolderURI string `json:"folderUri"`
			} `json:"gitFolder"`
		} `json:"resources"`
	} `json:"projectResources"`
}

// ProjectForDir searches projectsDir (*.json in sorted filename order) for an
// Antigravity project whose resource folderUri or gitFolder.folderUri matches
// "file://" + dir, returning its project id or "".
func ProjectForDir(projectsDir, dir string) string {
	if projectsDir == "" || dir == "" {
		return ""
	}
	want := "file://" + strings.TrimRight(filepath.Clean(dir), "/")
	paths, _ := filepath.Glob(filepath.Join(projectsDir, "*.json"))
	slices.Sort(paths)
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Clean(p)) //nolint:gosec // project config under projectsDir
		if err != nil {
			continue
		}
		var pf projectFile
		if json.Unmarshal(b, &pf) != nil || strings.TrimSpace(pf.ID) == "" {
			continue
		}
		for _, r := range pf.ProjectResources.Resources {
			for _, u := range []string{r.GitFolder.FolderURI, r.FolderURI} {
				if strings.TrimRight(strings.TrimSpace(u), "/") == want {
					return strings.TrimSpace(pf.ID)
				}
			}
		}
	}
	return ""
}

var modelSelectorRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ParseModelsOutput extracts the model selector IDs from the stdout of
// `agy models`, ignoring spinner carriage-return frames and headers.
func ParseModelsOutput(raw string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' }) {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		sel := fields[0]
		if !modelSelectorRe.MatchString(sel) || strings.EqualFold(sel, "fetching") {
			continue
		}
		if !slices.Contains(out, sel) {
			out = append(out, sel)
		}
	}
	return out
}
