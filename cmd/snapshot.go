package cmd

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/check"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// snapshot is one tick of the machine and its sessions.
type snapshot struct {
	At          time.Time         `json:"at"`
	Load        [3]float64        `json:"load"`
	Mem         machine.Mem       `json:"mem"`
	PSIFull60   float64           `json:"psiFull60"`
	Scope       *machine.Scope    `json:"scope,omitempty"`
	Tmp         machine.Disk      `json:"tmp"`
	Root        machine.Disk      `json:"root"`
	Slots       []machine.Slot    `json:"slots"`
	Clusters    []machine.Cluster `json:"clusters"`
	ClustersErr string            `json:"clustersError,omitempty"`
	Sessions    []string          `json:"sessions"`
	CLIMemMiB   int               `json:"cliMemMiB"`
	Waits       []wait            `json:"waits,omitempty"`
	OOMSince    time.Time         `json:"oomSince"`
	OOM         []oomKill         `json:"oom,omitempty"`
	Oomd        []string          `json:"oomd,omitempty"`
	Leases      []leaseView       `json:"leases,omitempty"`
	Holds       []state.Hold      `json:"holds,omitempty"`
	Budget      *github.Budget    `json:"budget,omitempty"`
	BudgetErr   string            `json:"budgetError,omitempty"`
	Checks      []checkOutput     `json:"checks,omitempty"`
}

