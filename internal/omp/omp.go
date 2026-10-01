// Package omp finds the omp (oh-my-pi) agents running on the machine and
// what they are doing, from what is on disk: the omp processes (their
// working directory, and for an agent beekeeper started its environment)
// and the session files omp writes under ~/.omp/agent/sessions, one JSON
// line per entry: a title line (rewritten in place), the session header
// (id, cwd), model changes, and messages of the roles user, assistant and
// toolResult.
//
// omp keeps no record of which process runs which session file and holds
// none open: a process runs the session file of its working directory that
// it created (the header's time is the first one at or after its start),
// else the one written last since it started (a resumed session).
package omp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// Harness is the harness name omp sessions carry (claude.Session.Harness).
const Harness = "omp"

// Comm is the omp binary's process name.
const Comm = "omp"

// The environment beekeeper starts an omp agent with: its start id and its
// roster name, which no session file carries.
const (
	EnvAgent = "BEEKEEPER_OMP_AGENT"
	EnvName  = "BEEKEEPER_AGENT_NAME"
)

// HostPrefix makes a start id the agent's host session id on the roster:
// omp_<start id>.
const HostPrefix = "omp_"

// The session states: busy while a turn runs, idle while it waits for a
// message, ended when no process runs the session.
const (
	StateBusy  = "busy"
	StateIdle  = "idle"
	StateEnded = "ended"
)

// helpers are the omp subcommands that run no session; acp is one (an
// agent over stdio).
var helpers = map[string]bool{
	"agents": true, "auth-broker": true, "auth-gateway": true, "bench": true, "browser-relay": true,
	"cleanse": true, "clip": true, "collab": true, "commit": true, "completions": true, "compress": true,
	"config": true, "dry-balance": true, "find": true, "gallery": true, "gc": true, "git": true,
	"grep": true, "grievances": true, "if-bench": true, "images": true, "install": true, "join": true,
	"login": true, "models": true, "play": true, "plugin": true, "predict": true, "ps": true,
	"read": true, "render": true, "say": true, "search": true, "setup": true, "share": true,
	"shell": true, "skill": true, "ssh": true, "stats": true, "stream": true, "tiny-models": true,
	"token": true, "toks": true, "ttsr": true, "update": true, "usage": true, "worktree": true,
}

// runsSession says p is an omp process that runs a session: no helper
// subcommand, no --export, --help or --version.
func runsSession(p *proc.Process) bool {
	if p.Comm != Comm {
		return false
	}
	for _, a := range p.Args[min(1, len(p.Args)):] {
		switch {
		case a == "--export" || strings.HasPrefix(a, "--export=") || a == "--help" || a == "-h" || a == "--version" || a == "-v":
			return false
		case strings.HasPrefix(a, "-"):
			continue
		}
		return !helpers[a]
	}
	return true
}

// Discover returns the running omp sessions, newest first. A session's
// process may not have written its session file yet (omp creates it with
// the first message): it is listed without a transcript. A process that
// another omp process started (a subagent) belongs to that session.
func Discover(sessionsDir string, t *proc.Table, now time.Time) []*claude.Session {
	var procs []*proc.Process
	for _, p := range t.ByPID {
		if runsSession(p) && !underOmp(t, p) {
			procs = append(procs, p)
		}
	}
	if len(procs) == 0 {
		return nil
	}
	slices.SortFunc(procs, func(a, b *proc.Process) int { return b.Start.Compare(a.Start) })
	files := assign(sessionFiles(sessionsDir, procs[len(procs)-1].Start.Add(-startSlack)), procs, t.Cwd)
	var out []*claude.Session
	for _, p := range procs {
		s := &claude.Session{PID: p.PID, Started: p.Start, Cwd: t.Cwd(p.PID), Harness: Harness, State: StateIdle}
		if env, err := t.Environ(p.PID); err == nil {
			if id := env[EnvAgent]; id != "" {
				s.HostID = HostPrefix + id
			}
			s.Name = env[EnvName]
		}
		if f, ok := files[p.PID]; ok {
			fillFromFile(s, f.path, true)
		}
		if s.Name == "" {
			s.Name = "omp in " + filepath.Base(s.Cwd)
		}
		s.Repo, s.Branch = claude.GitInfo(s.Cwd)
		s.MemMiB, s.Commands = claude.ProcessTree(t, p.PID, now)
		out = append(out, s)
	}
	return out
}

// Ended returns the omp sessions written to since since that no running
// session runs, most recently active first.
func Ended(sessionsDir string, running []*claude.Session, since time.Time) []*claude.Session {
	var out []*claude.Session
	for _, f := range sessionFiles(sessionsDir, since) {
		if slices.ContainsFunc(running, func(s *claude.Session) bool { return s.Transcript == f.path }) {
			continue
		}
		s := &claude.Session{Cwd: f.cwd, Harness: Harness}
		fillFromFile(s, f.path, false)
		if s.Name == "" {
			s.Name = "omp in " + filepath.Base(s.Cwd)
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *claude.Session) int { return b.LastActive.Compare(a.LastActive) })
	return out
}

