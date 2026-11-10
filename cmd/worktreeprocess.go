package cmd

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// worktreeRoot accepts linked worktrees only, never a main checkout or an
// arbitrary directory a session happened to start in.
func worktreeRoot(dir string) string {
	if !filepath.IsAbs(dir) {
		return ""
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		var fi os.FileInfo
		if root, err := os.OpenRoot(d); err == nil {
			fi, _ = root.Stat(".git")
			_ = root.Close()
		}
		if fi != nil {
			if fi.Mode().IsRegular() {
				return d
			}
			return ""
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

const orphanSessionEnv = "CLAUDE_CODE_SESSION_ID"
const orphanArchiveVerb = "agents.archive"

type worktreeOrphan struct {
	process *proc.Process
	owner   state.Party
	dir     string
}

// orphanIdentity uses the same identity shape as each harness's start and
// roster entry. A hosted harness takes precedence over a launching Claude
// session inherited in its environment.
func orphanIdentity(env map[string]string) state.Party {
	for _, harness := range []struct{ key, prefix string }{
		{codex.EnvAgent, codex.HostPrefix},
		{omp.EnvAgent, omp.HostPrefix},
		{agy.EnvAgent, agy.HostPrefix},
	} {
		if id := env[harness.key]; id != "" {
			return state.Party{HostSession: harness.prefix + id}
		}
	}
	return state.Party{Session: env[orphanSessionEnv]}
}

// worktreeOrphans ties reparented trees to known local starts, by inherited
// session identity or the start's worktree. Every roster entry (including
// done entries awaiting removal) and every live CLI protects its worktree.
// cwd and environ are injectable so tests never inspect unrelated processes.
func worktreeOrphans(st *state.State, tb *proc.Table, sessions []*claude.Session,
	cwd func(int) string, env func(int) (map[string]string, error),
) []worktreeOrphan {
	protected := map[string]bool{}
	protect := func(dir string) {
		if d := worktreeRoot(dir); d != "" {
			protected[d] = true
		}
	}
	rostered := func(p state.Party) bool {
		return slices.ContainsFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(p) })
	}
	for _, s := range sessions {
		protect(s.Cwd)
	}
	for _, s := range st.Starts {
		if rostered(s.Party) {
			protect(s.Dir)
		}
	}
	identities := map[int]state.Party{}
	dirs := map[int]string{}
	for pid := range tb.ByPID {
		dirs[pid] = worktreeRoot(cwd(pid))
		e, err := env(pid)
		if err != nil {
			continue
		}
		identities[pid] = orphanIdentity(e)
		for _, ag := range st.Agents {
			if identities[pid].Is(ag.Party) {
				protected[dirs[pid]] = true
			}
		}
	}
	var out []worktreeOrphan
	for _, pid := range slices.Sorted(maps.Keys(tb.ByPID)) {
		p := tb.ByPID[pid]
		d := dirs[pid]
		if d == "" || protected[d] {
			continue
		}
		// Children of an adopted server need cleanup too, even while their
		// server parent still lives. Only that same worktree is eligible.
		chain := append([]*proc.Process{p}, tb.Ancestors(pid)...)
		if !slices.ContainsFunc(chain, func(q *proc.Process) bool { return dirs[q.PID] == d && tb.Orphaned(q) }) {
			continue
		}
		for _, s := range slices.Concat(st.Starts, st.EndedWorkers) {
			identity := identities[pid]
			identified := identity.Is(s.Party)
			if (identity.Session != "" || identity.HostSession != "") && !identified {
				continue
			}
			if s.Execution != nil || rostered(s.Party) || s.At.IsZero() {
				continue
			}
			// MCP servers may predate manual registration. An exact inherited
			// session identity is evidence even then; directory-only matches
			// must postdate the recorded start.
			if !identified && (p.Start.Before(s.At) || worktreeRoot(s.Dir) != d) {
				continue
			}
			out = append(out, worktreeOrphan{process: p, owner: s.Party, dir: d})
			break
		}
	}
	return out
}

// stopWorktreeOrphans holds the roster lock through signalling: a session
// cannot re-register between the protection check and the last signal.
func (a *app) stopWorktreeOrphans(by state.Party, dry bool) ([]string, error) {
	// Only the local file store serializes the roster with a machine lock.
	// A central store may retry callbacks and describes other hosts too.
	if _, local := a.store.(*state.FileStore); !local {
		return nil, nil
	}
	var lines []string
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		sessions, tb, err := a.sessions()
		if err != nil {
			return nil, err
		}
		orphans := worktreeOrphans(st, tb, sessions, tb.Cwd, tb.Environ)
		var targets []*proc.Process
		for _, o := range orphans {
			targets = append(targets, o.process)
		}
		stopped := map[int]bool{}
		if !dry {
			for _, pid := range proc.TerminateMatching(targets, leftoverGrace, func(p *proc.Process) bool {
				return proc.UID(p.PID) == os.Getuid() && slices.ContainsFunc(orphans, func(o worktreeOrphan) bool {
					return o.process.PID == p.PID && worktreeRoot(proc.Cwd(p.PID)) == o.dir
				})
			}) {
				stopped[pid] = true
			}
		}
		var evs []state.Event
		for _, o := range orphans {
			if !dry && !stopped[o.process.PID] {
				continue
			}
			verb := "stopped"
			if dry {
				verb = "would stop"
			}
			cmd, _ := masked(o.process.Args)
			if cmd == "" {
				cmd = o.process.Comm
			}
			line := fmt.Sprintf("%s orphan process %d %s (age %s) of %q in %s", verb, o.process.PID, cmd, dur(o.process.Elapsed(time.Now())), o.owner.Name, o.dir)
			lines = append(lines, line)
			if !dry {
				evs = append(evs, event(by, orphanArchiveVerb, "%s", line))
			}
		}
		return evs, nil
	})
	return lines, err
}
