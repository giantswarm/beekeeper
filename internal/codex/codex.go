package codex

import (
	"bufio"
	"bytes"
	"cmp"
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

const (
	// maxLine is the scanner buffer ceiling for a rollout JSONL line (16 MiB).
	maxLine = 16 * 1024 * 1024
	// startSlack is how much earlier than its process a rollout file may have
	// been written to and still match a freshly started process.
	startSlack = 5 * time.Second

	evSessionMeta   = "session_meta"
	evTurnContext   = "turn_context"
	evResponseItem  = "response_item"
	evEventMsg      = "event_msg"
	evThreadStarted = "thread.started"
	evTurnStarted   = "turn.started"
	evTurnCompleted = "turn.completed"
	evItemCompleted = "item.completed"

	itemMessage        = "message"
	itemAgentMessage   = "agent_message"
	itemFunctionCall   = "function_call"
	itemCustomToolCall = "custom_tool_call"
	itemLocalShellCall = "local_shell_call"

	roleUser      = "user"
	roleAssistant = "assistant"

	keyCommand = "command"
	toolBash   = "Bash"
)

// StateEnded is Session.State when a Codex session is no longer running.
const StateEnded = "ended"

// sessionDirs returns the deduplicated list of directories holding Codex
// rollout files: the isolated worker CODEX_HOME sessions directory under
// stateDir and the configured sessionsDir.
func sessionDirs(stateDir, sessionsDir string, extra ...string) []string {
	var out []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	if stateDir != "" {
		add(HomeSessionsDir(stateDir))
	}
	add(sessionsDir)
	for _, d := range extra {
		add(d)
	}
	return out
}

// Discover finds running Codex sessions from the process table and matches
// them to their rollout transcripts. It inspects both beekeeper's own
// `codex-run` unit runners and standalone `codex` CLI processes (ignoring
// `codex app-server`), and only sets HostID (`codex_<id>`) when the process
// carries BEEKEEPER_CODEX_AGENT in its environment.
func Discover(stateDir, sessionsDir string, t *proc.Table, now time.Time) []*claude.Session {
	if t == nil {
		return nil
	}
	runners, standalone, childCodex, extraDirs := codexProcesses(t)
	files := sessionFiles(sessionDirs(stateDir, sessionsDir, extraDirs...), time.Time{})
	byID := make(map[string]file, len(files))
	for _, f := range files {
		if f.id != "" {
			byID[f.id] = f
		}
	}
	taken := map[string]bool{}
	var out []*claude.Session

	// 1. Beekeeper-started codex-run processes (live for the lifetime of the unit,
	// both during a turn and while idle waiting on the FIFO inbox).
	for _, r := range runners {
		env, _ := proc.Environ(r.PID)
		agentID := strings.TrimSpace(env[EnvAgent])
		name := strings.TrimSpace(env[EnvName])
		cwd := proc.Cwd(r.PID)
		s := &claude.Session{
			PID:     r.PID,
			Cwd:     cwd,
			Harness: Harness,
			Name:    name,
			Model:   runnerModel(r.Args),
		}
		if agentID != "" {
			s.HostID = HostPrefix + agentID
		}
		inbox := runnerInbox(r.Args, stateDir, agentID)
		threadID := ReadThread(stateDir, inbox)
		if threadID == "" {
			if c := childCodex[r.PID]; c != nil {
				threadID = resumeThreadArg(c.Args)
			}
		}
		var matched file
		var hasFile bool
		if threadID != "" {
			if f, ok := byID[threadID]; ok {
				matched, hasFile = f, true
			}
		}
		if !hasFile {
			for _, f := range files {
				if !taken[f.path] && f.cwd == cwd && !f.mod.Before(r.Start.Add(-startSlack)) {
					matched, hasFile = f, true
					break
				}
			}
		}
		if hasFile {
			taken[matched.path] = true
			fillFromFile(s, matched.path, true)
		} else if logPath := runnerLog(r.Args, stateDir, agentID); logPath != "" {
			if _, err := os.Stat(logPath); err == nil {
				fillFromFile(s, logPath, true)
			}
		}
		if s.ID == "" && threadID != "" {
			s.ID = threadID
		}
		if s.ID == "" && agentID != "" {
			s.ID = agentID
		}
		if name != "" {
			s.Name = name
		} else if s.Name == "" {
			s.Name = "codex in " + filepath.Base(s.Cwd)
		}
		s.Repo, s.Branch = claude.GitInfo(s.Cwd)
		s.MemMiB, s.Commands = claude.ProcessTree(t, r.PID, now)
		if c := childCodex[r.PID]; c != nil {
			_, cmds := claude.ProcessTree(t, c.PID, now)
			if len(cmds) > 0 {
				s.Commands = cmds
			}
			s.State = StateBusy
		} else if st := ReadState(inbox); st == StateBusy {
			s.State = StateBusy
		} else {
			s.State = StateIdle
		}
		out = append(out, s)
	}

	// 2. Standalone codex processes (user-started interactive/exec sessions, or
	// tests that place a codex process directly in the process table).
	var remainingFiles []file
	for _, f := range files {
		if !taken[f.path] {
			remainingFiles = append(remainingFiles, f)
		}
	}
	assigned := assign(remainingFiles, standalone, proc.Cwd)
	for _, p := range standalone {
		cwd := proc.Cwd(p.PID)
		s := &claude.Session{
			PID:     p.PID,
			Cwd:     cwd,
			Harness: Harness,
			Model:   flagValue(p.Args, "-m", "--model"),
		}
		var agentID, envName string
		if env, err := proc.Environ(p.PID); err == nil {
			agentID = strings.TrimSpace(env[EnvAgent])
			envName = strings.TrimSpace(env[EnvName])
			if agentID != "" {
				s.HostID = HostPrefix + agentID
			}
			s.Name = envName
		}
		var matched file
		var hasFile bool
		if tid := resumeThreadArg(p.Args); tid != "" {
			if f, ok := byID[tid]; ok && !taken[f.path] {
				matched, hasFile = f, true
			}
		}
		if !hasFile && agentID != "" {
			if tid := ReadThread(stateDir, InboxPath(stateDir, agentID)); tid != "" {
				if f, ok := byID[tid]; ok && !taken[f.path] {
					matched, hasFile = f, true
				}
			}
		}
		if !hasFile {
			matched, hasFile = assigned[p.PID]
		}
		if hasFile {
			taken[matched.path] = true
			fillFromFile(s, matched.path, true)
		} else if agentID != "" {
			if logPath := LogPath(stateDir, agentID); logPath != "" {
				if _, err := os.Stat(logPath); err == nil {
					fillFromFile(s, logPath, true)
				}
			}
		}
		if envName != "" {
			s.Name = envName
		} else if s.Name == "" {
			s.Name = "codex in " + filepath.Base(s.Cwd)
		}
		if agentID != "" {
			if st := ReadState(InboxPath(stateDir, agentID)); st == StateBusy || st == StateIdle {
				s.State = st
			}
		}
		if s.State == "" {
			s.State = StateIdle
		}
		s.Repo, s.Branch = claude.GitInfo(s.Cwd)
		s.MemMiB, s.Commands = claude.ProcessTree(t, p.PID, now)
		out = append(out, s)
	}
	return out
}

// Ended returns the Codex sessions written to since `since` that no running
// session runs, most recently active first.
func Ended(stateDir, sessionsDir string, running []*claude.Session, since time.Time) []*claude.Session {
	var out []*claude.Session
	for _, f := range sessionFiles(sessionDirs(stateDir, sessionsDir), since) {
		if slices.ContainsFunc(running, func(s *claude.Session) bool {
			return s.Transcript == f.path || (f.id != "" && s.ID == f.id)
		}) {
			continue
		}
		s := &claude.Session{Cwd: f.cwd, Harness: Harness}
		fillFromFile(s, f.path, false)
		if stateDir != "" && s.ID != "" {
			if b, err := os.ReadFile(ThreadLinkPath(stateDir, s.ID)); err == nil {
				inbox := strings.TrimSpace(string(b))
				if id := strings.TrimSuffix(filepath.Base(inbox), ".in"); id != "" {
					s.HostID = HostPrefix + id
				}
			}
		}
		if s.Name == "" {
			s.Name = "codex in " + filepath.Base(s.Cwd)
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *claude.Session) int { return b.LastActive.Compare(a.LastActive) })
	return out
}

func codexProcesses(t *proc.Table) (runners, standalone []*proc.Process, childCodex map[int]*proc.Process, extraDirs []string) {
	childCodex = map[int]*proc.Process{}
	runnerSet := map[int]bool{}
	codexSet := map[int]bool{}
	for _, p := range t.ByPID {
		if isRunnerProc(p) {
			runnerSet[p.PID] = true
			runners = append(runners, p)
			if env, err := proc.Environ(p.PID); err == nil {
				if ch := strings.TrimSpace(env["CODEX_HOME"]); ch != "" {
					extraDirs = append(extraDirs, filepath.Join(ch, "sessions"))
				}
			}
		} else if isCodexProc(p) {
			codexSet[p.PID] = true
			if env, err := proc.Environ(p.PID); err == nil {
				if ch := strings.TrimSpace(env["CODEX_HOME"]); ch != "" {
					extraDirs = append(extraDirs, filepath.Join(ch, "sessions"))
				}
			}
		}
	}
	for _, p := range t.ByPID {
		if !codexSet[p.PID] {
			continue
		}
		var underRunner int
		var underOtherCodex bool
		for _, a := range t.Ancestors(p.PID) {
			if runnerSet[a.PID] {
				underRunner = a.PID
				break
			}
			if codexSet[a.PID] {
				underOtherCodex = true
				break
			}
		}
		if underRunner != 0 {
			childCodex[underRunner] = p
			continue
		}
		if !underOtherCodex {
			standalone = append(standalone, p)
		}
	}
	slices.SortFunc(runners, func(a, b *proc.Process) int { return a.PID - b.PID })
	slices.SortFunc(standalone, func(a, b *proc.Process) int { return a.PID - b.PID })
	return runners, standalone, childCodex, extraDirs
}

func isRunnerProc(p *proc.Process) bool {
	if len(p.Args) < 4 {
		return false
	}
	return p.Args[1] == "codex-run"
}

func isCodexProc(p *proc.Process) bool {
	if p.Comm != Comm && (len(p.Args) == 0 || filepath.Base(p.Args[0]) != Comm) {
		return false
	}
	// Never treat `codex app-server` as an agent session.
	if slices.Contains(p.Args, "app-server") {
		return false
	}
	return true
}

func runnerInbox(args []string, stateDir, agentID string) string {
	if len(args) >= 3 && strings.HasSuffix(args[2], ".in") {
		return args[2]
	}
	if stateDir != "" && agentID != "" {
		return InboxPath(stateDir, agentID)
	}
	return ""
}

func runnerLog(args []string, stateDir, agentID string) string {
	if len(args) >= 4 && strings.HasSuffix(args[3], ".jsonl") {
		return args[3]
	}
	if stateDir != "" && agentID != "" {
		return LogPath(stateDir, agentID)
	}
	return ""
}

func runnerModel(args []string) string {
	if len(args) >= 6 {
		return args[5]
	}
	return ""
}

func flagValue(args []string, flags ...string) string {
	for i, a := range args {
		for _, fl := range flags {
			if v, ok := strings.CutPrefix(a, fl+"="); ok {
				return v
			}
			if a == fl && i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return ""
}

func resumeThreadArg(args []string) string {
	idx := slices.Index(args, "resume")
	if idx < 0 {
		return ""
	}
	for i := idx + 1; i < len(args); i++ {
		a := args[i]
		if a == "-" {
			continue
		}
		if a == "-m" || a == "--model" || a == "-c" || a == "--config" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

type file struct {
	path, id, cwd string
	created, mod  time.Time
}

func sessionFiles(dirs []string, since time.Time) []file {
	seen := map[string]bool{}
	var out []file
	for _, dir := range dirs {
		var paths []string
		for _, pat := range []string{
			filepath.Join(dir, "*", "*", "*", "rollout-*.jsonl"),
			filepath.Join(dir, "*", "rollout-*.jsonl"),
			filepath.Join(dir, "rollout-*.jsonl"),
			filepath.Join(dir, "*.jsonl"),
		} {
			matches, _ := filepath.Glob(pat)
			paths = append(paths, matches...)
		}
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			fi, err := os.Stat(p)
			if err != nil || (!since.IsZero() && fi.ModTime().Before(since)) {
				continue
			}
			h, err := readHead(p)
			if err != nil || h.id == "" {
				continue
			}
			created := h.created
			if created.IsZero() {
				created = fi.ModTime()
			}
			out = append(out, file{
				path:    p,
				id:      h.id,
				cwd:     h.cwd,
				created: created,
				mod:     fi.ModTime(),
			})
		}
	}
	slices.SortFunc(out, func(a, b file) int { return b.mod.Compare(a.mod) })
	return out
}

func assign(files []file, procs []*proc.Process, cwd func(pid int) string) map[int]file {
	taken := map[string]bool{}
	out := map[int]file{}
	pick := func(p *proc.Process, ok func(f file, since time.Time) bool, better func(f, best file) bool) {
		since := p.Start.Add(-startSlack)
		var best *file
		for i, f := range files {
			if !taken[f.path] && ok(f, since) && (best == nil || better(f, *best)) {
				best = &files[i]
			}
		}
		if best != nil {
			out[p.PID], taken[best.path] = *best, true
		}
	}
	byStart := slices.Clone(procs)
	slices.SortFunc(byStart, func(a, b *proc.Process) int { return b.Start.Compare(a.Start) })
	for _, p := range byStart {
		dir := cwd(p.PID)
		pick(p,
			func(f file, since time.Time) bool { return f.cwd == dir && !f.created.Before(since) },
			func(f, best file) bool { return f.created.Before(best.created) })
	}
	for _, p := range byStart {
		if _, ok := out[p.PID]; !ok {
			dir := cwd(p.PID)
			pick(p,
				func(f file, since time.Time) bool { return f.cwd == dir && !f.mod.Before(since) },
				func(f, best file) bool { return f.mod.After(best.mod) })
		}
	}
	return out
}

type head struct {
	id, cwd string
	created time.Time
}

type rolloutLine struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	ThreadID  string          `json:"thread_id"`
	Payload   json.RawMessage `json:"payload"`
	Item      json.RawMessage `json:"item"`
	Usage     *tokenUsage     `json:"usage"`
}

type sessionMetaPayload struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Cwd       string    `json:"cwd"`
	Git       *struct {
		Branch string `json:"branch"`
	} `json:"git"`
}

type turnContextPayload struct {
	Cwd   string `json:"cwd"`
	Model string `json:"model"`
}

type responseItemPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
	Content   []contentPart   `json:"content"`
	Action    *struct {
		Command []string `json:"command"`
	} `json:"action"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type eventMsgPayload struct {
	Type string `json:"type"`
	Info *struct {
		TotalTokenUsage tokenUsage `json:"total_token_usage"`
		LastTokenUsage  tokenUsage `json:"last_token_usage"`
	} `json:"info"`
}

type tokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type streamItem struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Text     string `json:"text"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
}

func readHead(path string) (head, error) {
	f, err := os.Open(path) //nolint:gosec // rollout file under configured sessions directory
	if err != nil {
		return head{}, err
	}
	defer func() { _ = f.Close() }()
	sc := newScanner(f)
	var h head
	for i := 0; i < 16 && sc.Scan(); i++ {
		var l rolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case evSessionMeta:
			var m sessionMetaPayload
			if json.Unmarshal(l.Payload, &m) == nil {
				h.id = cmp.Or(h.id, m.ID)
				h.cwd = cmp.Or(h.cwd, m.Cwd)
				if !m.Timestamp.IsZero() {
					h.created = m.Timestamp
				} else if !l.Timestamp.IsZero() {
					h.created = l.Timestamp
				}
			}
		case evTurnContext:
			var tc turnContextPayload
			if json.Unmarshal(l.Payload, &tc) == nil {
				h.cwd = cmp.Or(h.cwd, tc.Cwd)
			}
		case evThreadStarted:
			h.id = cmp.Or(h.id, l.ThreadID)
		}
		if h.id != "" && h.cwd != "" {
			return h, nil
		}
	}
	return h, sc.Err()
}

