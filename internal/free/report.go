package free

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// state prints RAM, swap, tmpfs, the oomd headroom and the desktop scope.
func (r *Run) state() {
	m, _ := r.mem()
	tmp, _ := machine.ReadDisk(r.TmpDir)
	r.say("  RAM available %6d MiB of %d MiB   swap used %5d MiB of %d MiB   tmpfs /tmp %5d MiB   shmem %5d MiB",
		m.AvailableMiB, m.TotalMiB, m.SwapUsedMiB, m.SwapTotalMiB, tmp.UsedMiB, m.ShmemMiB)
	if m.SwapTotalMiB > 0 {
		r.say("  systemd-oomd swap trigger (90 %%): %d MiB of swap growth left before it kills the largest scope",
			m.SwapTotalMiB*90/100-m.SwapUsedMiB)
	}
	path := r.scope()
	if path == "" {
		return
	}
	s := machine.ReadScope(path)
	procs, _ := os.ReadFile(filepath.Clean(filepath.Join(path, "cgroup.procs")))
	r.say("  Claude Desktop scope: %d MiB RAM, %d MiB swap, %d processes, %d Claude CLIs",
		s.CurrentMiB, s.SwapMiB, strings.Count(string(procs), "\n"), r.claudeCount())
	if s.Max == "max" {
		r.say("  memory guard: none on the scope (no MemoryMax): the first kernel OOM kill can take every session with it")
		return
	}
	pol := oomPolicy(filepath.Base(path))
	warn := ""
	if pol != "continue" {
		warn = "  <- must be continue, or the first kernel kill stops the whole scope"
	}
	r.say("  memory guard: high %s, max %s, swap max %s, OOMPolicy=%s%s", capMiB(s.High), capMiB(s.Max), capMiB(s.SwapMax), pol, warn)
}

func (r *Run) claudeCount() int {
	n := 0
	for _, p := range r.Table.ByPID {
		if p.Comm == "claude" {
			n++
		}
	}
	return n
}

// capMiB renders a machine.Scope limit ("max" or MiB).
func capMiB(v string) string {
	if v == "max" {
		return "none"
	}
	return v + " MiB"
}

func oomPolicy(unit string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", unit, "-p", "OOMPolicy", "--value").Output() // #nosec G204 -- the unit is the desktop scope's cgroup name, one argument
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(out))
}

// slots prints who holds the build slots `beekeeper run` hands out.
func (r *Run) slots() {
	if r.Slots < 1 {
		return
	}
	r.head("build slots (beekeeper run; a heavy command waits here instead of piling on)")
	for _, s := range machine.ReadSlots(r.SlotDir, r.Slots) {
		if s.Free {
			r.say("  slot %d: free", s.N)
		} else {
			r.say("  slot %d: %s", s.N, s.Holder)
		}
	}
}

// kind reports the kind clusters; tearing one down is its owner's call.
func (r *Run) kind() {
	r.head("kind clusters (not touched; the owning session runs agentlab down)")
	if r.ClustersErr != nil {
		r.say("  docker not available")
		return
	}
	r.say("  (memory of each cluster's node cgroups)")
	for _, c := range r.Clusters {
		r.emit("kind", c.Name, c.MemMiB, c.RunningFor)
		r.say("  %-36s %6d MiB  up since %s", c.Name, c.MemMiB, c.RunningFor)
	}
	if len(r.Clusters) > guard.MaxLabs {
		r.say("  %d labs up; this machine holds %d beside a build or test session, tear the finished ones down (agentlab down)",
			len(r.Clusters), guard.MaxLabs)
	}
}

// clis reports the live Claude CLIs; archiving an idle one is a desktop
// action. Its scratch stays: only a dead session's is freed.
func (r *Run) clis() {
	r.head("Claude CLIs alive (each keeps its MCP servers; archive idle sessions in the desktop)")
	n, mib := 0, 0
	for _, s := range r.Sessions {
		var idle time.Duration
		if !s.LastActive.IsZero() {
			idle = r.Now.Sub(s.LastActive)
		}
		mark := ""
		if idle >= r.Stale {
			n++
			mib += s.MemMiB
			mark = "   <- idle, archive it"
			r.emit("cli", s.PID, s.MemMiB, int(idle.Minutes()), r.tilde(s.Cwd))
		}
		r.say("  pid %-8d %5d MiB  idle %5d min  %s  (%s)%s", s.PID, s.MemMiB, int(idle.Minutes()), s.Name, r.tilde(s.Cwd), mark)
	}
	if n > 0 {
		r.say("  %d idle CLIs hold %d MiB; archiving their sessions in the desktop frees it", n, mib)
	}
}