// checkOutput is a configured check's snapshot section.
type checkOutput struct {
	Name   string `json:"name"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// wait is a long-running command a session sits on: a devctl wait or merge,
// a gh watch, an image import, a port-forward.
type wait struct {
	PID     int           `json:"pid"`
	Session string        `json:"session,omitempty"`
	Args    string        `json:"args"`
	Elapsed time.Duration `json:"elapsed"`
}

// oomKill is a kernel OOM kill with whose it was.
type oomKill struct {
	machine.OOMKill
	Owner string `json:"owner"`
}

func (a *app) snapshotCmd() *cobra.Command {
	var since string
	var changes, noBudget, noChecks bool
	c := &cobra.Command{
		Use:   "snapshot",
		Short: "One tick: the machine, its sessions and what changed since your last snapshot",
		Long: `Print one screen of the machine's state: load, RAM and swap, memory
pressure, the Claude Desktop scope (anonymous memory is what can OOM it;
memory.current is mostly reclaimable cache), tmpfs and disk, build slots,
kind clusters, the commands sessions sit on (with their owner), the kernel
OOM kills since your last snapshot (every one counted, attributed to a
memcap scope, a kind lab or the desktop scope), leases, holds and the GitHub
budget, and each configured check's snapshot section.

Each caller's last snapshot is kept; the next one ends with what changed
since. --changes prints only that: the quiet tick.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			me, err := a.caller()
			if err != nil {
				me = state.Party{Name: "anonymous"}
			}
			file := "snapshot." + fileKey(me) + ".json"
			var prev snapshot
			found, err := a.store.ReadFile(file, &prev)
			if err != nil {
				return err
			}
			oomSince := a.now.Add(-time.Hour)
			if found {
				oomSince = prev.At
			}
			if since != "" {
				t, err := sinceTime(a.now, since)
				if err != nil {
					return err
				}
				oomSince = t
			}
			s, err := a.takeSnapshot(cmd.Context(), oomSince, !noBudget, !noChecks && !changes)
			if err != nil {
				return err
			}
			if err := a.store.WriteFile(file, s); err != nil {
				return err
			}
			var diff []string
			if found {
				diff = diffSnapshots(&prev, s)
			}
			if a.json {
				return a.printJSON(struct {
					*snapshot
					Changes []string `json:"changes,omitempty"`
				}{s, diff})
			}
			if !changes {
				a.printSnapshot(s)
			}
			switch {
			case !found:
			case len(diff) == 0:
				_, _ = fmt.Fprintf(a.out, "no change since %s\n", clock(a.now, prev.At))
			default:
				_, _ = fmt.Fprintf(a.out, "since %s (%s ago):\n", clock(a.now, prev.At), dur(a.now.Sub(prev.At)))
				for _, d := range diff {
					_, _ = fmt.Fprintf(a.out, "  %s\n", d)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&since, "since", "", "count OOM kills since this time (15:04) or duration (2h) instead of your last snapshot")
	c.Flags().BoolVar(&changes, "changes", false, "print only what changed since your last snapshot")
	c.Flags().BoolVar(&noBudget, "no-budget", false, "skip the GitHub budget probe")
	c.Flags().BoolVar(&noChecks, "no-checks", false, "skip the configured checks' snapshot commands")
	return c
}

var unsafeKey = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func fileKey(p state.Party) string {
	k := p.HostSession
	if k == "" {
		k = p.Session
	}
	if k == "" {
		k = p.Name
	}
	return unsafeKey.ReplaceAllString(k, "_")
}

func sinceTime(now time.Time, s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	t, err := time.ParseInLocation("15:04", s, time.Local)
	if err != nil {
		return time.Time{}, &exitError{code: ExitUsage, msg: fmt.Sprintf("--since %q is neither 15:04 nor a duration", s)}
	}
	t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	if t.After(now) {
		t = t.Add(-24 * time.Hour)
	}
	return t, nil
}

func (a *app) takeSnapshot(ctx context.Context, oomSince time.Time, withBudget, withChecks bool) (*snapshot, error) {
	s := &snapshot{At: a.now.UTC(), OOMSince: oomSince.UTC()}
	var err error
	if s.Mem, err = machine.ReadMem(); err != nil {
		return nil, err
	}
	s.Load, _ = machine.ReadLoad()
	s.PSIFull60, _ = machine.ReadPSIFull60()
	if p := machine.FindScope(); p != "" {
		s.Scope = machine.ReadScope(p)
	}
	s.Tmp, _ = machine.ReadDisk("/tmp")
	s.Root, _ = machine.ReadDisk("/")
	s.Slots = machine.ReadSlots(a.cfg.Memcap.SlotDir, a.cfg.Memcap.Slots)
	if s.Clusters, err = machine.KindClusters(ctx); err != nil {
		s.ClustersErr = err.Error()
	}
	t, err := proc.Read()
	if err != nil {
		return nil, err
	}
	sessions := claude.Discover(a.cfg, t, a.now)
	for _, ss := range sessions {
		s.Sessions = append(s.Sessions, ss.Name)
		s.CLIMemMiB += ss.MemMiB
	}
	slices.Sort(s.Sessions)
	s.Waits = findWaits(t, sessions, a.now)
	kills, err := machine.OOMKills(ctx, oomSince)
	if err != nil {
		return nil, err
	}
	for _, k := range kills {
		s.OOM = append(s.OOM, oomKill{OOMKill: k, Owner: oomOwner(k, s.Clusters, sessions, t)})
	}
	s.Oomd, _ = machine.OomdKills(ctx, oomSince)
	holders, err := lease.Dir(a.cfg.LeaseDir).List()
	if err != nil {
		return nil, err
	}
	for _, h := range holders {
		s.Leases = append(s.Leases, a.leaseView(sessions, h))
	}
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	s.Holds = a.activeHolds(st)
	if withBudget {
		b, err := a.probeBudget(ctx)
		if err != nil {
			s.BudgetErr = err.Error()
		} else {
			s.Budget = &b
		}
	}
	if withChecks {
		for _, c := range a.cfg.Checks {
			if len(c.Snapshot) == 0 {
				continue
			}
			out, err := check.Snapshot(ctx, c)
			co := checkOutput{Name: c.Name, Output: out}
			if err != nil {
				co.Error = err.Error()
			}
			s.Checks = append(s.Checks, co)
		}
	}
	return s, nil
}

// probeBudget reads the GitHub budget with the stored ETag and keeps the new one.
func (a *app) probeBudget(ctx context.Context) (github.Budget, error) {
	token, err := github.Token(ctx)
	if err != nil {
		return github.Budget{}, err
	}
	st, err := a.store.Read()
	if err != nil {
		return github.Budget{}, err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	b, etag, err := github.Probe(ctx, client, token, a.cfg.GitHub.ProbeRepo, st.BudgetETag)
	if err != nil {
		return b, err
	}
	if etag != st.BudgetETag {
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.BudgetETag = etag
			return nil, nil
		})
	}
	return b, nil
}

