package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
)

func (a *app) watchCmd() *cobra.Command {
	var once bool
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stay silent until something needs a look, then say it in one line",
		Long: `Poll the machine every watch.interval (30s) and print one line per event,
silent otherwise: made to be the source of a Monitor, so the supervisor
wakes only when something needs a look.

Threshold breaches (RAM, swap, desktop scope, load, memory pressure, tmpfs,
disk, the GitHub budget) repeat at most every watch.repeat (10m) per kind.
OOM kills are never folded away: every poll reports every kill since the
last one, grouped by whose limit they hit. Sessions that start, end or
restart are reported, and so is a lease whose holder is gone. The
installations' alerts are read every alerts.every and each NEW or RESOLVED
one is a line (beekeeper alerts watch); only one watch at a time reads them.

Runs until killed. --once polls once and exits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			w := &watcher{app: a, last: map[string]time.Time{}, seenKills: map[string]bool{}, reportedLeases: map[string]bool{}}
			return w.run(ctx, once)
		},
	}
	c.Flags().BoolVar(&once, "once", false, "poll once and exit")
	return c
}

type watcher struct {
	*app
	mu             sync.Mutex
	last           map[string]time.Time
	lastPoll       time.Time
	lastBudget     time.Time
	scopeOOM       int64
	sessions       map[string]*claude.Session
	seenKills      map[string]bool
	reportedLeases map[string]bool
}

func (w *watcher) run(ctx context.Context, once bool) error {
	w.lastPoll = time.Now().Add(-w.cfg.Watch.Interval.Duration)
	if p := machine.FindScope(); p != "" {
		w.scopeOOM = machine.ReadScope(p).OOMKills
	} else {
		w.emitNow("scope", "no Claude Desktop scope found; watching the machine numbers only")
	}
	var wg sync.WaitGroup
	if !once {
		wg.Go(func() { w.watchAlerts(ctx) })
	}
	defer wg.Wait()
	tick := time.NewTicker(w.cfg.Watch.Interval.Duration)
	defer tick.Stop()
	for {
		w.poll(ctx)
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// watchAlerts reads the alerts every alerts.every while this watch owns the
// baseline; another watch that owns it is said once, and this one takes over
// when it ends.
func (w *watcher) watchAlerts(ctx context.Context) {
	store := alerts.NewStore(w.cfg.StateDir)
	defer func() { _ = store.Release() }()
	other := 0
	for {
		start := time.Now()
		owned, owner, err := store.Own()
		switch {
		case err != nil:
			w.emit("alerts", "ALERTS baseline unusable: %v", err)
		case !owned:
			if owner.PID != other {
				other = owner.PID
				w.emitNow("alerts", "ALERTS read by the watch with pid %d; this one takes over when it ends", other)
			}
		default:
			if other != 0 {
				other = 0
				w.emitNow("alerts", "ALERTS taken over by this watch")
			}
			for _, l := range w.alertCycle(ctx, store) {
				w.emitNow("alerts", "%s", l)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(time.Until(start.Add(w.cfg.Alerts.Every.Duration)), 0)):
		}
	}
}

// emit prints a breach at most once per watch.repeat per key.
func (w *watcher) emit(key, format string, args ...any) {
	w.mu.Lock()
	now := time.Now()
	if t, ok := w.last[key]; ok && now.Sub(t) < w.cfg.Watch.Repeat.Duration {
		w.mu.Unlock()
		return
	}
	w.last[key] = now
	w.mu.Unlock()
	w.emitNow(key, format, args...)
}

// emitNow prints an event that is never folded away.
func (w *watcher) emitNow(_ string, format string, args ...any) {
	w.emitLine(time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...))
}

// emitLine prints one complete line.
func (w *watcher) emitLine(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintln(w.out, line)
}

func (w *watcher) poll(ctx context.Context) {
	w.now = time.Now()
	since := w.lastPoll
	w.lastPoll = w.now
	th := w.cfg.Watch

	if m, err := machine.ReadMem(); err == nil {
		if m.AvailableMiB < th.AvailMinMiB {
			w.emit("avail", "LOW RAM: %d MiB available (swap used %d MiB)", m.AvailableMiB, m.SwapUsedMiB)
		}
		if m.SwapUsedMiB > th.SwapMaxMiB {
			w.emit("swap", "SWAP near the oomd trigger: %d of %d MiB used (systemd-oomd kills at 90%%)", m.SwapUsedMiB, m.SwapTotalMiB)
		}
	}
	if l, err := machine.ReadLoad(); err == nil && l[0] > th.LoadMax {
		w.emit("load", "HIGH LOAD: %.0f (image imports into a fresh lab reach 35-53 on an encrypted disk)", l[0])
	}
	if psi, err := machine.ReadPSIFull60(); err == nil && psi > th.PSIMax {
		w.emit("psi", "MEMORY PRESSURE: full avg60 %.0f%%", psi)
	}
	if d, err := machine.ReadDisk("/tmp"); err == nil && d.UsedMiB > th.TmpMaxMiB {
		w.emit("tmp", "TMPFS /tmp at %d MiB (RAM-backed scratch)", d.UsedMiB)
	}
	if d, err := machine.ReadDisk("/"); err == nil && d.FreeMiB < th.DiskMinMiB {
		w.emit("disk", "LOW DISK: / has %d GiB free (go clean -cache; prune unreferenced images and volumes)", d.FreeMiB/1024)
	}
	if p := machine.FindScope(); p != "" {
		s := machine.ReadScope(p)
		if s.AnonMiB > th.ScopeAnonMaxMiB {
			w.emit("scopeanon", "DESKTOP SCOPE anonymous memory %d MiB (only anon cannot be reclaimed; archiving idle sessions frees it)", s.AnonMiB)
		}
		if s.CurrentMiB > th.ScopeMaxMiB {
			w.emit("scope", "DESKTOP SCOPE near its hard cap: %d MiB RAM + %d MiB swap (max %s)", s.CurrentMiB, s.SwapMiB, s.Max)
		}
		if s.OOMKills != w.scopeOOM {
			w.emitNow("scopeoom", "OOM KILL in the desktop scope: oom_kill %d -> %d", w.scopeOOM, s.OOMKills)
			w.scopeOOM = s.OOMKills
		}
	}

	t, err := proc.Read()
	if err != nil {
		w.emit("proc", "cannot read the process table: %v", err)
		return
	}
	sessions := claude.Discover(w.cfg, t, w.now)
	w.kills(ctx, since, sessions, t)
	w.sessionChanges(sessions)
	w.staleLeases(sessions)

	if w.now.Sub(w.lastBudget) >= th.BudgetEvery.Duration {
		w.lastBudget = w.now
		b, err := w.probeBudget(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
		case err != nil:
			w.emit("budget-error", "GitHub budget unknown: %v", err)
		case b.Remaining < w.cfg.GitHub.Floor:
			w.emit("budget", "GITHUB BUDGET %d of %d, under the floor %d: hold GitHub work until the reset at %s",
				b.Remaining, b.Limit, w.cfg.GitHub.Floor, b.Reset.Local().Format("15:04"))
		}
	}
}

// commandTimeout bounds one journalctl or docker call of a poll: a wedged
// daemon must not stall the watch or its shutdown.
const commandTimeout = 20 * time.Second

func (w *watcher) kills(ctx context.Context, since time.Time, sessions []*claude.Session, t *proc.Table) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	kills, err := machine.OOMKills(ctx, since.Add(-2*time.Second))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.emit("journal", "cannot read the kernel journal: %v", err)
		return
	}
	var fresh []oomKill
	var clusters []machine.Cluster
	for _, k := range kills {
		key := fmt.Sprintf("%d@%d", k.PID, k.At.Unix())
		if w.seenKills[key] {
			continue
		}
		w.seenKills[key] = true
		if clusters == nil && strings.Contains(k.Memcg, "docker-") {
			clusters, _ = machine.KindClusters(ctx)
		}
		fresh = append(fresh, oomKill{OOMKill: k, Owner: oomOwner(k, clusters, sessions, t)})
	}
	for _, line := range groupKills(fresh) {
		w.emitNow("kern", "KERNEL OOM: %s", line)
	}
	if lines, err := machine.OomdKills(ctx, since.Add(-2*time.Second)); err == nil {
		for _, l := range lines {
			if !w.seenKills[l] {
				w.seenKills[l] = true
				w.emitNow("oomd", "SYSTEMD-OOMD: %s", truncate(l, 200))
			}
		}
	}
}

func (w *watcher) sessionChanges(sessions []*claude.Session) {
	cur := map[string]*claude.Session{}
	for _, s := range sessions {
		cur[sessionKey(s)] = s
	}
	if w.sessions == nil {
		w.sessions = cur
		return
	}
	var started, ended, restarted []string
	for k, s := range cur {
		prev, ok := w.sessions[k]
		switch {
		case !ok:
			started = append(started, fmt.Sprintf("%q", s.Name))
		case prev.PID != s.PID:
			restarted = append(restarted, fmt.Sprintf("%q", s.Name))
		}
	}
	for k, s := range w.sessions {
		if _, ok := cur[k]; !ok {
			ended = append(ended, fmt.Sprintf("%q", s.Name))
		}
	}
	for _, l := range [][]string{started, ended, restarted} {
		slices.Sort(l)
	}
	if len(started) > 0 {
		w.emitNow("sessions", "SESSIONS started: %s", strings.Join(started, ", "))
	}
	if len(ended) > 0 {
		w.emitNow("sessions", "SESSIONS ended (CLI gone: paused, closed or crashed): %s", strings.Join(ended, ", "))
	}
	if len(restarted) > 0 {
		w.emitNow("sessions", "SESSIONS restarted (a new CLI: its context may be fresh, send it its state): %s", strings.Join(restarted, ", "))
	}
	w.sessions = cur
}

func sessionKey(s *claude.Session) string {
	if s.HostID != "" {
		return s.HostID
	}
	if s.ID != "" {
		return s.ID
	}
	return fmt.Sprint(s.PID)
}

func (w *watcher) staleLeases(sessions []*claude.Session) {
	holders, err := lease.Dir(w.cfg.LeaseDir).List()
	if err != nil {
		return
	}
	for _, h := range holders {
		v := w.leaseView(sessions, h)
		key := h.Env + "@" + h.Since
		if v.State == holderGone && !w.reportedLeases[key] {
			w.reportedLeases[key] = true
			w.emitNow("lease", "STALE LEASE: %s is held by %q, whose session no longer runs (%s)", h.Env, v.Name, truncate(h.Purpose, 60))
		}
	}
}
