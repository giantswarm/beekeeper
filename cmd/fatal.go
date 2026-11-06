package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/install"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	fatalCoreRepository    = "giantswarm/beekeeper"
	fatalProbeTimeout      = 20 * time.Second
	fatalSentinelName      = "fatal sentinel"
	fatalAsFlag            = "--as"
	fatalBudgetCommand     = "budget"
	fatalStatusError       = "error"
	fatalInstallationGuard = "guard:installation"
	fatalDisabledState     = "disabled"
	fatalEnabledState      = "enabled"
	fatalNameOption        = "--name"
	fatalDirOption         = "--dir"
	fatalBranchOption      = "--branch"
	fatalHarnessOption     = "--harness"
	fatalExcludeOption     = "--exclude"
	fatalGateOption        = "--gate"
)

var fatalRun = runArgv

var fatalSessionAlive = func(a *app, p state.Party) (bool, error) {
	sessions, _, err := a.sessions()
	if err != nil {
		return false, err
	}
	_, live := claude.Live(sessions, p)
	return live, nil
}

// fatalCmd deliberately overrides the root's configuration-dependent pre-run.
// The installed unit supplies the state directory out of band, so malformed
// configuration cannot disable detection or the ordinary-start barrier.
func (a *app) fatalCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{Use: "fatal", Short: "Detect and own system outages independently of the supervisor",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if a.managedExecutionID() != "" {
				return refused("fatal sentinel runs on the coordinator only")
			}
			return nil
		}}
	c.PersistentFlags().StringVar(&dir, "state-dir", "", "state directory (installed unit pins this independently of config)")
	resolve := func() error {
		if dir != "" {
			return nil
		}
		if err := a.loadConfig(); err != nil {
			return fmt.Errorf("fatal: pass --state-dir when configuration cannot load: %w", err)
		}
		dir = a.cfg.StateDir
		return nil
	}
	c.AddCommand(&cobra.Command{Use: "preflight <harness>", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.loadConfig(); err != nil {
				return err
			}
			// Discard parse errors: configuration can contain credential values.
			if err := a.fatalHarnessConfig(cmd.Context(), args[0]); err != nil {
				return errors.New("harness startup preflight failed")
			}
			return nil
		}})
	c.AddCommand(&cobra.Command{Use: executionStatus, Short: "Show outages, repair owners, attempts and verified outcomes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := resolve(); err != nil {
				return err
			}
			q, err := fatal.Read(dir)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(q)
		}})
	c.AddCommand(&cobra.Command{Use: "scan", Short: "Run one bounded detection and repair pass", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := resolve(); err != nil {
				return err
			}
			return a.fatalScan(cmd.Context(), dir)
		}})
	return c
}

// fatalProbe discards command output: probes may touch credentials, whose
// values must never enter outage reports, logs, or repair prompts.
func fatalProbe(ctx context.Context, argv ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, fatalProbeTimeout)
	defer cancel()
	_, _, code, err := fatalRun(ctx, "", argv)
	return code, err
}

func fatalResult(id string, code int, err error) fatal.Observation {
	o := fatal.Observation{ID: id, Evidence: fmt.Sprintf("probe exit %d", code)}
	switch {
	case err != nil:
		o.Unknown = true
		o.Evidence = "probe unavailable"
	case code == 0:
	case code == 1:
		o.Failed = true
	case code == 2:
		o.Disabled = true
	default:
		o.Unknown = true
	}
	return o
}

// Managed services deliberately disabled or uninstalled are not outages.
// A running systemctl is not sufficient: only enabled intent is monitored.
func fatalService(ctx context.Context, unit string) fatal.Observation {
	o := fatal.Observation{ID: "service:" + unit}
	ctx, cancel := context.WithTimeout(ctx, fatalProbeTimeout)
	defer cancel()
	out, _, _, err := fatalRun(ctx, "", []string{"systemctl", "--user", "is-enabled", unit})
	if err != nil {
		o.Unknown = true
		return o
	}
	switch strings.TrimSpace(out) {
	case fatalDisabledState, "masked", "not-found":
		o.Disabled = true
		return o
	case fatalEnabledState, "enabled-runtime":
	default:
		loaded, _, _, loadErr := fatalRun(ctx, "", []string{"systemctl", "--user", "show", unit, "--property=LoadState", "--value"})
		if loadErr == nil && strings.TrimSpace(loaded) == "not-found" {
			o.Disabled = true
			return o
		}
		o.Unknown = true
		return o
	}
	code, err := fatalProbe(ctx, "systemctl", "--user", "is-active", "--quiet", unit)
	o.Unknown = err != nil
	o.Failed = err == nil && code != 0
	o.Evidence = "enabled managed service is not active"
	return o
}