func fillFromFile(s *claude.Session, path string, running bool) {
	s.Transcript = path
	if fi, err := os.Stat(path); err == nil {
		s.LastActive = fi.ModTime()
	}
	f, err := os.Open(path) //nolint:gosec // rollout file under configured sessions directory
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var busy bool
	var pendingTools int
	var firstUserPrompt string
	sc := newScanner(f)
	for sc.Scan() {
		var l rolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case evSessionMeta:
			var m sessionMetaPayload
			if json.Unmarshal(l.Payload, &m) == nil {
				s.ID = cmp.Or(s.ID, m.ID)
				s.Cwd = cmp.Or(s.Cwd, m.Cwd)
				if m.Git != nil && s.Branch == "" {
					s.Branch = m.Git.Branch
				}
			}
		case evTurnContext:
			var tc turnContextPayload
			if json.Unmarshal(l.Payload, &tc) == nil {
				s.Cwd = cmp.Or(s.Cwd, tc.Cwd)
				if tc.Model != "" {
					s.Model = tc.Model
				}
			}
		case evResponseItem:
			var item responseItemPayload
			if json.Unmarshal(l.Payload, &item) != nil {
				continue
			}
			switch item.Type {
			case itemMessage:
				switch item.Role {
				case roleUser:
					txt := messageText(item.Content)
					if isUserPrompt(txt) {
						if firstUserPrompt == "" {
							firstUserPrompt = extractTitlePrompt(txt)
						}
						busy = true
						pendingTools = 0
					}
				case roleAssistant:
					if pendingTools == 0 {
						busy = false
					}
				}
			case itemFunctionCall, itemCustomToolCall, itemLocalShellCall:
				pendingTools++
				busy = true
			case "function_call_output", "custom_tool_call_output":
				if pendingTools > 0 {
					pendingTools--
				}
				if pendingTools == 0 {
					busy = false
				}
			}
		case evEventMsg:
			var em eventMsgPayload
			if json.Unmarshal(l.Payload, &em) != nil {
				continue
			}
			switch em.Type {
			case "task_started", "turn_started":
				busy = true
			case "task_complete", "turn_complete", "turn_aborted":
				busy = false
				pendingTools = 0
			}
		case evThreadStarted:
			s.ID = cmp.Or(s.ID, l.ThreadID)
		case evTurnStarted:
			busy = true
		case evTurnCompleted, "turn.failed":
			busy = false
			pendingTools = 0
		}
	}
	if s.Name == "" && firstUserPrompt != "" {
		s.Name = promptTitle(firstUserPrompt)
	}
	switch {
	case !running:
		s.State = StateEnded
	case busy:
		s.State = StateBusy
	default:
		s.State = StateIdle
	}
}

