package free

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// minKiB is the size below which a dir is not worth a line of its own; the
// section's total counts it.
const minKiB = 1024

// sessionID matches a Claude Code session id, the name of a session's dir.
var sessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sessionDirs frees the scratch of dead sessions: the session dirs under
// Scratch untouched (transcript and files) for Stale whose session is dead.
// A session lives on while its CLI runs (under this id or, after a restart,
// a newer one of its desktop session), while the desk counts on it (a roster
// agent with its task, a role's holder) and while the git worktree it works
// in remains; a process working or holding a file inside keeps its dir too.
// A live session's scratch is kept however idle it is: the session still
// reads it when it resumes work.
func (r *Run) sessionDirs() {
	r.head(fmt.Sprintf("dead sessions' dirs in %s (tmpfs; a live session's are kept however idle)", r.Scratch))
	if !r.tableRead() {
		return
	}
	if r.Desk == nil {
		r.say("  every dir kept: beekeeper's state cannot be read, so the sessions the roster and the roles count on are unknown")
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(r.Scratch, "*", "*"))
	n := 0
	for _, d := range dirs {
		sid, slug := filepath.Base(d), filepath.Base(filepath.Dir(d))
		if !sessionID.MatchString(sid) || !isDir(filepath.Join(d, "scratchpad")) && !isDir(filepath.Join(d, "tasks")) {
			continue
		}
		sc := r.scan(d)
		transcript := filepath.Join(r.Projects, slug, sid+".jsonl")
		last := sc.newest
		if fi, err := os.Stat(transcript); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
		idle := r.Now.Sub(last)
		if idle < r.Stale {
			continue
		}
		if name, why := r.alive(sid); why != "" {
			r.say("  keep %s  %q (%s): %s, idle %d h", size(sc.kib), name, sid, why, int(idle.Hours()))
			continue
		}
		if wt, _ := claude.TranscriptCwd(transcript); claude.LinkedWorktree(wt) {
			r.say("  keep %s  %s: its worktree %s remains, idle %d h", size(sc.kib), r.name(slug, sid), r.tilde(wt), int(idle.Hours()))
			continue
		}
		if r.inUse(d) {
			r.say("  keep (a process works or holds a file inside): %s", d)
			continue
		}
		n++
		if sc.foreign {
			r.say("  root-owned files, needs sudo: %d MiB  %s", sc.kib/1024, d)
			r.sudo = append(r.sudo, "sudo rm -rf "+guard.ShellQuote(d))
			continue
		}
		if sc.kib >= minKiB {
			r.act("remove %d MiB  %s  (CLI gone, idle %d h)", sc.kib/1024, r.name(slug, sid), int(idle.Hours()))
		}
		r.drop(SessionDirs, d, sc.kib)
	}
	r.total(SessionDirs, n)
}

// alive names the session sid and says why it lives on: its CLI runs, under
// this id or, after a restart, a newer one of its desktop session; or the
// desk counts on it. "" for a dead session.
func (r *Run) alive(sid string) (name, why string) {
	host := ""
	if rec := r.record(sid); rec != nil {
		host = rec.SessionID
	}
	for _, s := range r.Sessions {
		switch {
		case s.ID == sid:
			return s.Name, fmt.Sprintf("CLI alive, pid %d", s.PID)
		case host != "" && s.HostID == host:
			return s.Name, fmt.Sprintf("its desktop session runs a newer CLI, pid %d", s.PID)
		}
	}
	return r.counted(sid, host)
}

// counted says why the desk counts on the session sid, whose desktop session
// is host: an agent the roster keeps with its task (busy or parked) or the
// holder of the supervisor's or the guide's role. "" when it does not.
func (r *Run) counted(sid, host string) (name, why string) {
	is := func(p state.Party) bool { return p.Session == sid || host != "" && p.HostSession == host }
	for _, ag := range r.Desk.Agents {
		if ag.Task != "" && !ag.Done && is(ag.Party) {
			return ag.Name, "the roster keeps it with its task"
		}
	}
	holders := map[string]*state.Supervisor{roleSupervisor: r.Desk.SupervisorRole().Holder, roleGuide: r.Desk.GuideRole().Holder}
	for role, h := range holders {
		if h != nil && is(h.Party) {
			return h.Name, "it holds the " + role + "'s role"
		}
	}
	return "", ""
}