func (a *app) fatalScan(ctx context.Context, dir string) error {
	ctx, cancelScan := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelScan()
	if !filepath.IsAbs(dir) {
		return usageErr("fatal --state-dir must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	l := flock.New(filepath.Join(dir, "fatal.lock"))
	ok, err := l.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return refused("another fatal scan is running")
	}
	defer func() { _ = l.Unlock() }()
	q, err := fatal.Read(dir)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	recoveryPath := filepath.Join(dir, "fatal-recovery.yaml")
	store, err := state.Open(dir)
	if err != nil {
		return err
	}
	a.store = store
	st, err := store.Read()
	if err != nil {
		return err
	}
	var transitions []string
	observed := map[string]bool{}
	observe := func(o fatal.Observation) {
		observed[o.ID] = true
		if msg := q.Observe(o, now); msg != "" {
			transitions = append(transitions, msg)
			_, _ = fmt.Fprintln(a.out, msg)
		}
		if o.Unknown {
			_, _ = fmt.Fprintf(a.out, "%s: probe unavailable; health unknown\n", o.ID)
		}
	}
	path, pathErr := config.Path(a.cfgPath)
	var cfg *config.Config
	if pathErr == nil {
		cfg, pathErr = config.Load(path)
	}
	if pathErr == nil && a.cfgPath != "" {
		_, pathErr = os.Stat(path)
	}
	if pathErr != nil {
		cfg = nil
	}
	observe(fatal.Observation{ID: "configuration", Failed: pathErr != nil, Evidence: "beekeeper configuration cannot load"})
	sourcePath := path
	recovering := cfg == nil
	if cfg != nil {
		// Snapshot only validated bytes, atomically; a corrupt source never
		// replaces the settings needed to admit its own repair.
		cfg, err = config.RecoverySnapshot(path, recoveryPath)
		if err != nil {
			return err
		}
	} else if path != "" {
		cfg, _ = config.LoadRecovery(path, recoveryPath)
		if cfg != nil {
			path = recoveryPath
		}
	}
	for _, unit := range []string{"beekeeper-notify.service", "beekeeper-supervisor-loop.service", "beekeeper-timers.service"} {
		observe(fatalService(ctx, unit))
	}
	timers := fatalTimers(st, now)
	observe(fatal.Observation{ID: "timers:overdue", Failed: timers.overdue > 0, Disabled: timers.disabled, Evidence: timers.evidence()})
	broken := []string{}
	if cfg != nil {
		if cfg.StateDir != dir {
			return fmt.Errorf("fatal state directory differs from configuration; reinstall to pin it")
		}
		a.cfg = cfg
		for _, c := range cfg.Fatal.Checks {
			code, err := fatalProbe(ctx, c.Probe...)
			o := fatalResult("check:"+c.Name, code, err)
			observe(o)
			if (o.Failed || o.Unknown || o.Disabled) && c.Harness != "" {
				broken = append(broken, c.Harness)
			}
		}
		githubHeld := slices.ContainsFunc(st.Holds, func(h state.Hold) bool { return h.Target == holdGitHub && h.Active(now) })
		for _, repo := range cfg.Fatal.Repositories {
			if githubHeld {
				observe(fatal.Observation{ID: "main:" + repo, Unknown: true})
				continue
			}
			observe(fatalMain(ctx, repo, path))
		}
		seen := map[string]bool{}
		choices := cfg.Capacity.Choices
		if len(choices) == 0 {
			choices = []config.Choice{{Harness: config.ClaudeHarness}}
		}
		for _, c := range choices {
			if seen[c.Harness] {
				continue
			}
			seen[c.Harness] = true
			code, err := fatalProbe(ctx, c.Harness, "--version")
			if err == nil && code == 0 {
				var bin string
				bin, err = os.Executable()
				if err == nil {
					code, err = fatalProbe(ctx, bin, workerGitHubConfigOption, path, "fatal", "preflight", c.Harness)
				}
			}
			o := fatal.Observation{ID: "harness:" + c.Harness, Failed: err != nil || code != 0, Evidence: "configured harness fails non-model startup preflight"}
			if ctx.Err() != nil {
				o.Failed = false
				o.Unknown = true
			}
			observe(o)
			if o.Failed || o.Unknown {
				broken = append(broken, c.Harness)
			}
		}
		for _, o := range fatalGuards(dir, cfg) {
			observe(o)
			if o.Failed {
				broken = append(broken, strings.TrimPrefix(o.ID, "guard:"))
			}
		}
		for _, x := range q.Incidents {
			if !observed[x.ID] && !recovering {
				observe(fatal.Observation{ID: x.ID, Disabled: true})
			}
		}
	}
	if err := q.Save(dir); err != nil {
		return err
	}
	// Stop and holds are read afresh every pass. Detection continues, but no
	// mutation or session launch runs without the normal admission machinery.
	st, err = store.Read()
	if err != nil {
		return err
	}
	a.now = now
	for _, msg := range transitions {
		if err := store.Record(state.Event{At: now, By: state.Party{Name: fatalSentinelName}, Verb: "fatal.transition", Detail: msg}); err != nil {
			return err
		}
		// Notification is best effort; the durable status and event survive a
		// desktop that is itself unavailable.
		_, _ = fatalProbe(ctx, "notify-send", "beekeeper system health", msg)
	}
	transitions = nil
	if why := a.noStart(st); why != "" {
		_, err = fmt.Fprintln(a.out, "fatal repair held: "+why)
		for i := range q.Incidents {
			if q.Incidents[i].Resolved.IsZero() {
				q.Incidents[i].Outcome = "held by stop or starts hold"
			}
		}
		return errors.Join(err, q.Save(dir))
	}
	if cfg == nil {
		_, err = fmt.Fprintln(a.out, "fatal repair blocked: no validated recovery configuration; outage retained")
		for i := range q.Incidents {
			if q.Incidents[i].Resolved.IsZero() {
				q.Incidents[i].Outcome = "blocked: no validated recovery configuration"
			}
		}
		return errors.Join(err, q.Save(dir))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var deadOwners []state.Agent
	x := q.Reserve(now, func(name string) bool {
		for _, ag := range st.Agents {
			if ag.Name != name || ag.Done || ag.Task == "" {
				continue
			}
			// Parked work owns its outage through review/proof. An importing
			// or remote worker, or an unreadable process table, is not dead.
			if ag.Park != nil || ag.Import.Pending(now) || ag.Execution != nil {
				return true
			}
			for _, incident := range q.Incidents {
				if incident.Owner == name && now.Sub(incident.LastAttempt) < 2*time.Minute {
					return true
				}
			}
			live, err := fatalSessionAlive(a, ag.Party)
			if err != nil || live {
				return true
			}
			deadOwners = append(deadOwners, ag)
		}
		return false
	})
	if len(deadOwners) > 0 {
		if err := store.Update(func(current *state.State) ([]state.Event, error) {
			if a.noStart(current) != "" {
				return nil, errLoopLater
			}
			var events []state.Event
			for _, dead := range deadOwners {
				for i := range current.Agents {
					ag := &current.Agents[i]
					if ag.Name != dead.Name {
						continue
					}
					if ag.Done || ag.Task == "" {
						continue
					}
					if !ag.Is(dead.Party) || !ag.ActivityAt.Equal(dead.ActivityAt) || !ag.AssignedAt.Equal(dead.AssignedAt) || ag.Park != nil || ag.Import.Pending(now) || ag.Execution != nil {
						return nil, errLoopLater
					}
					ag.LastTask, ag.Task, ag.Done, ag.IdleSince = ag.Task, "", true, now
					events = append(events, state.Event{At: now, By: state.Party{Name: fatalSentinelName}, Verb: "fatal.owner-ended", Detail: dead.Name + ": repair process ended before verified recovery"})
				}
			}
			return events, nil
		}); err != nil {
			return err
		}
	}
	if x == nil {
		return q.Save(dir)
	}
	repo := fatalCoreRepository
	if mainRepo, ok := strings.CutPrefix(x.ID, "main:"); ok {
		repo = mainRepo
	}
	for _, check := range cfg.Fatal.Checks {
		if x.ID == "check:"+check.Name && check.Repository != "" {
			repo = check.Repository
		}
	}
	if _, held := activeHold(st, a, repo); held {
		x.Attempts--
		x.Owner = ""
		x.Outcome = "repair held by repository or GitHub hold"
		return q.Save(dir)
	}
	// Reserve durably before the subprocess, including ambiguous launches.
	if err := q.Save(dir); err != nil {
		return err
	}
	if unit, ok := strings.CutPrefix(x.ID, "service:"); ok {
		// Recheck intent immediately before starting. Never enable a unit.
		fresh, err := store.Read()
		if err != nil {
			return err
		}
		if a.noStart(fresh) != "" {
			x.Attempts--
			x.Owner = ""
			x.Outcome = "held before service recovery"
			return q.Save(dir)
		}
		o := fatalService(ctx, unit)
		if o.Failed && !o.Unknown && !o.Disabled {
			code, runErr := fatalProbe(ctx, "systemctl", "--user", agentStartName, unit)
			x.Outcome = fmt.Sprintf("service start exited %d", code)
			if runErr != nil {
				x.Outcome = "service start unavailable"
			}
			o = fatalService(ctx, unit)
		}
		observe(o)
		for _, msg := range transitions {
			if err := store.Record(state.Event{At: now, By: state.Party{Name: fatalSentinelName}, Verb: "fatal.transition", Detail: msg}); err != nil {
				return err
			}
			_, _ = fatalProbe(ctx, "notify-send", "beekeeper system health", msg)
		}
		_, _ = fmt.Fprintf(a.out, "%s: %s\n", x.ID, x.Outcome)
		if err := q.Save(dir); err != nil {
			return err
		}
		// A restart that did not restore health needs a code/config repair,
		// owned by a healthy harness, in the same pass rather than hours later.
		if !o.Failed || o.Unknown || o.Disabled {
			return nil
		}
	}
	brief := fmt.Sprintf("Repair fatal system outage %s in https://github.com/%s. Evidence: %s. You own this outage. Fix the component in its owning repository; a desk rule or note is an antipattern. Respect all holds, stop intent, capacity, secret rules, green CI and independent desk review. Never force-push. Do not re-enable a deliberately disabled component. Prove the failing probe passes before reporting done. Read beekeeper fatal status for the durable incident.\n", x.ID, repo, x.Evidence)
	if recovering {
		brief += fmt.Sprintf("The main configuration at %q cannot load. Admission uses validated recovery settings at %q. Repair the main file; only a successful scan of that original file clears the incident.\n", sourcePath, recoveryPath)
	}
	briefPath := filepath.Join(dir, "fatal-repair-brief.txt")
	if err := os.WriteFile(briefPath, []byte(brief), 0600); err != nil {
		return err
	}
	args := []string{workerGitHubConfigOption, path, fatalAsFlag, fatalSentinelName, agentsName, agentStartName, fatalNameOption, x.Owner}
	checkout := cfg.Fatal.Checkouts[repo]
	if checkout == "" {
		checkout = filepath.Join(cfg.Projects.Dir, repoName(repo))
	}
	args = append(args, fatalDirOption, homePath(checkout), fatalBranchOption, fmt.Sprintf("fatal/%d-%d", x.Opened.UnixNano(), x.Attempts))
	if len(cfg.Capacity.Choices) == 0 && slices.Contains(broken, config.ClaudeHarness) {
		x.Attempts--
		x.Owner = ""
		x.Outcome = "blocked: no healthy configured repair harness"
		return q.Save(dir)
	}
	if len(cfg.Capacity.Choices) > 0 {
		args = append(args, fatalHarnessOption, "auto")
		for _, harness := range slices.Compact(broken) {
			args = append(args, fatalExcludeOption, harness)
		}
	}
	args = append(args, briefPath)
	launchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, code, runErr := beekeeperRun(launchCtx, args...)
	x.Outcome = fmt.Sprintf("repair start exited %d; awaiting probe verification", code)
	if runErr == nil && (code == ExitRefused || code == 75) {
		// Admission refusals created no worker and consume no repair attempt.
		x.Attempts--
		x.Owner = ""
		x.Outcome = "waiting for normal start admission; retry after cooldown"
	}
	if runErr != nil {
		x.Outcome = "repair launch uncertain; reserved owner retained"
	}
	_, _ = fmt.Fprintf(a.out, "%s: owner %q; attempt %d/3; %s\n", x.ID, x.Owner, x.Attempts, x.Outcome)
	if err := store.Record(state.Event{At: now, By: state.Party{Name: fatalSentinelName}, Verb: "fatal.repair", Detail: fmt.Sprintf("%s: owner %q; attempt %d; %s", x.ID, x.Owner, x.Attempts, x.Outcome)}); err != nil {
		return err
	}
	_, _ = fatalProbe(ctx, "notify-send", "beekeeper system repair", x.ID+": "+x.Outcome)
	return q.Save(dir)
}

// fatalMain checks only completed failing checks on the current default-branch
// HEAD. Pending CI and unavailable GitHub are never called red or healthy.
func fatalMain(ctx context.Context, repo, path string) fatal.Observation {
	o := fatal.Observation{ID: "main:" + repo}
	ctx, cancel := context.WithTimeout(ctx, fatalProbeTimeout)
	defer cancel()
	bin, err := os.Executable()
	if err != nil {
		o.Unknown = true
		return o
	}
	_, _, code, err := fatalRun(ctx, "", []string{bin, workerGitHubConfigOption, path, fatalAsFlag, fatalSentinelName, fatalBudgetCommand, fatalGateOption})
	if err != nil || code != 0 {
		o.Unknown = true
		return o
	}
	out, _, code, err := fatalRun(ctx, "", []string{"gh", workerGitHubAPIGroup, "repos/" + repo + "/commits/HEAD/status?per_page=100"})
	if err != nil || code != 0 {
		o.Unknown = true
		return o
	}
	var status struct {
		SHA   string `json:"sha"`
		State string `json:"state"`
		Total int    `json:"total_count"`
	}
	if json.Unmarshal([]byte(out), &status) != nil || status.SHA == "" {
		o.Unknown = true
		return o
	}
	if status.Total > 0 {
		switch status.State {
		case "failure", fatalStatusError:
			o.Failed = true
			o.Evidence = "https://github.com/" + repo + "/commit/" + status.SHA
		case "success":
		default:
			o.Unknown = true
		}
	}
	out, _, code, err = fatalRun(ctx, "", []string{"gh", workerGitHubAPIGroup, "repos/" + repo + "/commits/" + status.SHA + "/check-runs?per_page=100"})
	if err != nil || code != 0 {
		if !o.Failed {
			o.Unknown = true
		}
		return o
	}
	// Explicit wire fields keep unknown conclusions from becoming healthy.
	var wire struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			URL        string `json:"html_url"`
		} `json:"check_runs"`
	}
	if json.Unmarshal([]byte(out), &wire) != nil {
		if !o.Failed {
			o.Unknown = true
		}
		return o
	}
	if len(wire.Runs) == 0 && status.Total == 0 {
		o.Unknown = true
	}
	if wire.Total > len(wire.Runs) {
		o.Unknown = true
	}
	for _, r := range wire.Runs {
		switch r.Conclusion {
		case "failure", "cancelled", "timed_out", "startup_failure", "action_required":
			o.Failed = true
			o.Evidence = r.URL
		case "success", "neutral", "skipped":
		default:
			o.Unknown = true
		}
	}
	if o.Failed {
		o.Unknown = false
	}
	return o
}