// startSlack is how much earlier than its process a session file may have
// been written to and still be the process's: a resumed session's file
// is older, and its process rewrites the title line at its start.
const startSlack = 5 * time.Second

// file is a session file with what its head says.
type file struct {
	path, id, cwd string
	// created is the header's time, mod the last write.
	created, mod time.Time
}

// sessionFiles returns the session files written to since since, newest
// first. Subagents' files live in a folder per session and are no sessions.
func sessionFiles(dir string, since time.Time) []file {
	paths, _ := filepath.Glob(filepath.Join(dir, "*", "*.jsonl"))
	var out []file
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || fi.ModTime().Before(since) {
			continue
		}
		h, err := readHead(p)
		if err != nil || h.id == "" {
			continue
		}
		out = append(out, file{path: p, id: h.id, cwd: h.cwd, created: h.created, mod: fi.ModTime()})
	}
	slices.SortFunc(out, func(a, b file) int { return b.mod.Compare(a.mod) })
	return out
}

// assign maps each process to the session file it runs. First every
// process takes the file it created: of cwd's, the first one created at or
// after its start, newest process first. Then a process without one takes
// the file of cwd written last since it started that no process took: the
// session it resumed.
func assign(files []file, procs []*proc.Process, cwd func(pid int) string) map[int]file {
	taken := map[string]bool{}
	out := map[int]file{}
	pick := func(p *proc.Process, ok func(f file, since time.Time) bool, better func(f, best file) bool) {
		since := p.Start.Add(-startSlack)
		var best *file
		for i, f := range files {
			if f.cwd == cwd(p.PID) && !taken[f.path] && !f.mod.Before(since) && ok(f, since) && (best == nil || better(f, *best)) {
				best = &files[i]
			}
		}
		if best != nil {
			taken[best.path] = true
			out[p.PID] = *best
		}
	}
	for _, p := range procs {
		pick(p, func(f file, since time.Time) bool { return !f.created.Before(since) },
			func(f, best file) bool { return f.created.Before(best.created) })
	}
	for _, p := range procs {
		if _, ok := out[p.PID]; !ok {
			pick(p, func(file, time.Time) bool { return true }, func(f, best file) bool { return f.mod.After(best.mod) })
		}
	}
	return out
}

// underOmp says an ancestor of p runs an omp session: p is its subagent.
func underOmp(t *proc.Table, p *proc.Process) bool {
	for _, a := range t.Ancestors(p.PID) {
		if a.PID != p.PID && runsSession(a) {
			return true
		}
	}
	return false
}

// entry is the part of a session file line beekeeper reads.
type entry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	// ID is the session id on the session header.
	ID      string `json:"id"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title"`
	Model   string `json:"model"`
	Message *struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stopReason"`
		IsError    bool            `json:"isError"`
		ToolName   string          `json:"toolName"`
		Model      string          `json:"model"`
		Usage      *usage          `json:"usage"`
		Snapshot   *struct {
			PromptTokens int64 `json:"promptTokens"`
		} `json:"contextSnapshot"`
	} `json:"message"`
}

type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Cost       *struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// block is a message content block: text, thinking or toolCall.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

const (
	roleUser       = "user"
	roleAssistant  = "assistant"
	roleToolResult = "toolResult"
)

// head is what a session file's first lines say: its title line and its
// session header.
type head struct {
	id, cwd, title string
	created        time.Time
}

// headBytes bounds the read of a session file's head: the title line is
// padded so omp can rewrite it in place, the header follows it.
const headBytes = 16 << 10

func readHead(path string) (head, error) {
	f, err := os.Open(path) //nolint:gosec // a session file under the configured folder
	if err != nil {
		return head{}, err
	}
	defer func() { _ = f.Close() }()
	var h head
	sc := bufio.NewScanner(io.LimitReader(f, headBytes))
	sc.Buffer(make([]byte, 0, headBytes), headBytes)
	for i := 0; i < 3 && sc.Scan(); i++ {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "title":
			h.title = strings.TrimSpace(e.Title)
		case "session":
			h.id, h.cwd, h.created = e.ID, e.Cwd, e.Timestamp
			if h.title == "" {
				h.title = strings.TrimSpace(e.Title)
			}
		}
	}
	return h, sc.Err()
}