// record is the desktop record of the CLI session sid, nil for none.
func (r *Run) record(sid string) *claude.Record {
	if r.records == nil && r.Records != nil {
		r.records = r.Records()
	}
	return r.records[sid]
}

func (r *Run) title(sid string) string {
	if rec := r.record(sid); rec != nil {
		return rec.Title
	}
	return ""
}

// name is a session dir's session: its desktop title and id, else the dir.
func (r *Run) name(slug, sid string) string {
	if t := r.title(sid); t != "" {
		return fmt.Sprintf("%q (%s)", t, sid)
	}
	return slug + "/" + sid
}

// goBuilds frees the work dirs go commands left in TmpDir. A go command
// removes its go-build dir as it exits, unless it dies first (killed, out of
// memory): a go-build dir unchanged for GoBuildAge is orphaned once no go
// command of the user runs that began before its last change, and no process
// holds anything inside.
func (r *Run) goBuilds() {
	r.head(fmt.Sprintf("orphaned go-build dirs in %s (their go command gone) unchanged for %d min", r.TmpDir, int(GoBuildAge.Minutes())))
	if !r.tableRead() {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(r.TmpDir, "go-build*"))
	n := 0
	for _, d := range dirs {
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() || fileUID(fi) != r.UID {
			continue
		}
		sc := r.scan(d)
		last := sc.newest
		if last.IsZero() {
			last = fi.ModTime()
		}
		if r.Now.Sub(last) < GoBuildAge {
			continue
		}
		if p := r.goCommand(last); p != nil {
			r.say("  keep (go command pid %d began before its last change): %s", p.PID, d)
			continue
		}
		if r.inUse(d) {
			r.say("  keep (in use): %s", d)
			continue
		}
		n++
		if sc.kib >= minKiB {
			r.act("remove %d MiB  %s  (go command gone, unchanged %d h)", sc.kib/1024, d, int(r.Now.Sub(last).Hours()))
		}
		r.drop(GoBuild, d, sc.kib)
	}
	r.total(GoBuild, n)
}

// goCommand is a go command of the user that began no later than at, which
// may own a work dir changed then; nil when none runs.
func (r *Run) goCommand(at time.Time) *proc.Process {
	for _, p := range r.procs() {
		if p.Comm == "go" && !p.Start.After(at) && r.uid(p.PID) == r.UID {
			return p
		}
	}
	return nil
}

// tmpPatterns are the throwaway temp dirs tools leave in /tmp: vm-manager's
// e2e runs, mktemp, pytest and jest's cache.
var tmpPatterns = []string{"vmm-e2e-*", "tmp.*", "pytest-of-*", "jest_rs"}

// tmpDirs frees the user's throwaway temp dirs untouched for Stale and not in
// use; jest's cache stays while jest runs.
func (r *Run) tmpDirs() {
	r.head(fmt.Sprintf("known throwaway temp dirs in %s older than %d h", r.TmpDir, int(r.Stale.Hours())))
	if !r.tableRead() {
		return
	}
	n := 0
	for _, pat := range tmpPatterns {
		dirs, _ := filepath.Glob(filepath.Join(r.TmpDir, pat))
		for _, d := range dirs {
			fi, err := os.Lstat(d)
			if err != nil || !fi.IsDir() || fileUID(fi) != r.UID {
				continue
			}
			sc := r.scan(d)
			if sc.newest.IsZero() || r.Now.Sub(sc.newest) < r.Stale {
				continue
			}
			if r.inUse(d) {
				r.say("  keep (in use): %s", d)
				continue
			}
			if pat == "jest_rs" && r.jestRuns() {
				r.say("  keep (jest running): %s", d)
				continue
			}
			if sc.kib < minKiB {
				continue
			}
			n++
			r.act("remove %d MiB  %s", sc.kib/1024, d)
			r.drop(TmpDirs, d, sc.kib)
		}
	}
	r.total(TmpDirs, n)
}

func (r *Run) jestRuns() bool {
	for _, p := range r.Table.ByPID {
		if c := p.Cmdline(); strings.Contains(c, "jest") || strings.Contains(c, "processChild.js") {
			return true
		}
	}
	return false
}

// drop removes d and counts it to section; a dry run only counts it.
func (r *Run) drop(section, d string, kib int) {
	if r.Apply {
		if err := os.RemoveAll(d); err != nil {
			r.say("  failed: %v", err)
			return
		}
		r.freedKiB += kib
	}
	r.tally(section, kib)
}