// Installation ownership, rather than executable presence, determines whether
// hooks are desired. Uninstall withdraws that intent in install.json.
func fatalGuards(dir string, cfg *config.Config) []fatal.Observation {
	b, err := os.ReadFile(filepath.Join(dir, "install.json")) //nolint:gosec // fixed manifest basename in the pinned state directory
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var m install.Manifest
	if err != nil || json.Unmarshal(b, &m) != nil {
		return []fatal.Observation{{ID: fatalInstallationGuard, Unknown: true}}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return []fatal.Observation{{ID: fatalInstallationGuard, Unknown: true}}
	}
	exe, err := os.Executable()
	if err != nil {
		return []fatal.Observation{{ID: fatalInstallationGuard, Unknown: true}}
	}
	var out []fatal.Observation
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	for _, c := range []struct {
		name, path string
		desired    bool
		check      func(string, string, string) string
	}{
		{"claude", filepath.Join(claudeDir, "settings.json"), len(m.Hooks) > 0, install.CheckClaudeHooks},
		{config.AgyHarness, filepath.Join(home, ".gemini", "config", "hooks.json"), len(m.AgyHook) > 0, install.CheckAgyHooks},
		{"codex", codexHooksPath(home, true, cfg.Codex), len(m.CodexHook) > 0, func(_ string, exe, home string) string { return checkCodexHooks(home, dir, exe, true, cfg.Codex) }},
	} {
		o := fatal.Observation{ID: "guard:" + c.name, Disabled: !c.desired}
		if c.desired {
			o.Failed = c.check(c.path, exe, home) != ""
			o.Evidence = "installed guard configuration fails validation"
		}
		out = append(out, o)
	}
	return out
}
