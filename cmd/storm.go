package cmd

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// stackAge is how long each copy of a command line runs before it counts
// toward a STACKED line: a burst of short commands is no pile-up.
const stackAge = time.Minute

// stormNames is how many commands and sessions a PROCESS STORM line names.
const stormNames = 3

// forkUsualOver is the span the machine's usual fork rate averages over: a
// machine with thirty sessions forks a hundred processes a second at rest.
const forkUsualOver = 10 * time.Minute

// fresh are the processes of cur that prev had not seen (a new PID, or a
// reused one), less the forks that never ran a program of their own: a child
// with its parent's command line and no CPU time. Go programs (kubectl, helm,
// tsh) start one such child to probe pidfd support, which exits at once; it
// is its parent, not a second kubectl.
func fresh(prev, cur *proc.Table) []*proc.Process {
	if prev == nil || cur == nil {
		return nil
	}
	var out []*proc.Process
	for pid, p := range cur.ByPID {
		if q := prev.ByPID[pid]; q != nil && q.StartTicks == p.StartTicks {
			continue
		}
		if pp := cur.ByPID[p.PPID]; pp != nil && p.CPU == 0 && p.Cmdline() == pp.Cmdline() {
			continue
		}
		out = append(out, p)
	}
	return out
}

// share is one name's part of a set of processes, largest first.
type share struct {
	Name    string
	Percent int
}

func shares(counts map[string]int, total, n int) []share {
	out := make([]share, 0, len(counts))
	for name, c := range counts {
		out = append(out, share{name, c * 100 / total})
	}
	slices.SortFunc(out, func(a, b share) int {
		return cmp.Or(cmp.Compare(counts[b.Name], counts[a.Name]), cmp.Compare(a.Name, b.Name))
	})
	return out[:min(n, len(out))]
}

func sharesLine(ss []share, quote bool) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		name := s.Name
		if quote {
			name = fmt.Sprintf("%q", name)
		}
		parts[i] = fmt.Sprintf("%s %d %%", name, s.Percent)
	}
	return strings.Join(parts, ", ")
}

// stormLine is the PROCESS STORM line: the fork rate and the usual one, and
// the commands and sessions of the processes started since the last sample
// that still ran at this one, by share.
func stormLine(rate, usual float64, started []*proc.Process, t *proc.Table, owners map[int]string) string {
	line := fmt.Sprintf("PROCESS STORM: %.0f forks/s (usual %.0f/s)", rate, usual)
	if len(started) == 0 {
		return line + ", none of them still running at the sample"
	}
	comms, sessions := map[string]int{}, map[string]int{}
	for _, p := range started {
		comms[p.Comm]++
		if s := owner(t, p.PID, owners); s != "" {
			sessions[s]++
		}
	}
	line += ", top: " + sharesLine(shares(comms, len(started), stormNames), false)
	if len(sessions) > 0 {
		line += "; sessions: " + sharesLine(shares(sessions, len(started), stormNames), true)
	}
	return line
}

// owner is the name of the session whose CLI is pid or one of its
// ancestors; "" outside every session.
func owner(t *proc.Table, pid int, owners map[int]string) string {
	if s := owners[pid]; s != "" {
		return s
	}
	for _, a := range t.Ancestors(pid) {
		if s := owners[a.PID]; s != "" {
			return s
		}
	}
	return ""
}

// stack is one command line running more than watch.stackMax times over,
// each copy for over stackAge, from the same place in the process tree.
type stack struct {
	key     string
	Cmd     string
	Count   int
	Oldest  time.Duration
	Parent  string
	Session string
	pids    []int
}

// line is the STACKED line; its head names the command, so its ENDED line
// says which pile-up ended.
func (s stack) line() string {
	l := fmt.Sprintf("STACKED %d × %s: oldest %s", s.Count, s.Cmd, s.Oldest.Round(time.Second))
	if s.Parent != "" {
		l += ", parent " + s.Parent
	}
	if s.Session != "" {
		l += fmt.Sprintf(", session %q", s.Session)
	}
	return l
}

// stacks finds the command lines that run more than max times over. Copies
// count together when their command lines and their ancestors' up to an
// anchor are the same: the nearest session CLI or interactive shell, or the
// third ancestor. So one MCP server under each of thirty CLIs is no stack,
// while seven `beekeeper free` under seven copies of one script are. An
// interactive shell is never a copy, and of a stack whose copies each lead
// a stack below it only the one below is said.
func stacks(t *proc.Table, now time.Time, max int, owners map[int]string) []stack {
	by := map[string]*stack{}
	for _, p := range t.ByPID {
		if len(p.Args) == 0 || interactiveShell(p) || p.Elapsed(now) <= stackAge {
			continue
		}
		key := stackKey(t, p, owners)
		s := by[key]
		if s == nil {
			s = &stack{key: key, Cmd: display(p.Args)}
			by[key] = s
		}
		s.Count++
		s.pids = append(s.pids, p.PID)
		if e := p.Elapsed(now); e > s.Oldest {
			s.Oldest = e
			s.Parent, s.Session = "", owner(t, p.PID, owners)
			if pp := t.ByPID[p.PPID]; pp != nil {
				s.Parent = display(pp.Args)
			}
		}
	}
	parents := map[int]bool{}
	for _, s := range by {
		if s.Count > max {
			for _, pid := range s.pids {
				parents[t.ByPID[pid].PPID] = true
			}
		}
	}
	var out []stack
	for _, key := range slices.Sorted(maps.Keys(by)) {
		s := by[key]
		if s.Count <= max || !slices.ContainsFunc(s.pids, func(pid int) bool { return !parents[pid] }) {
			continue
		}
		out = append(out, *s)
	}
	return out
}

// stackKey is a hash of p's command line and its ancestors' up to the
// anchor, and the anchor's PID: the watch's mark keeps it, and a command
// line may carry a credential.
func stackKey(t *proc.Table, p *proc.Process, owners map[int]string) string {
	parts := []string{p.Cmdline()}
	for i, a := range t.Ancestors(p.PID) {
		if i >= 2 || owners[a.PID] != "" || a.Comm == "claude" || interactiveShell(a) {
			parts = append(parts, fmt.Sprint(a.PID))
			break
		}
		parts = append(parts, a.Cmdline())
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true}

// interactiveShell reports whether p is a shell with no script or command
// to run: a terminal's or a tool's shell, never a copy of a command.
func interactiveShell(p *proc.Process) bool {
	if len(p.Args) == 0 || !shells[strings.TrimPrefix(filepath.Base(p.Args[0]), "-")] {
		return false
	}
	for _, a := range p.Args[1:] {
		if !strings.HasPrefix(a, "-") || a == "-c" {
			return false
		}
	}
	return true
}

// display is a command line safe to print: the program's base name, the
// words before the first flag (its subcommands), and the flags without
// their values. A word after a flag may be the flag's value, a password
// among them, and is left out, as is a word that may carry a credential
// (a URL's user info, a key=value).
func display(args []string) string {
	if len(args) == 0 {
		return ""
	}
	out := []string{filepath.Base(args[0])}
	flags := false
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "-"):
			flags = true
			name, _, _ := strings.Cut(a, "=")
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		case !flags && !strings.ContainsAny(a, ":@="):
			out = append(out, a)
		}
	}
	return truncate(strings.Join(out, " "), 80)
}