// Tail returns the last n turns of a Codex rollout file: user prompts and
// assistant replies, without developer instructions, tool calls or tool results.
func Tail(path string, n int) ([]claude.Turn, error) { return tail(path, n, false) }

// Follow is Tail with the agent's tool calls among the turns, as claude.Follow
// returns them.
func Follow(path string, n int) ([]claude.Turn, error) { return tail(path, n, true) }

func tail(path string, n int, tools bool) ([]claude.Turn, error) {
	f, err := os.Open(path) //nolint:gosec // rollout file under configured sessions directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var turns []claude.Turn
	sc := newScanner(f)
	for sc.Scan() {
		var l rolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case evResponseItem:
			var item responseItemPayload
			if json.Unmarshal(l.Payload, &item) != nil {
				continue
			}
			switch item.Type {
			case itemMessage:
				rawTxt := messageText(item.Content)
				switch item.Role {
				case roleUser:
					if isUserPrompt(rawTxt) {
						turns = append(turns, claude.Turn{
							At:   l.Timestamp,
							Role: roleUser,
							Text: strings.TrimSpace(extractTitlePrompt(rawTxt)),
						})
					}
				case roleAssistant:
					if strings.TrimSpace(rawTxt) != "" {
						turns = append(turns, claude.Turn{
							At:   l.Timestamp,
							Role: roleAssistant,
							Text: strings.TrimSpace(rawTxt),
						})
					}
				}
			case itemFunctionCall:
				if tools {
					tName, argsJSON := normalizeToolArgs(item.Name, item.Arguments)
					turns = append(turns, claude.Turn{
						At:   l.Timestamp,
						Role: claude.RoleTool,
						Text: claude.ToolGist(tName, argsJSON),
					})
				}
			case itemCustomToolCall:
				if tools {
					tName, argsJSON := normalizeCustomToolArgs(item.Name, item.Input)
					turns = append(turns, claude.Turn{
						At:   l.Timestamp,
						Role: claude.RoleTool,
						Text: claude.ToolGist(tName, argsJSON),
					})
				}
			case itemLocalShellCall:
				if tools && item.Action != nil {
					cmdStr := strings.Join(item.Action.Command, " ")
					argsJSON, _ := json.Marshal(map[string]string{keyCommand: cmdStr})
					turns = append(turns, claude.Turn{
						At:   l.Timestamp,
						Role: claude.RoleTool,
						Text: claude.ToolGist(toolBash, argsJSON),
					})
				}
			}
		case evItemCompleted:
			var si streamItem
			if json.Unmarshal(l.Item, &si) != nil {
				continue
			}
			switch si.Type {
			case itemAgentMessage:
				if strings.TrimSpace(si.Text) != "" {
					turns = append(turns, claude.Turn{
						At:   l.Timestamp,
						Role: roleAssistant,
						Text: strings.TrimSpace(si.Text),
					})
				}
			case "command_execution":
				if tools && si.Command != "" {
					argsJSON, _ := json.Marshal(map[string]string{keyCommand: si.Command})
					turns = append(turns, claude.Turn{
						At:   l.Timestamp,
						Role: claude.RoleTool,
						Text: claude.ToolGist(toolBash, argsJSON),
					})
				}
			}
		}
	}
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
	}
	return turns, nil
}

