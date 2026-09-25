// Package claude finds the Claude Code sessions running on the machine and
// what they are doing, from what is on disk: the CLI processes (their
// environment names the desktop session), the record each CLI keeps of the
// session it runs, the desktop app's session records (title, branch), the
// transcripts (last activity, last words) and the git checkout each works
// in. No MCP call and no GitHub request is needed.
package claude

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Session is one running Claude Code CLI.
type Session struct {
	PID    int    `json:"pid"`
	ID     string `json:"session"`
	HostID string `json:"hostSession,omitempty"`
	// Parent is the session whose tool command started this one.
	Parent     string    `json:"parent,omitempty"`
	Name       string    `json:"name"`
	Cwd        string    `json:"cwd"`
	Repo       string    `json:"repo,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
	LastActive time.Time `json:"lastActive,omitzero"`
	Started    time.Time `json:"started"`
	Commands   []Command `json:"commands,omitempty"`
	MemMiB     int       `json:"memMiB"`
	Permission string    `json:"permissionMode,omitempty"`
	Model      string    `json:"model,omitempty"`
	// Waiting is what the session waits on its person for, as the desktop
	// recorded it after its last turn; "" when it waits on nobody.
	Waiting *Waiting `json:"waiting,omitempty"`
}

// Waiting is a session's turn that ended needing its person: the desktop's
// summary of the turn Turn says what it needs (Action).
type Waiting struct {
	Turn   string `json:"turn"`
	Action string `json:"action"`
}

// Party is the session as the state names it.
func (s *Session) Party() state.Party {
	return state.Party{Session: s.ID, HostSession: s.HostID, Name: s.Name}
}

// Key identifies the session across CLI restarts: the desktop's host id,
// else the session id, else the process.
func (s *Session) Key() string {
	switch {
	case s.HostID != "":
		return s.HostID
	case s.ID != "":
		return s.ID
	}
	return "pid " + strconv.Itoa(s.PID)
}

// Command is a tool command a session runs right now (foreground or in the
// background): the first non-shell process under the tool's shell.
type Command struct {
	PID     int           `json:"pid"`
	Args    string        `json:"args"`
	Elapsed time.Duration `json:"elapsed"`
	// Remaining is set for a `sleep`: when the bounded wait ends.
	Remaining time.Duration `json:"remaining,omitempty"`
}

// Record is the part of a desktop session record beekeeper reads.
type Record struct {
	SessionID      string `json:"sessionId"`
	CLISessionID   string `json:"cliSessionId"`
	Cwd            string `json:"cwd"`
	Branch         string `json:"branch"`
	Title          string `json:"title"`
	IsArchived     bool   `json:"isArchived"`
	LastActivityAt int64  `json:"lastActivityAt"`
	PermissionMode string `json:"permissionMode"`
	Model          string `json:"model"`
	// PriorCLISessionIDs are the CLI sessions the desktop session ran before
	// a restart gave it a new one.
	PriorCLISessionIDs []string `json:"priorCliSessionIds"`
	// PostTurnSummary is the desktop's summary of the turn
	// PostTurnSummaryFor, LastAssistantUUID the session's latest turn.
	PostTurnSummary    *TurnSummary `json:"postTurnSummary"`
	PostTurnSummaryFor string       `json:"postTurnSummaryFor"`
	LastAssistantUUID  string       `json:"lastAssistantUuid"`
}

// TurnSummary is the desktop's summary of a turn: the status it files the
// session under (blocked: it needs its person) and what it needs.
type TurnSummary struct {
	Category    string `json:"status_category"`
	NeedsAction string `json:"needs_action"`
}

// Waiting is what the record says the session waits on its person for:
// its latest turn's summary is blocked on an action; nil otherwise.
func (r *Record) Waiting() *Waiting {
	t := r.PostTurnSummary
	if t == nil || t.Category != "blocked" || strings.TrimSpace(t.NeedsAction) == "" ||
		r.PostTurnSummaryFor == "" || r.PostTurnSummaryFor != r.LastAssistantUUID {
		return nil
	}
	return &Waiting{Turn: r.PostTurnSummaryFor, Action: strings.TrimSpace(t.NeedsAction)}
}

// Discover returns the running sessions, newest first. Every session is a
// CLI process of its own: a desktop session's, a background session's behind
// the daemon (a fresh CLI, or the spare the daemon handed it), a headless
// one's. A CLI that another session's tool command runs belongs to that
// session unless it was started under an id of its own.
func Discover(cfg *config.Config, t *proc.Table, now time.Time) []*Session {
	records := map[int]*cliRecord{}
	for _, p := range t.ByPID {
		if isCLI(p) || isSpare(p) {
			if r, ok := readCLIRecord(cfg, p); ok {
				records[p.PID] = r
			}
		}
	}
	clis := map[int]bool{}
	for _, p := range t.ByPID {
		if runsSession(p, records) && (ownID(p.Args) != "" || !underSession(t, p, records)) {
			clis[p.PID] = true
		}
	}
	byKey := map[string]*Session{}
	for pid := range clis {
		s := newSession(cfg, t, t.ByPID[pid], records[pid], clis, now)
		// A restarted CLI can overlap its predecessor for a moment:
		// the newest process is the session.
		if prev, ok := byKey[s.Key()]; ok && prev.Started.After(s.Started) {
			continue
		}
		byKey[s.Key()] = s
	}
	out := slices.Collect(maps.Values(byKey))
	slices.SortFunc(out, func(a, b *Session) int { return b.Started.Compare(a.Started) })
	return out
}

// helpers are the claude subcommands: processes of the claude binary that
// are no session. daemon, bg-pty-host and bg-spare run the background
// sessions; attach, logs, stop and the rest are clients.
var helpers = map[string]bool{
	"daemon": true, "bg-pty-host": true, "bg-spare": true,
	"agents": true, "attach": true, "auth": true, "auto-mode": true, "doctor": true, "gateway": true,
	"import": true, "install": true, "kill": true, "logs": true, "mcp": true, "plugin": true,
	"plugins": true, "project": true, "respawn": true, "rm": true, "setup-token": true,
	"stop": true, "ultrareview": true, "update": true, "upgrade": true,
}

// isCLI reports whether p is a session's CLI: the claude binary, neither a
// subcommand nor the `claude --bg` launcher, which exits once the daemon
// runs the session.
func isCLI(p *proc.Process) bool {
	if p.Comm != "claude" || len(p.Args) == 0 || subcommand(p) != "" || isSpare(p) {
		return false
	}
	return !slices.Contains(p.Args, "--bg") && !slices.Contains(p.Args, "--background")
}

// isSpare reports whether p is a spare CLI the daemon starts ahead of the
// next background session and hands it when it starts or wakes; it retitles
// itself "claude bg-spare" once it runs.
func isSpare(p *proc.Process) bool {
	return p.Comm == "claude" && (subcommand(p) == "bg-spare" || len(p.Args) > 1 && p.Args[1] == "--bg-spare")
}

// subcommand is a claude process's subcommand, "" for none: argv[1], or the
// second word of an argv[0] the process retitled ("claude bg-pty-host").
func subcommand(p *proc.Process) string {
	if len(p.Args) == 0 {
		return ""
	}
	if title := strings.Fields(p.Args[0]); len(title) > 1 && helpers[title[1]] {
		return title[1]
	}
	if len(p.Args) > 1 && helpers[p.Args[1]] {
		return p.Args[1]
	}
	return ""
}

// runsSession reports whether p runs a session: a CLI, or a spare the daemon
// has handed one. Only the spare's record says which: the spare was started
// before the session.
func runsSession(p *proc.Process, records map[int]*cliRecord) bool {
	return isCLI(p) || isSpare(p) && records[p.PID] != nil
}

// underSession reports whether p runs inside another session (a headless
// `claude -p` a tool command started).
func underSession(t *proc.Table, p *proc.Process, records map[int]*cliRecord) bool {
	return slices.ContainsFunc(t.Ancestors(p.PID), func(a *proc.Process) bool { return runsSession(a, records) })
}

// ownID is the session id a CLI was started under: --session-id, or the
// session --resume continues, which the daemon names by its transcript
// when it wakes a background session (<projects>/<dir>/<id>.jsonl).
func ownID(args []string) string {
	for _, flag := range []string{"--session-id", "--resume", "-r"} {
		if v := argValue(args, flag); v != "" && !strings.HasPrefix(v, "-") {
			if id, ok := strings.CutSuffix(filepath.Base(v), ".jsonl"); ok {
				return id
			}
			return v
		}
	}
	return ""
}

// cliRecord is the part of the record a running CLI keeps of the session it
// runs (<sessions dir>/<pid>.json) that beekeeper reads. It names the session
// the process runs now: a spare's, or a resumed CLI's that went on under a
// new id.
type cliRecord struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	// ProcStart is the process's start time in clock ticks since boot: a
	// record a dead process left behind is not taken for another process
	// under its PID.
	ProcStart string `json:"procStart"`
}

// readCLIRecord reads the record of the session process p runs.
func readCLIRecord(cfg *config.Config, p *proc.Process) (*cliRecord, bool) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(cfg.Claude.SessionsDir, strconv.Itoa(p.PID)+".json")))
	if err != nil {
		return nil, false
	}
	r := &cliRecord{}
	if json.Unmarshal(raw, r) != nil || r.PID != p.PID || r.ProcStart != strconv.FormatInt(p.StartTicks, 10) || r.SessionID == "" {
		return nil, false
	}
	return r, true
}

// RecordName is the name in the record of the running CLI of session id, ""
// for none: the title a `claude --bg` worker was started under, which the
// commands its tools run do not inherit.
func RecordName(cfg *config.Config, t *proc.Table, id string) string {
	for _, p := range t.ByPID {
		if !isCLI(p) && !isSpare(p) {
			continue
		}
		if r, ok := readCLIRecord(cfg, p); ok && r.SessionID == id {
			return r.Name
		}
	}
	return ""
}

// newSession builds the session process p runs: its record names it, else
// its arguments and environment do.
func newSession(cfg *config.Config, t *proc.Table, p *proc.Process, rec *cliRecord, clis map[int]bool, now time.Time) *Session {
	s := &Session{PID: p.PID, Started: p.Start, Cwd: t.Cwd(p.PID), ID: ownID(p.Args)}
	s.Name = argValue(p.Args, "--name")
	if s.Name == "" {
		s.Name = argValue(p.Args, "-n")
	}
	if rec != nil {
		s.ID = rec.SessionID
		if rec.Name != "" {
			s.Name = rec.Name
		}
	}
	if env, err := t.Environ(p.PID); err == nil {
		// Every tool command inherits its session's ids. A CLI whose
		// environment names a session was started by that session and
		// carries its host id too; only a CLI the desktop app started
		// has a host id of its own.
		if parent := env["CLAUDE_CODE_SESSION_ID"]; parent != "" {
			s.Parent = parent
		} else {
			s.HostID = env["CLAUDE_CODE_HOST_SESSION_ID"]
		}
		if s.Name == "" {
			s.Name = env["CLAUDE_CODE_SESSION_NAME"]
		}
	}
	if s.HostID != "" {
		if r, ok := ReadRecord(cfg, s.HostID); ok {
			if r.Title != "" {
				s.Name = r.Title // follows renames; the environment keeps the first title
			}
			if s.ID == "" {
				s.ID = r.CLISessionID
			}
			s.Branch = r.Branch
			s.Permission = r.PermissionMode
			s.Model = r.Model
			s.Waiting = r.Waiting()
		}
	}
	if s.Name == "" {
		s.Name = "pid " + strconv.Itoa(p.PID)
	}
	repo, branch := GitInfo(s.Cwd)
	s.Repo = repo
	if s.Branch == "" {
		s.Branch = branch
	}
	if s.ID != "" {
		if m, _ := filepath.Glob(filepath.Join(cfg.Claude.ProjectsDir, "*", s.ID+".jsonl")); len(m) > 0 {
			s.Transcript = m[0]
			if fi, err := os.Stat(m[0]); err == nil {
				s.LastActive = fi.ModTime()
			}
		}
	}
	tree := ownTree(t, p.PID, clis)
	kib := t.AnonKiB(p.PID)
	for _, d := range tree {
		kib += t.AnonKiB(d.PID)
	}
	s.MemMiB = kib / 1024
	s.Commands = toolCommands(t, tree, now)
	return s
}

// ownTree returns the processes below pid that are its session's: the
// tree of a session one of its tool commands started is that session's.
func ownTree(t *proc.Table, pid int, clis map[int]bool) []*proc.Process {
	var out []*proc.Process
	for _, c := range t.Children(pid) {
		if !clis[c.PID] {
			out = append(out, c)
			out = append(out, ownTree(t, c.PID, clis)...)
		}
	}
	return out
}

// ReadRecord reads the desktop record of a host session id.
func ReadRecord(cfg *config.Config, hostID string) (*Record, bool) {
	if strings.ContainsAny(hostID, `/\*?[`) {
		return nil, false
	}
	m, _ := filepath.Glob(filepath.Join(cfg.Claude.DesktopDir, "*", "*", hostID+".json"))
	if len(m) == 0 {
		return nil, false
	}
	return readRecord(m[0])
}

func readRecord(path string) (*Record, bool) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, false
	}
	r := &Record{}
	if json.Unmarshal(raw, r) != nil {
		return nil, false
	}
	return r, true
}

func recordFiles(cfg *config.Config) []string {
	m, _ := filepath.Glob(filepath.Join(cfg.Claude.DesktopDir, "*", "*", "local_*.json"))
	return m
}

// Titles maps CLI session ids, current and prior, to their desktop titles,
// archived sessions included: the name of a session whose CLI is gone.
func Titles(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, path := range recordFiles(cfg) {
		r, ok := readRecord(path)
		if !ok || r.Title == "" {
			continue
		}
		for _, id := range append(r.PriorCLISessionIDs, r.CLISessionID) {
			if id != "" {
				out[id] = r.Title
			}
		}
	}
	return out
}

// RecentRecords returns the unarchived desktop records active since since.
func RecentRecords(cfg *config.Config, since time.Time) []*Record {
	var out []*Record
	for _, path := range recordFiles(cfg) {
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().Before(since) {
			continue
		}
		if r, ok := readRecord(path); ok && !r.IsArchived {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *Record) int { return int(b.LastActivityAt - a.LastActivityAt) })
	return out
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

var shells = map[string]bool{"zsh": true, "bash": true, "sh": true, "dash": true}

// toolCommands finds the tool shells Claude Code starts (their command line
// sources a shell snapshot) and reports the first non-shell process under
// each: `devctl pr merge …`, `sleep 1500`, `memcap -- …`.
func toolCommands(t *proc.Table, tree []*proc.Process, now time.Time) []Command {
	var out []Command
	var descend func(*proc.Process)
	descend = func(p *proc.Process) {
		for _, c := range t.Children(p.PID) {
			if c.PID == os.Getpid() {
				continue // this beekeeper call
			}
			if shells[c.Comm] {
				descend(c)
				continue
			}
			cmd := Command{PID: c.PID, Args: c.Cmdline(), Elapsed: c.Elapsed(now).Round(time.Second)}
			if c.Comm == "sleep" && len(c.Args) > 1 {
				if d, ok := sleepDuration(c.Args[1:]); ok && d > cmd.Elapsed {
					cmd.Remaining = (d - cmd.Elapsed).Round(time.Second)
				}
			}
			out = append(out, cmd)
		}
	}
	for _, p := range tree {
		if shells[p.Comm] && strings.Contains(p.Cmdline(), "shell-snapshots/snapshot-") && !isToolShellChild(t, p) {
			descend(p)
		}
	}
	return out
}

// isToolShellChild reports whether p is itself under another tool shell (a
// nested shell of a tool command), which descend already covers.
func isToolShellChild(t *proc.Table, p *proc.Process) bool {
	pp := t.ByPID[p.PPID]
	return pp != nil && shells[pp.Comm] && strings.Contains(pp.Cmdline(), "shell-snapshots/snapshot-")
}

// sleepDuration sums sleep's operands ("300", "5m", "1.5h").
func sleepDuration(args []string) (time.Duration, bool) {
	var total time.Duration
	for _, a := range args {
		unit := time.Second
		switch {
		case strings.HasSuffix(a, "s"):
			a = strings.TrimSuffix(a, "s")
		case strings.HasSuffix(a, "m"):
			a, unit = strings.TrimSuffix(a, "m"), time.Minute
		case strings.HasSuffix(a, "h"):
			a, unit = strings.TrimSuffix(a, "h"), time.Hour
		case strings.HasSuffix(a, "d"):
			a, unit = strings.TrimSuffix(a, "d"), 24*time.Hour
		}
		if a == "" || (a[0] < '0' || a[0] > '9') && a[0] != '.' {
			return 0, false // "infinity", "inf", "nan" parse as floats but are no bounded wait
		}
		f, err := strconv.ParseFloat(a, 64)
		if err != nil {
			return 0, false
		}
		total += time.Duration(f * float64(unit))
	}
	return total, true
}

// Resolve finds the session a name or id refers to: an exact session id,
// host id or name (case-insensitive), else a unique name substring.
func Resolve(sessions []*Session, q string) (*Session, error) {
	lq := strings.ToLower(strings.TrimSpace(q))
	if lq == "" {
		return nil, fmt.Errorf("no session named")
	}
	for _, s := range sessions {
		if s.ID == q || s.HostID == q || strings.ToLower(s.Name) == lq || strconv.Itoa(s.PID) == q {
			return s, nil
		}
	}
	var hits []*Session
	for _, s := range sessions {
		if strings.Contains(strings.ToLower(s.Name), lq) {
			hits = append(hits, s)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("no running session matches %q", q)
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = fmt.Sprintf("%q", h.Name)
	}
	return nil, fmt.Errorf("%q matches %d sessions: %s", q, len(hits), strings.Join(names, ", "))
}

// Live reports whether party names one of the running sessions, and which.
func Live(sessions []*Session, p state.Party) (*Session, bool) {
	for _, s := range sessions {
		if p.Is(s.Party()) {
			return s, true
		}
	}
	return nil, false
}

// OwnerOf returns the session a process was started by, from the session
// ids every tool command inherits in its environment: it holds for a command
// whose tool shell has exited and left it to systemd too.
func OwnerOf(sessions []*Session, pid int) (*Session, bool) {
	env, err := proc.Environ(pid)
	if err != nil {
		return nil, false
	}
	p := state.Party{Session: env["CLAUDE_CODE_SESSION_ID"], HostSession: env["CLAUDE_CODE_HOST_SESSION_ID"]}
	if p.Session == "" && p.HostSession == "" {
		return nil, false
	}
	// A child session's commands carry its id and the host id it inherited
	// from its parent: the session id decides.
	if s, ok := Live(sessions, state.Party{Session: p.Session}); ok && p.Session != "" {
		return s, true
	}
	return Live(sessions, p)
}