// total closes a section of dirs with what it freed, or would free, in all.
func (r *Run) total(section string, n int) {
	switch {
	case n == 0:
		r.say("  nothing stale")
	case r.sumN[section] > 0:
		verb := "would free"
		if r.Apply {
			verb = "freed"
		}
		r.say("  %s %s", verb, Freed{Section: section, N: r.sumN[section], KiB: r.sumKiB[section]})
	}
}

// orphans kills the user's workers whose parent is gone, reparented to PID 1
// or to the user's systemd, older than Orphan: jest workers and Claude CLIs
// whose desktop died.
func (r *Run) orphans() {
	r.head(fmt.Sprintf("orphaned workers (parent gone, reparented to systemd) older than %d min", int(r.Orphan.Minutes())))
	if !r.tableRead() {
		return
	}
	var pids []int
	for _, p := range r.procs() {
		if !r.Table.Orphaned(p) || p.Elapsed(r.Now) < r.Orphan || r.uid(p.PID) != r.UID {
			continue
		}
		c := p.Cmdline()
		if !strings.Contains(c, "processChild.js") && !strings.Contains(c, "jest-worker") && !strings.Contains(c, "claude --output-format stream-json") {
			continue
		}
		kib := r.anonKiB(p.PID)
		r.tally(Orphans, kib)
		r.act("kill pid %d (%d MiB, up %s): %s", p.PID, kib/1024, uptime(p.Elapsed(r.Now)), truncate(c, 90))
		pids = append(pids, p.PID)
		if r.Apply {
			r.freedKiB += kib
		}
	}
	if len(pids) == 0 {
		r.say("  none")
		return
	}
	if r.Apply {
		r.kill(pids)
	}
}

// terminate sends TERM, and KILL to what survives two seconds.
func terminate(pids []int) { proc.Terminate(pids, 2*time.Second) }

// swap offers the swap reset when RAM can take the swapped pages back. It
// needs root, so free only prints the command.
func (r *Run) swap() {
	r.head("swap")
	m, err := r.mem()
	switch {
	case platform.Missing(err):
		r.say("  %s", platform.Unavailable("memory"))
		return
	case err != nil:
		r.say("  cannot read /proc/meminfo: %v", err)
		return
	}
	switch {
	case m.SwapUsedMiB < 512:
		r.say("  %d MiB in swap, nothing to reset", m.SwapUsedMiB)
		return
	case m.AvailableMiB < 2*m.SwapUsedMiB:
		r.say("  %d MiB in swap but only %d MiB RAM available; a reset now would risk an OOM, free memory first", m.SwapUsedMiB, m.AvailableMiB)
		return
	}
	r.tally(Swap, m.SwapUsedMiB*1024)
	r.say("  %d MiB of cold pages in swap (%s); RAM has room, the reset needs root:", m.SwapUsedMiB, m.SwapSplit())
	r.sudo = append(r.sudo, SwapResetCmd)
}

// inUse reports whether some process works inside d or holds something
// there: its working directory, its executable or an open file.
func (r *Run) inUse(d string) bool {
	if r.held == nil {
		r.held = []string{}
		tmp, scratch := filepath.Clean(r.TmpDir), filepath.Clean(r.Scratch)
		for pid := range r.Table.ByPID {
			for _, p := range r.holds(pid) {
				if within(p, tmp) || within(p, scratch) {
					r.held = append(r.held, p)
				}
			}
		}
	}
	return slices.ContainsFunc(r.held, func(p string) bool { return within(p, d) })
}

// within reports whether path p is d or lies under it.
func within(p, d string) bool { return p == d || strings.HasPrefix(p, d+"/") }

type dirScan struct {
	kib     int
	newest  time.Time // newest file mtime; zero when d holds no file
	foreign bool      // something inside belongs to another user
}

func (r *Run) scan(d string) dirScan {
	var s dirScan
	var bytes int64
	_ = filepath.WalkDir(d, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return nil
		}
		if fileUID(fi) != r.UID {
			s.foreign = true
		}
		if fi.Mode().IsRegular() {
			bytes += fi.Size()
			if fi.ModTime().After(s.newest) {
				s.newest = fi.ModTime()
			}
		}
		return nil
	})
	s.kib = int(bytes / 1024)
	return s
}

// size renders KiB as MiB, or KiB below one MiB.
func size(kib int) string {
	if kib < 1024 {
		return fmt.Sprintf("%d KiB", kib)
	}
	return fmt.Sprintf("%d MiB", kib/1024)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