// ReadTranscript reads a Codex rollout file into the Work and Activity records
// that board, metrics, sessions and capacity use.
func ReadTranscript(path string, now time.Time) (claude.Work, claude.Activity) {
	buf, whole := claude.ReadWindow(path)
	a := claude.Activity{Whole: whole}
	if len(buf) == 0 {
		return claude.Work{}, a
	}
	hourStart := now.Add(-time.Hour)
	acts := claude.NewActs()
	var cwd string
	var prevTotal tokenUsage
	seededTotal := whole

	sc := newScanner(bytes.NewReader(buf))
	for sc.Scan() {
		var l rolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if a.Since.IsZero() && !l.Timestamp.IsZero() {
			a.Since = l.Timestamp
		}
		counts := []*claude.Counts{&a.Total}
		if !l.Timestamp.IsZero() && !l.Timestamp.Before(hourStart) {
			counts = append(counts, &a.LastHour)
		}
		switch l.Type {
		case evSessionMeta:
			var m sessionMetaPayload
			if json.Unmarshal(l.Payload, &m) == nil {
				if cwd == "" {
					cwd = m.Cwd
				}
				if a.Since.IsZero() && !m.Timestamp.IsZero() {
					a.Since = m.Timestamp
				}
			}
		case evTurnContext:
			var tc turnContextPayload
			if json.Unmarshal(l.Payload, &tc) == nil {
				if cwd == "" {
					cwd = tc.Cwd
				}
				if tc.Model != "" {
					a.Model = tc.Model
				}
			}
		case evResponseItem:
			var item responseItemPayload
			if json.Unmarshal(l.Payload, &item) != nil {
				continue
			}
			switch item.Type {
			case itemMessage:
				if item.Role == roleUser && isUserPrompt(messageText(item.Content)) {
					for _, c := range counts {
						c.Turns++
					}
				}
			case itemFunctionCall:
				tName, argsJSON := normalizeToolArgs(item.Name, item.Arguments)
				acts.Call(item.CallID, tName, argsJSON, cwd)
				isGH := tName == toolBash && invokesGitHub(argsJSON)
				for _, c := range counts {
					c.ToolCalls++
					if isGH {
						c.GitHubCalls++
					}
				}
			case itemCustomToolCall:
				tName, argsJSON := normalizeCustomToolArgs(item.Name, item.Input)
				acts.Call(item.CallID, tName, argsJSON, cwd)
				for _, c := range counts {
					c.ToolCalls++
				}
			case itemLocalShellCall:
				cmdStr := ""
				if item.Action != nil {
					cmdStr = strings.Join(item.Action.Command, " ")
				}
				argsJSON, _ := json.Marshal(map[string]string{keyCommand: cmdStr})
				acts.Call(item.CallID, toolBash, argsJSON, cwd)
				isGH := claude.InvokesGitHub(cmdStr)
				for _, c := range counts {
					c.ToolCalls++
					if isGH {
						c.GitHubCalls++
					}
				}
			case "function_call_output", "custom_tool_call_output":
				outText := outputString(item.Output)
				acts.Result(item.CallID, outText)
				if isOutputError(item.Output) {
					for _, c := range counts {
						c.ToolErrors++
					}
				}
			}
		case evEventMsg:
			var em eventMsgPayload
			if json.Unmarshal(l.Payload, &em) != nil || em.Type != "token_count" || em.Info == nil {
				continue
			}
			var (
				tok       claude.Tokens
				nextTotal tokenUsage
				ok        bool
			)
			if !seededTotal {
				seededTotal = true
				prevTotal = em.Info.TotalTokenUsage
				u := em.Info.LastTokenUsage
				if u.TotalTokens > 0 || u.InputTokens > 0 || u.OutputTokens > 0 {
					tok, nextTotal, ok = usageToClaudeTokens(u), prevTotal, true
				}
			} else {
				tok, nextTotal, ok = stepTokens(em.Info.TotalTokenUsage, em.Info.LastTokenUsage, prevTotal)
			}
			if !ok {
				continue
			}
			prevTotal = nextTotal
			for _, c := range counts {
				c.Tokens.Input += tok.Input
				c.Tokens.Output += tok.Output
				c.Tokens.CacheRead += tok.CacheRead
				c.Tokens.CacheWrite5m += tok.CacheWrite5m
			}
			u := em.Info.LastTokenUsage
			if ctx := u.InputTokens + u.OutputTokens; ctx > 0 {
				a.Context = ctx
			} else if u.TotalTokens > 0 {
				a.Context = u.TotalTokens
			}
		case evTurnStarted:
			for _, c := range counts {
				c.Turns++
			}
		case evItemCompleted:
			var si streamItem
			if json.Unmarshal(l.Item, &si) == nil && si.Type == "command_execution" {
				argsJSON, _ := json.Marshal(map[string]string{keyCommand: si.Command})
				acts.Call(si.ID, toolBash, argsJSON, cwd)
				isGH := claude.InvokesGitHub(si.Command)
				isErr := si.ExitCode != nil && *si.ExitCode != 0
				for _, c := range counts {
					c.ToolCalls++
					if isGH {
						c.GitHubCalls++
					}
					if isErr {
						c.ToolErrors++
					}
				}
			}
		case evTurnCompleted:
			if l.Usage != nil {
				tok := usageToClaudeTokens(*l.Usage)
				for _, c := range counts {
					c.Tokens.Input += tok.Input
					c.Tokens.Output += tok.Output
					c.Tokens.CacheRead += tok.CacheRead
					c.Tokens.CacheWrite5m += tok.CacheWrite5m
				}
				if ctx := l.Usage.InputTokens + l.Usage.OutputTokens; ctx > 0 {
					a.Context = ctx
				}
			}
		}
	}
	return acts.Work(string(buf)), a
}