// findWaits lists the long-running commands sessions block on.
func findWaits(t *proc.Table, sessions []*claude.Session, now time.Time) []wait {
	var out []wait
	for _, p := range t.ByPID {
		if !isWait(p) {
			continue
		}
		w := wait{PID: p.PID, Args: p.Cmdline(), Elapsed: p.Elapsed(now).Round(time.Second)}
		if s, ok := claude.OwnerOf(sessions, p.PID); ok {
			w.Session = s.Name
		}
		out = append(out, w)
	}
	slices.SortFunc(out, func(x, y wait) int { return int(y.Elapsed - x.Elapsed) })
	return out
}

func isWait(p *proc.Process) bool {
	has := func(words ...string) bool {
		for _, w := range words {
			if !slices.Contains(p.Args, w) {
				return false
			}
		}
		return true
	}
	switch p.Comm {
	case "devctl":
		return has("pr", "wait") || has("pr", "merge") || has("release", "wait")
	case "gh":
		return has("--watch") || has("run", "watch")
	case "ctr":
		return has("import")
	case "docker":
		return len(p.Args) > 1 && (p.Args[1] == "save" || p.Args[1] == "load" || has("image", "save") || has("image", "load"))
	case "kubectl":
		return has("port-forward")
	}
	return false
}

var memcapScope = regexp.MustCompile(`memcap-(\d+)-`)

// oomOwner names whose limit an OOM kill hit.
func oomOwner(k machine.OOMKill, clusters []machine.Cluster, sessions []*claude.Session, t *proc.Table) string {
	switch {
	case strings.Contains(k.Memcg, "memcap"):
		if m := memcapScope.FindStringSubmatch(k.Memcg); m != nil {
			pid, _ := strconv.Atoi(m[1])
			if s, ok := claude.OwnerOf(sessions, pid); ok {
				return fmt.Sprintf("memcap cap of %q's command", s.Name)
			}
			if p := t.ByPID[pid]; p != nil {
				return "memcap cap of `" + truncate(p.Cmdline(), 60) + "`"
			}
		}
		return "memcap cap on one command (its session has moved on)"
	case strings.Contains(k.Memcg, "docker-"):
		for _, c := range clusters {
			for _, id := range c.Containers {
				if strings.Contains(k.Memcg, "docker-"+id) {
					return "a pod's limit in kind lab " + c.Name
				}
			}
		}
		return "a container's limit"
	case strings.Contains(k.Memcg, "com.anthropic.Claude"):
		return "the Claude Desktop scope"
	case k.Constraint == "CONSTRAINT_NONE":
		return "the whole machine"
	}
	return k.Memcg
}