// fillFromFile sets what the session file says: id, title, model, last
// activity and, for a running session, whether a turn runs.
func fillFromFile(s *claude.Session, path string, running bool) {
	h, err := readHead(path)
	if err != nil {
		return
	}
	s.ID, s.Transcript = h.id, path
	if s.Cwd == "" {
		s.Cwd = h.cwd
	}
	if s.Name == "" {
		s.Name = h.title
	}
	if fi, err := os.Stat(path); err == nil {
		s.LastActive = fi.ModTime()
	}
	buf, _ := claude.ReadWindow(path)
	var last *entry
	eachEntry(buf, func(e *entry) {
		switch {
		case e.Type == "model_change" && e.Model != "":
			s.Model = e.Model
		case e.Type == "title_change" && e.Title != "" && s.Name == h.title:
			s.Name = e.Title
			h.title = e.Title
		case e.Type == "message" && e.Message != nil:
			last = e
		}
	})
	if !running {
		s.State = StateEnded
		return
	}
	s.State = StateIdle
	if last != nil && busy(last) {
		s.State = StateBusy
	}
}

// busy says the session's turn still runs after its last message: the
// model has a message or a tool result to answer, or called a tool.
func busy(e *entry) bool {
	switch e.Message.Role {
	case roleUser, roleToolResult:
		return true
	case roleAssistant:
		return e.Message.StopReason == "toolUse"
	}
	return false
}

// eachEntry calls fn with every whole line of buf that decodes; a partial
// first line of a window is skipped.
func eachEntry(buf []byte, fn func(*entry)) {
	for len(buf) > 0 {
		line := buf
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			line, buf = buf[:i], buf[i+1:]
		} else {
			buf = nil
		}
		var e entry
		if json.Unmarshal(line, &e) == nil && e.Type != "" {
			fn(&e)
		}
	}
}

// texts is the text of a message's content: a string, or its text blocks.
func texts(raw json.RawMessage) (string, []block) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var bs []block
	_ = json.Unmarshal(raw, &bs)
	var parts []string
	for _, b := range bs {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), bs
}

// Tail returns the last n turns of an omp session file: the person's and
// beekeeper's messages and the agent's words, without thinking, tool calls
// and tool results.
func Tail(path string, n int) ([]claude.Turn, error) {
	buf, whole := claude.ReadWindow(path)
	if buf == nil && !whole {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	var turns []claude.Turn
	eachEntry(buf, func(e *entry) {
		if e.Type != "message" || e.Message == nil {
			return
		}
		role := e.Message.Role
		if role != roleUser && role != roleAssistant {
			return
		}
		if text, _ := texts(e.Message.Content); strings.TrimSpace(text) != "" {
			turns = append(turns, claude.Turn{At: e.Timestamp, Role: role, Text: strings.TrimSpace(text)})
		}
	})
	return turns[max(0, len(turns)-n):], nil
}

// ReadTranscript reads an omp session file's window for the issues and
// repositories it is about and its activity: turns (the person's messages),
// tool calls and failed ones, tokens and cost as omp counted them, and
// the context its last reply had.
func ReadTranscript(path string, now time.Time) (claude.Work, claude.Activity) {
	buf, whole := claude.ReadWindow(path)
	a := claude.Activity{Whole: whole}
	hourStart := now.Add(-time.Hour)
	var said strings.Builder
	var total, hour float64
	var priced bool
	eachEntry(buf, func(e *entry) {
		if e.Type != "message" || e.Message == nil {
			return
		}
		if a.Since.IsZero() {
			a.Since = e.Timestamp
		}
		counts := []*claude.Counts{&a.Total}
		if !e.Timestamp.Before(hourStart) {
			counts = append(counts, &a.LastHour)
		}
		m := e.Message
		text, blocks := texts(m.Content)
		said.WriteString(text)
		said.WriteByte('\n')
		for _, c := range counts {
			switch m.Role {
			case roleUser:
				c.Turns++
			case roleToolResult:
				if m.IsError {
					c.ToolErrors++
				}
			case roleAssistant:
				for _, b := range blocks {
					if b.Type == "toolCall" {
						c.ToolCalls++
						if b.Name == "bash" && invokesGitHub(b.Arguments) {
							c.GitHubCalls++
						}
					}
				}
				if u := m.Usage; u != nil {
					c.Tokens.Input += u.Input
					c.Tokens.Output += u.Output
					c.Tokens.CacheRead += u.CacheRead
					c.Tokens.CacheWrite5m += u.CacheWrite
				}
			}
		}
		if m.Role == roleAssistant {
			if m.Model != "" {
				a.Model = m.Model
			}
			if m.Snapshot != nil && m.Snapshot.PromptTokens > 0 {
				a.Context = m.Snapshot.PromptTokens
			} else if u := m.Usage; u != nil {
				a.Context = u.Input + u.CacheRead + u.CacheWrite + u.Output
			}
			if u := m.Usage; u != nil && u.Cost != nil {
				priced = true
				total += u.Cost.Total
				if len(counts) > 1 {
					hour += u.Cost.Total
				}
			}
		}
	})
	if priced {
		a.Total.CostUSD, a.LastHour.CostUSD = &total, &hour
	}
	return claude.ScanWork(said.String()), a
}

// invokesGitHub says a bash tool call's command runs gh or devctl.
func invokesGitHub(args json.RawMessage) bool {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &in) != nil {
		return false
	}
	return claude.InvokesGitHub(in.Command)
}
