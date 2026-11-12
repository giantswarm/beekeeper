package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/notify"
)

func userHomeDir() (string, error) {
	if h := os.Getenv("BEEKEEPER_EXECUTION_HOME"); filepath.IsAbs(h) {
		return filepath.Clean(h), nil
	}
	return os.UserHomeDir()
}

func (c *Config) defaults() error {
	setDur(&c.Delivery.StallAfter, 24*time.Hour)
	if c.Delivery.EpicParallel == 0 {
		c.Delivery.EpicParallel = 3
	}
	home, err := userHomeDir()
	if err != nil {
		return err
	}
	setStr(&c.Projects.Dir, filepath.Join(home, "projects"))
	c.Projects.Dir = filepath.Clean(homePath(home, c.Projects.Dir))
	if c.Projects.Worktrees == nil {
		c.Projects.Worktrees = []string{filepath.Join(c.Projects.Dir, "worktrees")}
	}
	if c.Home.Places == nil {
		c.Home.Places = []string{filepath.Join(home, ".local", "state")}
	}
	for _, places := range [][]string{c.Projects.Worktrees, c.Home.Places} {
		for i, p := range places {
			places[i] = filepath.Clean(homePath(home, p))
		}
	}
	if c.Agents.Roots == nil {
		c.Agents.Roots = slices.Clone(c.Projects.Worktrees)
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	setStr(&c.StateDir, filepath.Join(state, "beekeeper"))
	if c.Fatal.Repositories == nil {
		c.Fatal.Repositories = []string{"giantswarm/beekeeper"}
	}
	setStr(&c.Disk.Prune, DiskPruneAuto)
	if c.Disk.TrashDirs == nil {
		c.Disk.TrashDirs = []string{filepath.Join(c.StateDir, "trash"), filepath.Join(state, "trash")}
	}
	for i, p := range c.Disk.TrashDirs {
		c.Disk.TrashDirs[i] = filepath.Clean(homePath(home, p))
	}
	setDur(&c.Disk.Every, time.Hour)
	setInt(&c.Disk.SccacheMaxGiB, 10)
	h := &c.Disk.Holders
	// The state home is stateDir's parent; a stateDir right under the
	// filesystem root is its own home, never the root itself.
	root := filepath.Dir(c.StateDir)
	if filepath.Clean(root) == string(filepath.Separator) {
		root = c.StateDir
	}
	setStr(&h.Root, root)
	h.Root = filepath.Clean(homePath(home, h.Root))
	setDur(&h.StaleAfter, 72*time.Hour)
	setDur(&h.AsideRetention, 7*24*time.Hour)
	if h.AsideDirs == nil {
		h.AsideDirs = []string{"aside", "moved-aside"}
	}
	setInt(&h.WarnGiB, 10)
	setDur(&h.Every, 6*time.Hour)
	for i, p := range c.Disk.Checkouts {
		c.Disk.Checkouts[i] = filepath.Clean(homePath(home, p))
	}
	for i, e := range c.Disk.Caches {
		if e.Path != "" {
			c.Disk.Caches[i].Path = filepath.Clean(homePath(home, e.Path))
		}
		if e.Name == "" {
			c.Disk.Caches[i].Name = c.Disk.Caches[i].Path
		}
	}
	for i, o := range c.Disk.BuildOutputs {
		c.Disk.BuildOutputs[i] = BuildOutput{Path: filepath.Clean(homePath(home, o.Path)), Worktree: filepath.Clean(homePath(home, o.Worktree))}
	}
	setStr(&c.LeaseDir, filepath.Join(c.StateDir, "leases"))
	setDur(&c.GrantTTL, 30*time.Minute)
	setDur(&c.LeaseRound, time.Hour)
	if c.Identity.Host == "" {
		if h, err := os.Hostname(); err == nil {
			c.Identity.Host, _, _ = strings.Cut(h, ".")
		}
	}
	if c.Supervisor.ReviewRounds == 0 {
		c.Supervisor.ReviewRounds = 1
	}
	if c.Supervisor.UnmergedLimit == 0 {
		c.Supervisor.UnmergedLimit = DefaultUnmergedLimit
	}
	if c.Supervisor.GraphQLLimit == 0 {
		c.Supervisor.GraphQLLimit = DefaultLoopGraphQLLimit
	}
	if c.Supervisor.ClassifierConfidence == 0 {
		c.Supervisor.ClassifierConfidence = 0.8
	}
	setDur(&c.Supervisor.LoopInterval, 30*time.Second)
	setDur(&c.Supervisor.ReleaseEvery, 10*time.Minute)
	setDur(&c.Supervisor.PostMergeWindow, 2*time.Hour)
	setDur(&c.Supervisor.FillRetryEvery, 5*time.Minute)
	setDur(&c.Supervisor.ReserveFor, DefaultReserveFor)
	setDur(&c.Supervisor.UnmergedWindow, DefaultUnmergedWindow)
	setDur(&c.Supervisor.NoteDue, 24*time.Hour)
	if c.Supervisor.Ownership.Team != "" {
		setStr(&c.Supervisor.Ownership.TeamFiles, DefaultTeamFiles)
	}
	setDur(&c.Supervisor.Ownership.OwnerWait, 24*time.Hour)
	c.Supervisor.ReviewBrief = homePath(home, c.Supervisor.ReviewBrief)
	c.Supervisor.WorkerBrief = homePath(home, c.Supervisor.WorkerBrief)
	setStr(&c.Serve.Gateway.TokenFile, "/var/run/secrets/klaus-gateway/token")
	c.Serve.Gateway.TokenFile = homePath(home, c.Serve.Gateway.TokenFile)
	setStr(&c.Serve.Gateway.AnswerTool, "x_beekeeper_note_answer")
	setStr(&c.Serve.Gateway.SendTool, "x_beekeeper_send_message")
	setStr(&c.Serve.Identity.TokenFile, "/var/run/secrets/dex/token")
	c.Serve.Identity.TokenFile = homePath(home, c.Serve.Identity.TokenFile)
	setStr(&c.Central.Server, "beekeeper")
	setStr(&c.Central.Muster, "muster")
	setDur(&c.Central.Timeout, 30*time.Second)
	setStr(&c.Feedback.Server, "slack")
	setDur(&c.Feedback.Every, 5*time.Minute)
	setDur(&c.Feedback.Window, 24*time.Hour)
	// The guide's context is the person's conversation: it relays early.
	if c.Guide.RelayAt == 0 {
		c.Guide.RelayAt = 200_000
	}
	c.Guide.defaults(home)
	setDur(&c.Guide.WaitingTTL, 2*time.Hour)
	if c.Agents.RelayAt == 0 {
		c.Agents.RelayAt = defaultRelayAt
	}
	setDur(&c.Agents.LastStepGrace, 30*time.Minute)
	if c.Agents.LastStepCeiling == 0 {
		c.Agents.LastStepCeiling = c.Agents.RelayAt + c.Agents.RelayAt/4
	}
	setDur(&c.Agents.NoteWait, 3*time.Minute)
	setDur(&c.Agents.StaleAfter, 24*time.Hour)
	if c.Agents.Shell.Unalias == nil {
		c.Agents.Shell.Unalias = DefaultUnalias
	}
	setStr(&c.Agents.Shell.Globs, GlobsLiteral)
	setStr(&c.Agents.Mode, ModeDesktop)
	c.Reporter.defaults(home, c.Guide.Person)
	c.Outbound.defaults(home)
	for i, d := range c.Hooks.Scope.Dirs {
		c.Hooks.Scope.Dirs[i] = filepath.Clean(homePath(home, d))
	}
	for i, d := range c.Hooks.PersonFolders {
		if d == "~" {
			d = home
		}
		c.Hooks.PersonFolders[i] = filepath.Clean(homePath(home, d))
	}
	setInt(&c.Scan.MinLength, 12)
	c.Secret.TokenFile = homePath(home, c.Secret.TokenFile)
	for i, f := range c.Secret.Files {
		c.Secret.Files[i] = homePath(home, f)
	}
	for i, f := range c.Secret.FileRoots {
		c.Secret.FileRoots[i] = filepath.Clean(homePath(home, f))
	}
	for i, f := range c.Kube.Kubeconfig {
		c.Kube.Kubeconfig[i] = homePath(home, f)
	}
	for i, f := range c.Alerts.Kubeconfig {
		c.Alerts.Kubeconfig[i] = homePath(home, f)
	}
	for in, s := range c.Apps.Secrets {
		s.File = homePath(home, s.File)
		c.Apps.Secrets[in] = s
	}
	c.Sandbox.defaults(home)
	c.Run.Remote.Kubeconfig = homePath(home, c.Run.Remote.Kubeconfig)
	for i := range c.Scan.SOPS {
		c.Scan.SOPS[i] = homePath(home, c.Scan.SOPS[i])
	}
	setStr(&c.Plans.Check, DefaultPlansCheck)
	c.Sink.defaults()
	c.Teleport.defaults(home)
	if b := c.Teleport.Bot; b.Enabled() {
		c.Kube.Kubeconfig = append([]string{b.Kubeconfig}, c.Kube.Kubeconfig...)
		c.Alerts.Kubeconfig = append([]string{b.Kubeconfig}, c.Alerts.Kubeconfig...)
	}
	setStr(&c.Shell, os.Getenv("SHELL"))
	setStr(&c.Shell, "sh")

	setDur(&c.Overlaps.ActiveWithin, time.Hour)
	setDur(&c.Board.StaleAfter, 365*24*time.Hour)
	setDur(&c.Board.ArchiveAfter, DefaultBoardArchiveAfter)
	setDur(&c.Board.TidyEvery, time.Hour)
	setDur(&c.Board.EscalateEvery, 7*24*time.Hour)
	setDur(&c.Board.Claims.Refresh, 6*time.Hour)
	setDur(&c.Board.Claims.StaleAfter, 24*time.Hour)

	setInt(&c.GitHub.Floor, 2500)
	setInt(&c.GitHub.UrgentBound, 200)
	setStr(&c.GitHub.ProbeRepo, "giantswarm/beekeeper")
	if c.GitHub.AllowedOrgs == nil {
		c.GitHub.AllowedOrgs = []string{"giantswarm"}
	}

	w := &c.Watch
	setDur(&w.Interval, 30*time.Second)
	setDur(&w.BatchInterval, 5*time.Minute)
	setDur(&w.Repeat, 10*time.Minute)
	setDur(&w.BudgetEvery, 5*time.Minute)
	setDur(&w.ThroughputAfter, 2*time.Hour)
	setDur(&w.SwapWindow, 10*time.Minute)
	setDur(&w.OOMDWithin, 30*time.Minute)
	setDur(&w.DiskFillWithin, 2*time.Hour)
	setInt(&c.Doctor.GoCacheMaxGiB, 20)
	setDur(&c.Doctor.GoCacheEvery, time.Hour)
	setDur(&c.Doctor.FreeEvery, time.Hour)
	setInt(&c.Doctor.DockerImagesMaxGiB, 40)
	setInt(&c.Doctor.DockerBuildCacheMaxGiB, 20)
	setDur(&c.Doctor.DockerEvery, time.Hour)
	setDur(&c.Doctor.WorktreesEvery, 6*time.Hour)
	if c.Doctor.WorktreeRoots == nil {
		c.Doctor.WorktreeRoots = []string{"~/.local/state/beekeeper/worktrees"}
	}
	for i, r := range c.Doctor.WorktreeRoots {
		c.Doctor.WorktreeRoots[i] = filepath.Clean(homePath(home, r))
	}
	setStr(&c.Ollama.Unit, "ollama")
	setInt(&c.Ollama.BudgetGiB, 12)
	setInt(&c.Ollama.MaxBudgetGiB, 24)
	if c.Ollama.LabTests == nil {
		c.Ollama.LabTests = []string{"models-test"}
	}
	if w.LoadPerCoreMax == 0 {
		w.LoadPerCoreMax = 1.5
	}
	if w.PSIMax == 0 {
		w.PSIMax = 10
	}
	if w.CPUPSIMax == 0 {
		w.CPUPSIMax = 40
	}
	if w.ForkRateMax == 0 {
		w.ForkRateMax = 50
	}
	setInt(&w.StackMax, 3)
	setInt(&w.ToolProcsMax, 1000)
	if w.Tools == nil {
		w.Tools = []string{"kubectl", "helm", "tsh", "gh", "flux", "devctl"}
	}

	setStr(&c.Claude.ProjectsDir, filepath.Join(home, ".claude", "projects"))
	setStr(&c.Claude.DesktopApp, DefaultDesktopApp)
	setStr(&c.Claude.SessionsDir, filepath.Join(home, ".claude", "sessions"))
	setStr(&c.Omp.SessionsDir, filepath.Join(home, ".omp", "agent", "sessions"))
	setStr(&c.Omp.ModelsFile, filepath.Join(home, ".omp", "agent", "models.yml"))
	c.Omp.SessionsDir = homePath(home, c.Omp.SessionsDir)
	c.Omp.ModelsFile = homePath(home, c.Omp.ModelsFile)
	setStr(&c.Agy.BrainDir, filepath.Join(home, ".gemini", "antigravity-cli", "brain"))
	setStr(&c.Agy.ProjectsDir, filepath.Join(home, ".gemini", "config", "projects"))
	setStr(&c.Agy.ConversationsDir, filepath.Join(home, ".gemini", "antigravity-cli", "conversations"))
	c.Agy.BrainDir = homePath(home, c.Agy.BrainDir)
	c.Agy.ProjectsDir = homePath(home, c.Agy.ProjectsDir)
	c.Agy.ConversationsDir = homePath(home, c.Agy.ConversationsDir)
	setStr(&c.Codex.SessionsDir, filepath.Join(home, ".codex", "sessions"))
	setStr(&c.Codex.AuthFile, filepath.Join(home, ".codex", "auth.json"))
	setStr(&c.Codex.ConfigFile, filepath.Join(home, ".codex", "config.toml"))
	c.Codex.SessionsDir = homePath(home, c.Codex.SessionsDir)
	c.Codex.AuthFile = homePath(home, c.Codex.AuthFile)
	c.Codex.ConfigFile = homePath(home, c.Codex.ConfigFile)
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	setStr(&c.Claude.DesktopDir, filepath.Join(cfg, "Claude", "claude-code-sessions"))
	setStr(&c.Claude.DesktopLog, filepath.Join(cfg, "Claude", "logs", "main.log"))
	setDur(&c.Desktop.TypingQuiet, 30*time.Second)

	if c.Metrics.Models == nil {
		c.Metrics.Models = map[string]Model{}
	}
	for k, v := range DefaultModels {
		if _, ok := c.Metrics.Models[k]; !ok {
			c.Metrics.Models[k] = v
		}
	}
	rw := &c.Metrics.Runaway
	setInt(&rw.GitHubCallsPerHour, 1000)
	setInt(&rw.SameErrorRepeats, 10)
	if rw.ContextFill == 0 {
		rw.ContextFill = 0.9
	}

	setInt(&c.Merge.Cap, 5)
	setInt(&c.Merge.SessionCap, 2)
	setDur(&c.Merge.QueueTTL, DefaultQueueTTL)
	setDur(&c.Merge.SeedTTL, 12*time.Hour)
	setDur(&c.Merge.Settle, 5*time.Minute)
	setDur(&c.Merge.SettleTimeout, 30*time.Minute)
	setDur(&c.Merge.BudgetFresh, time.Minute)
	setDur(&c.Merge.ResetFirst, 5*time.Minute)
	setDur(&c.Merge.StallAfter, 5*time.Minute)
	setDur(&c.Merge.HungAfter, 45*time.Minute)
	setDur(&c.Merge.CITimeout, 2*time.Hour)
	setDur(&c.Merge.BusyBudget, 4*time.Hour)
	setDur(&c.Merge.ToolLimit, 9*time.Minute)
	setDur(&c.Secret.UnlockWait, 8*time.Minute)
	setDur(&c.Secret.SessionLifetime, 12*time.Hour)

	setStr(&c.Memcap.SlotDir, filepath.Join(state, "memcap", "slots"))
	setInt(&c.Memcap.Slots, 2)
	setInt(&c.Memcap.CPUWeight, DefaultMemcapCPUWeight)
	setStr(&c.Run.Tail, DefaultRunTail)
	setDur(&c.Memcap.HoldBound, 30*time.Minute)
	if !c.Capacity.reviewHeadroomSet {
		c.Capacity.ReviewHeadroom = 2
	}
	if !c.Capacity.ceilingSet && c.Capacity.Ceiling == 0 {
		c.Capacity.Ceiling = DefaultCeiling
	}
	c.Capacity.Usage.defaults(home)
	for _, name := range slices.Sorted(maps.Keys(c.Capacity.Providers)) {
		p := c.Capacity.Providers[name]
		if p.SelfHosted() {
			// A self-hosted provider has no subscription to keep on pace:
			// capacity.ceiling, which a desk lowers to hold one on its week,
			// does not set its target. Without a ceiling of its own it runs
			// DefaultSelfHostedCeiling sessions at once, bounded by the
			// machine guards.
			if !p.ceilingSet {
				p.Ceiling = DefaultSelfHostedCeiling
			}
			for i := range p.ServeModeCommand {
				p.ServeModeCommand[i] = homePath(home, p.ServeModeCommand[i])
			}
		} else if !p.ceilingSet && p.Ceiling == 0 {
			p.Ceiling = c.Capacity.Ceiling
		}
		if name == ClaudeHarness && len(p.Command) == 0 {
			p.Command = slices.Clone(DefaultClaudeUsageCommand)
		}
		if name == AgyHarness && len(p.Command) == 0 && !p.SelfHosted() {
			p.Command = slices.Clone(DefaultAgyUsageCommand)
		}
		if name == CodexHarness && len(p.Command) == 0 {
			p.Command = slices.Clone(DefaultCodexUsageCommand)
		}
		p.defaults(home)
		c.Capacity.Providers[name] = p
	}
	setInt(&c.Capacity.AvailMinMiB, 20<<10)
	setInt(&c.Capacity.SwapGrowthMaxMiB, 256)
	al := &c.Alerts
	if al.Ignore == nil {
		al.Ignore = slices.Clone(DefaultIgnore)
	}
	if al.Quiet == nil {
		al.Quiet = DefaultQuietFor(al.Team)
	}
	if c.Watch.QuietSessions == nil {
		c.Watch.QuietSessions = slices.Clone(DefaultQuietSessions)
	}
	setInt(&al.Collapse, 3)
	setDur(&al.Every, 5*time.Minute)
	setDur(&al.Timeout, time.Minute)
	setStr(&al.Kubectl, "kubectl")
	setInt(&al.Flap.Changes, 4)
	setDur(&al.Flap.Window, time.Hour)
	setStr(&al.PageSeverity, alerts.Page)
	setDur(&al.OwnerGrace, 15*time.Minute)
	setStr(&al.PagerDuty.Server, "pd")
	setDur(&al.PagerDuty.Every, time.Minute)
	setStr(&al.Discover.Label, "application.giantswarm.io/team")
	if al.Discover.Exclude == nil {
		al.Discover.Exclude = []string{"*-*", "*.*"}
	}
	setDur(&c.Upgrades.Every, 5*time.Minute)
	if c.Notify.Kinds == nil {
		c.Notify.Kinds = slices.Clone(notify.Kinds)
	}
	setDur(&c.Notify.Repeat, 30*time.Minute)
	return nil
}

func homePath(home, p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return p
}

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}

func setDur(p *Duration, v time.Duration) {
	if p.Duration == 0 {
		p.Duration = v
	}
}