func messageText(parts []contentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if (p.Type == "input_text" || p.Type == "output_text" || p.Type == "text") && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func isUserPrompt(txt string) bool {
	s := strings.TrimSpace(txt)
	if s == "" {
		return false
	}
	for _, prefix := range []string{
		"# AGENTS.md instructions",
		"<INSTRUCTIONS>",
		"<environment_context>",
		"<permissions instructions>",
		"<collaboration_mode>",
		"<system-reminder>",
	} {
		if strings.HasPrefix(s, prefix) {
			return false
		}
	}
	return true
}

func extractTitlePrompt(txt string) string {
	if _, after, ok := strings.Cut(txt, "\nTask:\n"); ok {
		if s := strings.TrimSpace(after); s != "" {
			return s
		}
	}
	return strings.TrimSpace(txt)
}

func promptTitle(txt string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(txt), "\n")
	line = strings.TrimSpace(line)
	const maxLen = 60
	if len(line) > maxLen {
		line = line[:maxLen]
	}
	return line
}

func normalizeToolArgs(name, argsStr string) (string, json.RawMessage) {
	var args map[string]any
	_ = json.Unmarshal([]byte(argsStr), &args)
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	switch name {
	case "exec_command", "shell_command", "shell", toolBash:
		cmd := str("cmd", keyCommand)
		if cmd == "" {
			if arr, ok := args[keyCommand].([]any); ok {
				parts := make([]string, 0, len(arr))
				for _, x := range arr {
					if s, ok := x.(string); ok {
						parts = append(parts, s)
					}
				}
				cmd = strings.Join(parts, " ")
			}
		}
		b, _ := json.Marshal(map[string]string{keyCommand: cmd})
		return toolBash, b
	default:
		if len(argsStr) > 0 {
			return name, json.RawMessage(argsStr)
		}
		return name, nil
	}
}

