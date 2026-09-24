package free

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// minKiB is the size below which a dir is not worth a line.
const minKiB = 1024

// sessionDirs frees the scratch of dead sessions: the session dirs under
// Scratch whose CLI is gone, untouched (transcript and files) for Stale, with
// no process working inside. A live session's scratch is kept however idle
// it is: the session still reads it when it resumes work.
func (r *Run) sessionDirs() {
	r.head(fmt.Sprintf("dead sessions' dirs in %s (tmpfs; a live session's are kept however idle)", r.Scratch))
	live := map[string]*claude.Session{}
	for _, s := range r.Sessions {
		if s.ID != "" {
			live[s.ID] = s
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(r.Scratch, "*", "*"))
	n := 0
	for _, d := range dirs {
		if !isDir(filepath.Join(d, "scratchpad")) && !isDir(filepath.Join(d, "tasks")) {
			continue
		}
		sid, slug := filepath.Base(d), filepath.Base(filepath.Dir(d))
		sc := r.scan(d)
		last := sc.newest
		if fi, err := os.Stat(filepath.Join(r.Projects, slug, sid+".jsonl")); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
		idle := r.Now.Sub(last)
		if idle < r.Stale {
			continue
		}
		if s, ok := live[sid]; ok {
			r.say("  keep %s  %q (%s): CLI alive, pid %d, idle %d h", size(sc.kib), s.Name, sid, s.PID, int(idle.Hours()))
			continue
		}
		if sc.kib < minKiB {
			continue
		}
		if r.inUse(d) {
			r.say("  keep (a process runs inside): %s", d)
			continue
		}
		n++
		if sc.foreign {
			r.say("  root-owned files, needs sudo: %d MiB  %s", sc.kib/1024, d)
			r.sudo = append(r.sudo, "sudo rm -rf "+guard.ShellQuote(d))
			continue
		}
		r.tally(SessionDirs, sc.kib)
		name := slug + "/" + sid
		if t := r.title(sid); t != "" {
			name = fmt.Sprintf("%q (%s)", t, sid)
		}
		r.act("remove %d MiB  %s  (CLI gone, idle %d h)", sc.kib/1024, name, int(idle.Hours()))
		r.remove(d, sc.kib)
	}
	if n == 0 {
		r.say("  nothing stale")
	}
}

func (r *Run) title(sid string) string {
	if r.titles == nil && r.Titles != nil {
		r.titles = r.Titles()
	}
	return r.titles[sid]
}

// tmpPatterns are the throwaway temp dirs tools leave in /tmp: vm-manager's
// e2e runs, go build, mktemp, pytest and jest's cache.
var tmpPatterns = []string{"vmm-e2e-*", "go-build*", "tmp.*", "pytest-of-*", "jest_rs"}

// tmpDirs frees the user's throwaway temp dirs untouched for Stale and not in
// use; jest's cache stays while jest runs.
func (r *Run) tmpDirs() {
	r.head(fmt.Sprintf("known throwaway temp dirs in %s older than %d h", r.TmpDir, int(r.Stale.Hours())))
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
			r.tally(TmpDirs, sc.kib)
			r.act("remove %d MiB  %s", sc.kib/1024, d)
			r.remove(d, sc.kib)
		}
	}
	if n == 0 {
		r.say("  nothing stale")
	}
}

func (r *Run) jestRuns() bool {
	for _, p := range r.Table.ByPID {
		if c := p.Cmdline(); strings.Contains(c, "jest") || strings.Contains(c, "processChild.js") {
			return true
		}
	}
	return false
}

func (r *Run) remove(d string, kib int) {
	if !r.Apply {
		return
	}
	if err := os.RemoveAll(d); err != nil {
		r.say("  failed: %v", err)
		return
	}
	r.freedKiB += kib
}

// orphans kills the user's workers whose parent is gone, reparented to PID 1
// or to the user's systemd, older than Orphan: jest workers and Claude CLIs
// whose desktop died.
func (r *Run) orphans() {
	r.head(fmt.Sprintf("orphaned workers (parent gone, reparented to systemd) older than %d min", int(r.Orphan.Minutes())))
	var pids []int
	for _, p := range r.procs() {
		if !r.orphaned(p) || p.Elapsed(r.Now) < r.Orphan || r.uid(p.PID) != r.UID {
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

func (r *Run) orphaned(p *proc.Process) bool {
	if p.PPID == 1 {
		return true
	}
	parent := r.Table.ByPID[p.PPID]
	return parent != nil && parent.Comm == "systemd"
}

// terminate sends TERM, and KILL to what survives two seconds.
func terminate(pids []int) {
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	time.Sleep(2 * time.Second)
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
			_ = p.Kill()
		}
	}
}

// swap offers the swap reset when RAM can take the swapped pages back. It
// needs root, so free only prints the command.
func (r *Run) swap() {
	r.head("swap")
	m, err := r.mem()
	if err != nil {
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
	r.say("  %d MiB of cold pages in swap; RAM has room, the reset needs root:", m.SwapUsedMiB)
	r.sudo = append(r.sudo, SwapResetCmd)
}

// inUse reports whether some process works inside d.
func (r *Run) inUse(d string) bool {
	if r.cwds == nil {
		r.cwds = []string{}
		for pid := range r.Table.ByPID {
			if c := r.cwd(pid); c != "" {
				r.cwds = append(r.cwds, c)
			}
		}
	}
	for _, c := range r.cwds {
		if c == d || strings.HasPrefix(c, d+"/") {
			return true
		}
	}
	return false
}

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