func (a *app) printSnapshot(s *snapshot) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }
	p("time %s  load %.1f %.1f %.1f  memory PSI full avg60 %.1f%%", clock(a.now, s.At), s.Load[0], s.Load[1], s.Load[2], s.PSIFull60)
	p("RAM available %d of %d MiB  swap used %d of %d MiB", s.Mem.AvailableMiB, s.Mem.TotalMiB, s.Mem.SwapUsedMiB, s.Mem.SwapTotalMiB)
	if sc := s.Scope; sc != nil {
		p("desktop scope anon %d MiB (current %d, high %s, max %s)  swap %d/%s MiB  oom_kill %d  high events %d",
			sc.AnonMiB, sc.CurrentMiB, sc.High, sc.Max, sc.SwapMiB, sc.SwapMax, sc.OOMKills, sc.HighEvents)
	} else {
		p("desktop scope: none found")
	}
	p("tmpfs /tmp %d MiB used  disk / %d GiB free", s.Tmp.UsedMiB, s.Root.FreeMiB/1024)
	var slots []string
	for _, sl := range s.Slots {
		if sl.Free {
			slots = append(slots, fmt.Sprintf("%d free", sl.N))
		} else {
			slots = append(slots, fmt.Sprintf("%d: %s", sl.N, truncate(sl.Holder, 60)))
		}
	}
	p("build slots: %s", strings.Join(slots, "; "))
	var cl []string
	for _, c := range s.Clusters {
		cl = append(cl, fmt.Sprintf("%s (%d node, %d MiB)", c.Name, c.Nodes, c.MemMiB))
	}
	switch {
	case s.ClustersErr != "":
		p("kind clusters: unknown (%s)", truncate(s.ClustersErr, 60))
	case len(cl) == 0:
		p("kind clusters: none")
	default:
		p("kind clusters: %s", strings.Join(cl, ", "))
	}
	p("sessions: %d CLIs, %d MiB anonymous", len(s.Sessions), s.CLIMemMiB)
	if len(s.Waits) > 0 {
		p("waits:")
		for _, w := range s.Waits {
			owner := w.Session
			if owner == "" {
				owner = "no session"
			}
			p("  %s  %s  (%s)", dur(w.Elapsed), truncate(commandName(w.Args)+" "+tailArgs(w.Args), 70), truncate(owner, 40))
		}
	}
	p("OOM kills since %s: %d", clock(a.now, s.OOMSince), len(s.OOM))
	for _, k := range groupKills(s.OOM) {
		p("  %s", k)
	}
	for _, o := range s.Oomd {
		p("  systemd-oomd: %s", truncate(o, 140))
	}
	if len(s.Leases) > 0 {
		var ls []string
		for _, l := range s.Leases {
			ls = append(ls, fmt.Sprintf("%s: %s (%s)", l.Env, truncate(l.Name, 30), l.State))
		}
		p("leases: %s", strings.Join(ls, "; "))
	}
	for _, h := range s.Holds {
		p("hold %s until %s: %s", h.Target, untilText(a, h), truncate(h.Reason, 60))
	}
	switch {
	case s.Budget != nil:
		p("GitHub: %s", budgetLine(a, *s.Budget))
	case s.BudgetErr != "":
		p("GitHub: budget unknown (%s)", truncate(s.BudgetErr, 80))
	}
	for _, c := range s.Checks {
		p("== %s", c.Name)
		if c.Output != "" {
			p("%s", c.Output)
		}
		if c.Error != "" {
			p("(%s failed: %s)", c.Name, truncate(c.Error, 120))
		}
	}
}

// tailArgs is what commandName left out, for waits: the repo and number.
func tailArgs(args string) string {
	f := strings.Fields(args)
	if len(f) <= 4 {
		return ""
	}
	return strings.Join(f[4:], " ")
}

// groupKills folds the kills of one event (46 jest workers) into one line.
func groupKills(kills []oomKill) []string {
	type group struct {
		owner string
		tasks map[string]int
		n     int
		first time.Time
	}
	var groups []*group
	idx := map[string]*group{}
	for _, k := range kills {
		g := idx[k.Owner]
		if g == nil {
			g = &group{owner: k.Owner, tasks: map[string]int{}, first: k.At}
			idx[k.Owner] = g
			groups = append(groups, g)
		}
		g.n++
		g.tasks[k.Task]++
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		var ts []string
		for t, n := range g.tasks {
			ts = append(ts, fmt.Sprintf("%s×%d", t, n))
		}
		slices.Sort(ts)
		out = append(out, fmt.Sprintf("%d from %s: %s (first %s)", g.n, g.owner, strings.Join(ts, ", "), g.first.Local().Format("15:04")))
	}
	return out
}