func normalizeCustomToolArgs(name, input string) (string, json.RawMessage) {
	if name == "apply_patch" {
		for _, line := range strings.Split(input, "\n") {
			for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
				if p, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok && p != "" {
					b, _ := json.Marshal(map[string]string{"file_path": strings.TrimSpace(p)})
					return "Edit", b
				}
			}
		}
		return "Edit", nil
	}
	b, _ := json.Marshal(map[string]string{"input": strings.Join(strings.Fields(input), " ")})
	return name, b
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

func outputString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func isOutputError(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return hasNonZeroExit(s)
	}
	var obj struct {
		ExitCode *int   `json:"exit_code"`
		Error    string `json:"error"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.ExitCode != nil && *obj.ExitCode != 0 {
			return true
		}
		if obj.Error != "" {
			return true
		}
	}
	return false
}

func hasNonZeroExit(s string) bool {
	if _, after, ok := strings.Cut(s, "Process exited with code "); ok {
		code, _, _ := strings.Cut(strings.TrimSpace(after), "\n")
		code = strings.TrimSpace(code)
		return code != "" && code != "0"
	}
	return false
}

func stepTokens(total, last, prevTotal tokenUsage) (claude.Tokens, tokenUsage, bool) {
	if total.TotalTokens > 0 || total.InputTokens > 0 || total.OutputTokens > 0 {
		if total.InputTokens <= prevTotal.InputTokens && total.OutputTokens <= prevTotal.OutputTokens && total.TotalTokens <= prevTotal.TotalTokens {
			return claude.Tokens{}, prevTotal, false
		}
		delta := tokenUsage{
			InputTokens:           max(total.InputTokens-prevTotal.InputTokens, 0),
			CachedInputTokens:     max(total.CachedInputTokens-prevTotal.CachedInputTokens, 0),
			CacheWriteInputTokens: max(total.CacheWriteInputTokens-prevTotal.CacheWriteInputTokens, 0),
			OutputTokens:          max(total.OutputTokens-prevTotal.OutputTokens, 0),
			ReasoningOutputTokens: max(total.ReasoningOutputTokens-prevTotal.ReasoningOutputTokens, 0),
			TotalTokens:           max(total.TotalTokens-prevTotal.TotalTokens, 0),
		}
		return usageToClaudeTokens(delta), total, true
	}
	if last.TotalTokens > 0 || last.InputTokens > 0 || last.OutputTokens > 0 {
		return usageToClaudeTokens(last), prevTotal, true
	}
	return claude.Tokens{}, prevTotal, false
}

func usageToClaudeTokens(u tokenUsage) claude.Tokens {
	// OpenAI reports cached_input_tokens as a subset of input_tokens
	// (total_tokens == input_tokens + output_tokens).
	uncached := u.InputTokens
	if u.InputTokens >= u.CachedInputTokens+u.CacheWriteInputTokens {
		uncached = u.InputTokens - u.CachedInputTokens - u.CacheWriteInputTokens
	}
	return claude.Tokens{
		Input:        max(uncached, 0),
		Output:       max(u.OutputTokens, 0),
		CacheRead:    max(u.CachedInputTokens, 0),
		CacheWrite5m: max(u.CacheWriteInputTokens, 0),
	}
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	return sc
}