// heavy reports the user's processes above HeavyMiB of anonymous RSS or
// burning RunawayCPU % of their lifetime for Runaway of CPU time (the model
// case: a hook spinning on one core for hours). Not the ones other rows or
// tools own: kind node containers, the Claude Desktop scope and app, the
// browser, the compositor stack in session.slice and the bar, terminal and
// dialog the front end runs in. A systemd user service is one row, the unit
// with its whole anonymous RSS: a kill would be undone by Restart=.
func (r *Run) heavy() {
	if r.HeavyMiB <= 0 {
		return
	}
	r.head("heavy or runaway processes (not touched; the dialog offers them, or kill them yourself)")
	type row struct {
		anon                  int
		pid                   int
		up, comm, detail, why string
	}
	var rows []row
	units := map[string]bool{}
	heavyKiB := r.HeavyMiB * 1024
	for _, p := range r.procs() {
		why := ""
		if pct := cpuShare(p, r.Now); p.CPU >= r.Runaway && pct >= r.RunawayCPU {
			why = fmt.Sprintf("runaway:%d%%", pct)
		}
		// RssAnon never exceeds RSS: the small fry need no status read.
		if p.RSSKiB < heavyKiB && why == "" {
			continue
		}
		if r.uid(p.PID) != r.UID {
			continue
		}
		anon := r.anonKiB(p.PID)
		if anon >= heavyKiB {
			why = strings.TrimSuffix("heavy,"+why, ",")
		}
		if why == "" || isBrowser(p.Cmdline()) || strings.HasPrefix(p.Cmdline(), "/usr/lib/claude-desktop/") {
			continue
		}
		cg := r.cgroup(p.PID)
		if strings.Contains(cg, "/session.slice/") || isDesktopScope(cg) || strings.Contains(cg, "docker-") {
			continue
		}
		switch p.Comm {
		case "Hyprland", "waybar", "kitty", "zenity":
			continue
		}
		detail := p.Cmdline()
		if leaf := path.Base(cg); strings.HasSuffix(leaf, ".service") && strings.Contains(cg, "/user@"+strconv.Itoa(r.UID)+".service/") {
			if units[leaf] {
				continue
			}
			units[leaf] = true
			anon = r.cgroupAnonKiB(cg)
			detail = "unit:" + leaf
		} else if rs := []rune(detail); len(rs) > 120 {
			detail = string(rs[:120])
		}
		rows = append(rows, row{anon, p.PID, uptime(p.Elapsed(r.Now)), p.Comm, oneLine(detail), why})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return b.anon - a.anon })
	for _, w := range rows {
		r.emit("proc", w.pid, w.anon/1024, w.up, w.comm, w.detail, w.why)
		r.say("  pid %-8d %5d MiB  up %-9s %-18s %-15s %s", w.pid, w.anon/1024, w.up, w.why, w.comm, truncate(w.detail, 70))
	}
	if len(rows) == 0 {
		r.say("  none above %d MiB or %d %% CPU for %d min", r.HeavyMiB, r.RunawayCPU, int(r.Runaway.Minutes()))
	}
}

// cpuShare is the share of its lifetime p spent on a CPU, in percent.
func cpuShare(p *proc.Process, now time.Time) int {
	el := p.Elapsed(now)
	if el <= 0 {
		return 0
	}
	return int(p.CPU * 100 / el)
}

func (r *Run) cgroupAnonKiB(cg string) int {
	kib := 0
	for _, pid := range r.unitPIDs(cg) {
		kib += r.anonKiB(pid)
	}
	return kib
}

// cgroupPIDs lists the processes of a cgroup v2 path.
func cgroupPIDs(cg string) []int {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join("/sys/fs/cgroup", cg, "cgroup.procs")))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

// isBrowser matches Chrome's and Chromium's processes by their command line,
// which Chrome rewrites into one space-separated argument.
func isBrowser(cmdline string) bool {
	return strings.HasPrefix(cmdline, "/opt/google/chrome/chrome") || strings.Contains(cmdline+" ", "/chromium ")
}

// isDesktopScope matches the Claude Desktop scope, with or without a
// compositor prefix.
func isDesktopScope(cg string) bool {
	return strings.Contains(cg, "com.anthropic.Claude") && strings.Contains(cg, ".scope")
}

// tabs reports the Chrome renderers above TabMiB, largest first, skipping
// extension processes (killing one gains nothing). No titles: the browser
// runs without remote debugging, so a renderer can only be named by its
// process ("Aw, Snap!" in its tabs until they are reloaded).
func (r *Run) tabs() {
	if r.TabMiB <= 0 {
		return
	}
	r.head(fmt.Sprintf("Chrome renderers above %d MiB (not touched; a killed renderer shows 'Aw, Snap!' in its tabs until reloaded)", r.TabMiB))
	type row struct{ anon, pid int }
	var rows []row
	tabKiB := r.TabMiB * 1024
	up := map[int]string{}
	for _, p := range r.procs() {
		c := p.Cmdline()
		if !isBrowser(c) || !strings.Contains(c+" ", " --type=renderer ") || strings.Contains(c, "--extension-process") || p.RSSKiB < tabKiB {
			continue
		}
		if r.uid(p.PID) != r.UID {
			continue
		}
		if anon := r.anonKiB(p.PID); anon >= tabKiB {
			rows = append(rows, row{anon, p.PID})
			up[p.PID] = uptime(p.Elapsed(r.Now))
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int { return b.anon - a.anon })
	for _, w := range rows {
		r.emit("tab", w.pid, w.anon/1024, up[w.pid], "renderer")
		r.say("  pid %-8d %5d MiB  up %-9s renderer", w.pid, w.anon/1024, up[w.pid])
	}
	if len(rows) == 0 {
		r.say("  none")
	}
}

func truncate(s string, n int) string {
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n])
	}
	return s
}