func budgetLine(a *app, b github.Budget) string {
	status := "ok"
	if b.Remaining < a.cfg.GitHub.Floor {
		status = "UNDER THE FLOOR: stop GitHub work"
	}
	return fmt.Sprintf("%d of %d left, resets %s (in %s); floor %d: %s",
		b.Remaining, b.Limit, clock(a.now, b.Reset), dur(b.Reset.Sub(a.now)), a.cfg.GitHub.Floor, status)
}

// diffSnapshots says what changed between two ticks, in the words a report uses.
func diffSnapshots(prev, cur *snapshot) []string {
	var out []string
	num := func(name string, p, c, threshold int, unit string) {
		if d := c - p; d >= threshold || -d >= threshold {
			out = append(out, fmt.Sprintf("%s %d → %d %s (%+d)", name, p, c, unit, d))
		}
	}
	num("RAM available", prev.Mem.AvailableMiB, cur.Mem.AvailableMiB, 2048, "MiB")
	num("swap used", prev.Mem.SwapUsedMiB, cur.Mem.SwapUsedMiB, 1024, "MiB")
	if prev.Scope != nil && cur.Scope != nil {
		num("desktop scope anon", prev.Scope.AnonMiB, cur.Scope.AnonMiB, 2048, "MiB")
		if cur.Scope.OOMKills != prev.Scope.OOMKills {
			out = append(out, fmt.Sprintf("OOM KILL in the desktop scope: %d → %d", prev.Scope.OOMKills, cur.Scope.OOMKills))
		}
	}
	num("disk / free", prev.Root.FreeMiB/1024, cur.Root.FreeMiB/1024, 10, "GiB")
	num("tmpfs /tmp", prev.Tmp.UsedMiB, cur.Tmp.UsedMiB, 2048, "MiB")
	if len(cur.OOM) > 0 {
		out = append(out, fmt.Sprintf("%d kernel OOM kills", len(cur.OOM)))
	}
	if len(cur.Oomd) > 0 {
		out = append(out, fmt.Sprintf("SYSTEMD-OOMD killed %d times", len(cur.Oomd)))
	}
	setDiff := func(name string, p, c []string) {
		added, gone := minus(c, p), minus(p, c)
		if len(added) > 0 {
			out = append(out, fmt.Sprintf("%s +%d: %s", name, len(added), strings.Join(added, ", ")))
		}
		if len(gone) > 0 {
			out = append(out, fmt.Sprintf("%s -%d: %s", name, len(gone), strings.Join(gone, ", ")))
		}
	}
	setDiff("sessions", prev.Sessions, cur.Sessions)
	setDiff("kind clusters", clusterNames(prev.Clusters), clusterNames(cur.Clusters))
	setDiff("leases", leaseKeys(prev.Leases), leaseKeys(cur.Leases))
	setDiff("waits", waitKeys(prev.Waits), waitKeys(cur.Waits))
	setDiff("holds", holdKeys(prev.Holds), holdKeys(cur.Holds))
	if prev.Budget != nil && cur.Budget != nil && prev.Budget.Reset.Equal(cur.Budget.Reset) {
		if d := prev.Budget.Remaining - cur.Budget.Remaining; d >= 250 {
			out = append(out, fmt.Sprintf("GitHub budget %d → %d (%d spent)", prev.Budget.Remaining, cur.Budget.Remaining, d))
		}
	}
	return out
}

func minus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func clusterNames(cs []machine.Cluster) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func leaseKeys(ls []leaseView) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Env + " " + l.Name
	}
	return out
}

func waitKeys(ws []wait) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = truncate(commandName(w.Args)+" "+tailArgs(w.Args), 60)
	}
	return out
}

func holdKeys(hs []state.Hold) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Target
	}
	return out
}
