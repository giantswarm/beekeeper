// Package claude finds the Claude Code sessions running on the machine and
// what they are doing, from what is on disk: the CLI processes (their
// environment names the desktop session), the desktop app's session records
// (title, branch), the transcripts (last activity, last words) and the git
// checkout each works in. No MCP call and no GitHub request is needed.
package claude

import (
	"encoding/json"
	"fmt"
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
	PID        int       `json:"pid"`
	ID         string    `json:"session"`
	HostID     string    `json:"hostSession,omitempty"`
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
}

// Party is the session as the state names it.
func (s *Session) Party() state.Party {
	return state.Party{Session: s.ID, HostSession: s.HostID, Name: s.Name}
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
}

// Discover returns the running sessions, newest first.
func Discover(cfg *config.Config, t *proc.Table, now time.Time) []*Session {
	byHost := map[string]*Session{}
	var out []*Session
	for _, p := range t.ByPID {
		if p.Comm != "claude" || underClaude(t, p) {
			continue
		}
		s := newSession(cfg, t, p, now)
		if s.HostID != "" {
			// A restarted CLI can overlap its predecessor for a moment:
			// the newest process is the session.
			if prev, ok := byHost[s.HostID]; ok && prev.Started.After(s.Started) {
				continue
			}
			byHost[s.HostID] = s
			continue
		}
		out = append(out, s)
	}
	for _, s := range byHost {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b *Session) int { return b.Started.Compare(a.Started) })
	return out
}

// underClaude reports whether p runs inside another session (a headless
// `claude -p` started by a tool command): it belongs to that session.
func underClaude(t *proc.Table, p *proc.Process) bool {
	return slices.ContainsFunc(t.Ancestors(p.PID), func(a *proc.Process) bool { return a.Comm == "claude" })
}

func newSession(cfg *config.Config, t *proc.Table, p *proc.Process, now time.Time) *Session {
	s := &Session{PID: p.PID, Started: p.Start, Cwd: proc.Cwd(p.PID)}
	s.ID = argValue(p.Args, "--resume")
	if env, err := proc.Environ(p.PID); err == nil {
		s.HostID = env["CLAUDE_CODE_HOST_SESSION_ID"]
		s.Name = env["CLAUDE_CODE_SESSION_NAME"]
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
	tree := t.Descendants(p.PID)
	kib := proc.AnonKiB(p.PID)
	for _, d := range tree {
		kib += proc.AnonKiB(d.PID)
	}
	s.MemMiB = kib / 1024
	s.Commands = toolCommands(t, tree, now)
	return s
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
	return Live(sessions, p)
}
