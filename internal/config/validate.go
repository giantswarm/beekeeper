package config

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/execution"
)

// validate keeps the loader's first-finding contract. The checker uses
// validateEach to collect independent rules without repeated edit/check rounds.
func (c *Config) validate() error { return firstFinding(c.validateEach) }

func firstFinding(visit func(func(error))) error {
	var first error
	visit(func(err error) {
		if first == nil {
			first = err
		}
	})
	return first
}

func (c *Config) validateEach(report func(error)) {
	for _, owner := range c.GitHub.PublicOnlyOwners {
		if owner == "" || strings.TrimSpace(owner) != owner || strings.ContainsAny(owner, "/ *?\\\t\r\n") {
			report(fmt.Errorf("github.publicOnlyOwners: invalid owner %q", owner))
		}
	}
	if c.Delivery.StallAfter.Duration < 0 {
		report(fmt.Errorf("delivery.stallAfter must be positive"))
	}
	if c.Delivery.EpicLimit < 0 {
		report(fmt.Errorf("delivery.epicLimit must be nonnegative"))
	}
	if c.Delivery.EpicParallel < 0 {
		report(fmt.Errorf("delivery.epicParallel must be positive"))
	}
	if c.Delivery.WIPLimit < 0 {
		report(fmt.Errorf("delivery.wipLimit must be nonnegative"))
	}
	// These intervals drive recurring loops; zero receives a default, while
	// negative values cannot advance the scheduler.
	for _, interval := range []struct {
		key   string
		value time.Duration
	}{
		{"watch.interval", c.Watch.Interval.Duration},
		{"alerts.every", c.Alerts.Every.Duration},
		{"alerts.pagerduty.every", c.Alerts.PagerDuty.Every.Duration},
		{"supervisor.loopInterval", c.Supervisor.LoopInterval.Duration},
		{"supervisor.unmergedWindow", c.Supervisor.UnmergedWindow.Duration},
	} {
		if interval.value < 0 {
			report(fmt.Errorf("%s: %s is negative", interval.key, interval.value))
		}
	}

	for key, paths := range map[string][]string{"projects.dir": {c.Projects.Dir}, "projects.worktrees": c.Projects.Worktrees, "home.places": c.Home.Places} {
		for _, p := range paths {
			if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) == "/") {
				report(fmt.Errorf("%s: %q: want an absolute directory below the filesystem root (~/ allowed)", key, p))
			}
		}
	}

	if c.LeaseRound.Duration < 0 {
		report(errors.New("leaseRound: want a positive duration"))
	}
	for key, level := range map[string]string{"guide.effort": c.Guide.Effort, "supervisor.effort": c.Supervisor.Effort} {
		if !ValidEffort(level) {
			report(fmt.Errorf("%s: invalid effort %q (low, medium, high, xhigh, max)", key, level))
		}
	}
	for i, r := range c.Agents.Models {
		if r.Harness != "" && !slices.Contains([]string{ClaudeHarness, CodexHarness, AgyHarness, "omp"}, r.Harness) {
			report(fmt.Errorf("agents.models[%d].harness: unknown harness %q", i, r.Harness))
		}
		if !ValidEffort(strings.TrimSpace(r.Effort)) || r.Harness == AgyHarness && r.Effort != "" {
			report(fmt.Errorf("agents.models[%d].effort: invalid effort %q (low, medium, high, xhigh, max)", i, r.Effort))
		}
		if _, err := regexp.Compile(r.Match); err != nil {
			report(fmt.Errorf("agents.models[%d].match: %w", i, err))
		}
		if strings.TrimSpace(r.Match) == "" {
			report(fmt.Errorf("agents.models[%d]: match is required", i))
		}
	}
	seenKinds := map[string]bool{}
	for i, k := range c.Agents.Kinds {
		if !profileNamePattern.MatchString(k.Name) {
			report(fmt.Errorf("agents.kinds[%d].name: want letters, digits, - or _", i))
		} else if seenKinds[k.Name] {
			report(fmt.Errorf("agents.kinds[%d].name: %s is named twice", i, k.Name))
		}
		seenKinds[k.Name] = true
		if strings.TrimSpace(k.Match) == "" {
			report(fmt.Errorf("agents.kinds[%d]: match is required", i))
		} else if _, err := regexp.Compile(k.Match); err != nil {
			report(fmt.Errorf("agents.kinds[%d].match: %w", i, err))
		}
		if k.MaxBusy < 0 || k.MinInterval.Duration < 0 || k.DailySpend < 0 {
			report(fmt.Errorf("agents.kinds[%d]: maxBusy, minInterval and dailySpend must not be negative", i))
		}
	}
	for kind, p := range c.Capacity.TaskPolicies {
		if strings.TrimSpace(kind) == "" || p.MinClass < 0 || p.MaxCost < 0 {
			report(fmt.Errorf("capacity.taskPolicies.%s: nonempty kind and nonnegative class and cost required", kind))
		}
	}
	if err := c.Run.Remote.Validate(); err != nil {
		report(err)
	}
	if c.Run.Tail != "" && c.Run.Tail != "off" && !runTailRe.MatchString(c.Run.Tail) {
		report(fmt.Errorf("run.tail: want a size with an optional K, M, G or T suffix (256K) or off"))
	}
	if c.Disk.Prune != "" && c.Disk.Prune != DiskPruneAuto && c.Disk.Prune != DiskPruneAsk {
		report(fmt.Errorf("disk.prune: want auto or ask"))
	}
	for _, p := range c.Disk.TrashDirs {
		if !filepath.IsAbs(p) || filepath.Clean(p) == string(filepath.Separator) {
			report(fmt.Errorf("disk.trashDirs: each entry must be an absolute trash directory, not a filesystem root"))
		}
	}
	for _, n := range c.Disk.BuildDirs {
		if n == "" || n == "." || filepath.IsAbs(n) || strings.Contains(n, "..") || strings.Contains(n, ".git") {
			report(fmt.Errorf("disk.buildDirs: invalid directory %q", n))
		}
	}
	if r := c.Disk.Holders.Root; r != "" && (!filepath.IsAbs(r) || filepath.Clean(r) == string(filepath.Separator)) {
		report(fmt.Errorf("disk.holders.root: must be an absolute directory, not a filesystem root"))
	}
	if h := c.Disk.Holders; h.StaleAfter.Duration < 0 || h.AsideRetention.Duration < 0 || h.Every.Duration < 0 {
		report(fmt.Errorf("disk.holders: staleAfter, asideRetention and every take no negative duration"))
	}
	for _, n := range append(slices.Clone(c.Disk.Holders.AsideDirs), c.Disk.Holders.Keep...) {
		if n == "" || n == "." || strings.ContainsRune(n, filepath.Separator) || strings.Contains(n, "..") {
			report(fmt.Errorf("disk.holders: %q is no entry name of the root", n))
		}
	}
	validateDiskCachesEach(c.Disk.Caches, report)
	for _, o := range c.Disk.BuildOutputs {
		if !filepath.IsAbs(o.Path) || !filepath.IsAbs(o.Worktree) || filepath.Clean(o.Path) == string(filepath.Separator) || o.Path == o.Worktree || strings.HasPrefix(o.Worktree, filepath.Clean(o.Path)+string(filepath.Separator)) {
			report(fmt.Errorf("disk.buildOutputs: output and owning worktree must be distinct absolute paths"))
		}
	}
	if err := c.GitHub.WorkerApp.Validate(); err != nil {
		report(err)
	}
	if err := execution.ValidateMCP(c.Agents.RemoteMCP); err != nil {
		report(fmt.Errorf("agents.remoteMCP: %w", err))
	}
	if b := c.Agents.PolicyBundle; b != nil && (!filepath.IsAbs(b.Root) || len(b.Sources) == 0) {
		report(fmt.Errorf("agents.policyBundle: an absolute root and at least one source are required"))
	}
	identities := map[execution.HostID]bool{}
	for selector, host := range c.Agents.Hosts {
		if selector == "local" || !execution.ValidIdentity(selector) || identities[host.Identity] {
			report(errors.New("agents.hosts requires unique identities and selectors; local is reserved"))
		}
		if err := host.ValidateCatalog(); err != nil {
			report(fmt.Errorf("agents.hosts.%s: %w", selector, err))
		}
		identities[host.Identity] = true
	}
	if c.Agents.Execution != nil {
		if err := c.Agents.Execution.Validate(); err != nil {
			report(fmt.Errorf("agents.execution: %w", err))
		}
	}
	for name := range c.Agents.Profiles {
		if !profileNamePattern.MatchString(name) {
			report(fmt.Errorf("agents.profiles: %q is no profile name (letters, digits, - and _)", name))
		}
	}
	for key, name := range map[string]string{"agents.profile": c.Agents.Profile, "guide.profile": c.Guide.Profile} {
		if _, ok := c.Agents.ProfileFor(name); name != "" && !ok {
			report(fmt.Errorf("%s: %q is no profile: name one of agents.profiles or %s", key, name, MinimalProfile))
		}
	}
	if m := c.Agents.Mode; m != "" && m != ModeDesktop && m != ModeBackground {
		report(fmt.Errorf("agents.mode: %q: want %s or %s", m, ModeDesktop, ModeBackground))
	}
	if g := c.Agents.Shell.Globs; g != "" && g != GlobsLiteral && g != GlobsShell {
		report(fmt.Errorf("agents.shell.globs: %q: want %s or %s", g, GlobsLiteral, GlobsShell))
	}
	for _, n := range c.Agents.Shell.Unalias {
		if !commandName.MatchString(n) {
			report(fmt.Errorf("agents.shell.unalias: %q is no command name", n))
		}
	}
	for _, n := range c.Secret.UnlockCommands {
		if !commandName.MatchString(n) {
			report(fmt.Errorf("secret.unlockCommands: %q is no command name", n))
		}
	}
	for _, f := range c.Secret.Files {
		if _, err := filepath.Match(f, ""); err != nil || !filepath.IsAbs(f) {
			report(fmt.Errorf("secret.files: %q is no absolute path or glob (~/ allowed)", f))
		}
	}
	for i, id := range c.Secret.AgeIdentities {
		switch {
		case id.Recipient == "" && id.PathRegex == "":
			report(fmt.Errorf("secret.ageIdentities[%d]: name a recipient or a pathRegex", i))
		case id.Recipient != "" && !strings.HasPrefix(id.Recipient, "age1"):
			report(fmt.Errorf("secret.ageIdentities[%d]: recipient %q is no age recipient (age1…)", i, id.Recipient))
		case !strings.HasPrefix(id.Ref, "op://") && !strings.HasPrefix(id.Ref, "file:///") && !strings.HasPrefix(id.Ref, "store://"):
			report(fmt.Errorf("secret.ageIdentities[%d]: ref %q: want op://<vault>/<item>/<field>, file:///<absolute path> or store://[<entry>]", i, id.Ref))
		}
		if _, err := regexp.Compile(id.PathRegex); err != nil {
			report(fmt.Errorf("secret.ageIdentities[%d]: pathRegex: %w", i, err))
		}
		if rest, ok := strings.CutPrefix(id.Ref, "op://"); ok {
			if vault, _, _ := strings.Cut(rest, "/"); vault != c.Secret.Vault && !slices.Contains(c.Secret.AgeVaults, vault) {
				report(fmt.Errorf("secret.ageIdentities[%d]: ref %q: the vault %q is neither secret.vault nor one of secret.ageVaults", i, id.Ref, vault))
			}
		}
	}
	for _, in := range slices.Sorted(maps.Keys(c.Apps.Secrets)) {
		s := c.Apps.Secrets[in]
		switch {
		case !filepath.IsAbs(s.File):
			report(fmt.Errorf("apps.secrets.%s.file: %q is no absolute path (~/ allowed)", in, s.File))
		case (s.Name == "") != (s.Namespace == ""):
			report(fmt.Errorf("apps.secrets.%s: name and namespace start an absent file's Secret together", in))
		}
	}
	for i, v := range c.Secret.AgeVaults {
		if v == "" || strings.Contains(v, "/") || c.Secret.Vault == "" {
			report(fmt.Errorf("secret.ageVaults[%d]: %q: a vault's name, beside secret.vault", i, v))
		}
	}
	for i, r := range c.Secret.FileRoots {
		if !filepath.IsAbs(r) || r == "/" {
			report(fmt.Errorf("secret.fileRoots[%d]: %q: a directory's absolute path (~/ allowed), not the root", i, r))
		}
	}
	for i, v := range c.Secret.MoveVaults {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "/\x00\r\n") || c.Secret.Vault == "" {
			report(fmt.Errorf("secret.moveVaults[%d]: %q: a vault name requires secret.vault", i, v))
		}
	}
	for i, v := range c.Secret.Vaults {
		if v == "" || strings.Contains(v, "/") || c.Secret.Vault == "" {
			report(fmt.Errorf("secret.vaults[%d]: %q: a vault's name, beside secret.vault", i, v))
		}
	}
	for name, p := range c.Omp.Providers {
		// the message never carries the value someone put there
		if !opRef(p.APIKey) {
			report(fmt.Errorf("omp.providers.%s.apiKey: want op://<vault>/<item>/<field>, the reference of the provider's key: beekeeper hands an agent the key from the vault, never from a file", name))
		}
	}
	for i, r := range c.Outbound.StoreDeny {
		for _, g := range []string{r.Vault, r.Item} {
			if _, err := filepath.Match(g, ""); err != nil {
				report(fmt.Errorf("outbound.storeDeny[%d]: %q: %w", i, g, err))
			}
		}
	}
	for _, g := range c.Hooks.PersonFolders {
		if _, err := filepath.Match(g, ""); err != nil {
			report(fmt.Errorf("hooks.personFolders: %q: %w", g, err))
		}
	}
	for _, g := range c.Outbound.Paths {
		if _, err := filepath.Match(g, ""); err != nil {
			report(fmt.Errorf("outbound.paths: %q: %w", g, err))
		}
	}
	c.Sandbox.validateEach(report)
	if r := c.Reporter; r.Every.Duration != 0 && (r.Every.Duration < time.Minute || r.Brief == "") {
		report(fmt.Errorf("reporter: every %s needs at least a minute and a brief", r.Every.Duration))
	}
	if c.Feedback.Enabled() && !c.Reporter.Enabled() {
		report(fmt.Errorf("feedback.context: the feedback watch reads the reporter's posts: set reporter.every and reporter.brief"))
	}
	if tz := c.Reporter.TZ; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			report(fmt.Errorf("reporter.tz: %w", err))
		}
	}
	ownerReposEach("reporter.reviews", c.Reporter.Reviews, report)
	ownerReposEach("plans.repositories", c.Plans.Repositories, report)
	for repo, method := range c.Merge.Methods {
		ownerReposEach("merge.methods", []string{repo}, report)
		if method != "squash" && method != "rebase" && method != "merge" {
			report(fmt.Errorf("merge.methods[%s]: expected squash, rebase or merge", repo))
		}
	}
	for repo, timeout := range c.Merge.CITimeouts {
		ownerReposEach("merge.ciTimeouts", []string{repo}, report)
		if timeout.Duration <= 0 {
			report(fmt.Errorf("merge.ciTimeouts.%s: %s is not positive", repo, timeout.Duration))
		}
	}
	ownerReposEach("merge.admin", c.Merge.Admin, report)
	c.Sink.validateEach(report)
	for i, st := range c.Board.Order {
		if st.Name == "" {
			report(fmt.Errorf("board.order[%d]: no name", i))
		}
		if st.Search != "" && (len(st.Status)+len(st.Kind)+len(st.Labels) > 0 || st.SubIssues || st.Unblocked) {
			report(fmt.Errorf("board.order[%d] %q: a search step matches no board fields", i, st.Name))
		}
	}
	if b := c.Teleport.Bot; b.Enabled() {
		for name, value := range map[string]string{"config": b.Config, "certificate": b.Certificate, "kubeconfig": b.Kubeconfig} {
			if !filepath.IsAbs(value) {
				report(fmt.Errorf("teleport.bot.%s: an absolute path is required", name))
			}
		}
	}
	if t := c.Teleport; t.WarnBefore.Duration > t.RenewBefore.Duration {
		report(fmt.Errorf("teleport: warnBefore %s is past renewBefore %s: the watch would warn before the keeper renews", t.WarnBefore.Duration, t.RenewBefore.Duration))
	}
	if t := c.Kube.ContextTemplate; t != "" && !strings.Contains(t, "{installation}") {
		report(fmt.Errorf("kube.contextTemplate: %q has no {installation}", t))
	}
	if c.Supervisor.ReviewRounds < 0 {
		report(fmt.Errorf("supervisor.reviewRounds: %d is fewer than one review round (0 is the default, 1)", c.Supervisor.ReviewRounds))
	}
	if k := c.Supervisor.ClassifierConfidence; k < 0 || k > 1 {
		report(fmt.Errorf("supervisor.classifierConfidence: %v is not between 0 and 1", k))
	}
	c.Supervisor.Ownership.validateEach(func(err error) { report(fmt.Errorf("supervisor.ownership.%w", err)) })
	for _, selector := range c.Supervisor.Hosts {
		if selector == "" || selector == "local" {
			report(fmt.Errorf("supervisor.hosts: %q names no SSH execution host", selector))
		}
		if _, ok := c.Agents.Hosts[selector]; !ok {
			report(fmt.Errorf("supervisor.hosts: %q is no agents.hosts entry", selector))
		}
	}
	if r := c.Guide.Role; r.Skill != "" && r.Instructions != "" {
		report(errors.New("guide: set skill or instructions, not both"))
	}
	if pd := c.Alerts.PagerDuty; pd.Enabled() && len(pd.Services) == 0 {
		report(fmt.Errorf("alerts.pagerduty.services: reading PagerDuty through muster context %s needs the team's service ids", pd.Context))
	}
	if d := c.Alerts.Discover; d.Enabled() && c.Alerts.Team == "" {
		report(errors.New("alerts.discover: discovering the installations that carry the team's HelmReleases needs alerts.team"))
	}
	for _, p := range c.Alerts.Discover.Exclude {
		if _, err := path.Match(p, ""); err != nil {
			report(fmt.Errorf("alerts.discover.exclude: %q: %w", p, err))
		}
	}
	for i, in := range c.Alerts.Installations {
		if in.Name == "" {
			report(fmt.Errorf("alerts.installations[%d]: an installation needs a name", i))
		}
		if in.Floor != "" && !slices.Contains(alerts.Severities, in.Floor) {
			report(fmt.Errorf("alerts.installations[%d]: floor %q is none of %s", i, in.Floor, strings.Join(alerts.Severities, ", ")))
		}
	}
	if n := c.Alerts.Flap.Changes; n < 0 || n == 1 {
		report(fmt.Errorf("alerts.flap.changes: %d; an alert is flapping from its second change on at the earliest", n))
	}
	if p := c.Alerts.PageSeverity; p != "" && !slices.Contains(alerts.Severities, p) {
		report(fmt.Errorf("alerts.pageSeverity: %q is none of %s", p, strings.Join(alerts.Severities, ", ")))
	}
	if g := c.Alerts.OwnerGrace.Duration; g != 0 && g < time.Minute {
		report(fmt.Errorf("alerts.ownerGrace: %s; a page has at least a minute to be owned", g))
	}
	if e := c.Upgrades.Every.Duration; e < 0 {
		report(fmt.Errorf("upgrades.every: %s; an installation is read at most this long apart", e))
	}
	c.Notify.validateEach(report)
	for _, r := range c.Resources {
		if !validResourceName(r) {
			report(fmt.Errorf("resources: %q is not a valid resource name", r))
		}
	}
	if k := c.Capacity; k.Ceiling < 0 {
		report(fmt.Errorf("capacity.ceiling: %d is negative", k.Ceiling))
	}
	if n := c.Supervisor.GraphQLLimit; n < 0 {
		report(fmt.Errorf("supervisor.graphQLLimit: %d is negative", n))
	}
	if c.Capacity.ReviewHeadroom < 0 {
		report(fmt.Errorf("capacity.reviewHeadroom: %d is negative", c.Capacity.ReviewHeadroom))
	}
	if n, ok := c.Capacity.Local.Limit(); ok && n < 0 {
		report(fmt.Errorf("capacity.local.ceiling: %d is negative", n))
	}
	if c.Capacity.SwapPSIMax < 0 {
		report(fmt.Errorf("capacity.swapPSIMax: %g is not a pressure in percent", c.Capacity.SwapPSIMax))
	}
	c.Capacity.Usage.validateNamedEach("capacity.usage", report)
	for _, name := range c.Capacity.ProviderNames() {
		if strings.TrimSpace(name) == "" {
			report(errors.New("capacity.providers: empty provider name"))
		}
		p := c.Capacity.Providers[name]
		if k := strings.TrimSpace(p.Kind); k != "" && k != KindSubscription && k != KindSelfHosted {
			report(fmt.Errorf("capacity.providers.%s.kind: %q is not %s or %s", name, p.Kind, KindSubscription, KindSelfHosted))
		}
		if p.Ceiling < 0 {
			report(fmt.Errorf("capacity.providers.%s.ceiling: %d is negative", name, p.Ceiling))
		}
		if p.SelfHosted() {
			// claude, agy and codex are the subscription harnesses' names: a
			// self-hosted provider under one would be matched by a
			// subscription start's fallback and hand it its room.
			switch strings.ToLower(strings.TrimSpace(name)) {
			case ClaudeHarness, AgyHarness, CodexHarness:
				report(fmt.Errorf("capacity.providers.%s: %s names the %s subscription; a %s provider has another name", name, name, name, KindSelfHosted))
			}
			if p.Enabled() {
				report(fmt.Errorf("capacity.providers.%s: a %s provider reads no subscription usage: remove its command", name, KindSelfHosted))
			}
			if p.hasFields() {
				report(fmt.Errorf("capacity.providers.%s: a %s provider has no usage windows: remove its usage fields", name, KindSelfHosted))
			}
			if p.ServeModeStale.Duration < 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeStaleAfter: %s is negative", name, p.ServeModeStale.Duration))
			}
			if p.ServeModeSwitchTimeout.Duration < 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchTimeout: %s is negative", name, p.ServeModeSwitchTimeout.Duration))
			}
			for _, mode := range slices.Sorted(maps.Keys(p.ServeModeCeilings)) {
				if strings.TrimSpace(mode) == "" {
					report(fmt.Errorf("capacity.providers.%s.serveModeCeilings: empty mode name", name))
				}
				if p.ServeModeCeilings[mode] < 0 {
					report(fmt.Errorf("capacity.providers.%s.serveModeCeilings.%s: %d is negative", name, mode, p.ServeModeCeilings[mode]))
				}
			}
			if len(p.ServeModeCommand) > 0 {
				if strings.TrimSpace(p.ServeModeCommand[0]) == "" {
					report(fmt.Errorf("capacity.providers.%s.serveModeCommand[0]: empty executable", name))
				}
				if len(p.ServeModeCeilings) == 0 && len(p.ServeModeModes) == 0 {
					report(fmt.Errorf("capacity.providers.%s.serveModeCommand: no serveModeCeilings: the command's modes name no ceiling", name))
				}
			} else if len(p.ServeModeCeilings) > 0 || len(p.ServeModeModes) > 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeCeilings: no serveModeCommand: the ceilings name no mode reader", name))
			}
			if len(p.ServeModeModes) > 0 && len(p.ServeModeCommand) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeModes: no serveModeCommand: the modes name no mode reader", name))
			}
			if len(p.ServeModeSwitch) > 0 && len(p.ServeModeCommand) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchCommand: no serveModeCommand: the switch has no mode reader", name))
			}
			if len(p.ServeModeSwitch) > 0 && len(p.ServeModeModes) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchCommand: no serveModeModes: the switch knows no mode to switch into", name))
			}
			if len(p.ServeModeSwitch) > 0 && strings.TrimSpace(p.ServeModeSwitch[0]) == "" {
				report(fmt.Errorf("capacity.providers.%s.serveModeSwitchCommand[0]: empty executable", name))
			}
			if len(p.ServeModeModes) > 0 && len(p.ServeModeSwitch) == 0 {
				report(fmt.Errorf("capacity.providers.%s.serveModeModes: no serveModeSwitchCommand: the modes name no switch", name))
			}
			for _, mode := range slices.Sorted(maps.Keys(p.ServeModeModes)) {
				m := p.ServeModeModes[mode]
				if strings.TrimSpace(mode) == "" {
					report(fmt.Errorf("capacity.providers.%s.serveModeModes: empty mode name", name))
				}
				if m.Sessions < 0 {
					report(fmt.Errorf("capacity.providers.%s.serveModeModes.%s: sessions %d is negative", name, mode, m.Sessions))
				}
				if c, ok := p.ServeModeCeilings[mode]; ok && c != m.Sessions {
					report(fmt.Errorf("capacity.providers.%s.serveModeCeilings.%s: %d contradicts serveModeModes.%s.sessions %d", name, mode, c, mode, m.Sessions))
				}
				for _, n := range m.Nodes {
					if !validResourceName(n) {
						report(fmt.Errorf("capacity.providers.%s.serveModeModes.%s: %q is not a valid resource name", name, mode, n))
					}
				}
			}
			continue
		}
		if len(p.ServeModeCommand) > 0 || len(p.ServeModeCeilings) > 0 || len(p.ServeModeModes) > 0 ||
			len(p.ServeModeSwitch) > 0 || p.ServeModeSwitchTimeout.Duration != 0 {
			report(fmt.Errorf("capacity.providers.%s: serveModeCommand, serveModeCeilings, serveModeModes and serveModeSwitchCommand belong to a %s provider", name, KindSelfHosted))
		}
		p.validateNamedEach("capacity.providers."+name, report)
	}
	for i, ch := range c.Capacity.Choices {
		if !ValidEffort(strings.TrimSpace(ch.Effort)) || ch.Harness == AgyHarness && ch.Effort != "" {
			report(fmt.Errorf("capacity.choices[%d].effort: unsupported effort %q for %s", i, ch.Effort, ch.Harness))
		}
		if ch.Class < 0 || ch.Cost < 0 || ch.ReviewReserve < 0 || ch.ReviewReserve > 100 {
			report(fmt.Errorf("capacity.choices[%d]: class and cost must be nonnegative, reviewReserve in [0, 100]", i))
		}
		switch {
		case strings.TrimSpace(ch.Harness) == "":
			report(fmt.Errorf("capacity.choices[%d]: harness is required", i))
		case strings.TrimSpace(ch.Model) == "":
			report(fmt.Errorf("capacity.choices[%d]: model is required", i))
		case ch.fiveHourMaxSet && !(ch.FiveHourMax > 0 && ch.FiveHourMax <= 100):
			report(fmt.Errorf("capacity.choices[%d]: fiveHourMax %g is not in (0, 100]", i, ch.FiveHourMax))
		case ch.sevenDayMaxSet && !(ch.SevenDayMax > 0 && ch.SevenDayMax <= 100):
			report(fmt.Errorf("capacity.choices[%d]: sevenDayMax %g is not in (0, 100]", i, ch.SevenDayMax))
		case ch.paceMarginSet && (ch.PaceMargin < -100 || ch.PaceMargin > 100):
			report(fmt.Errorf("capacity.choices[%d]: paceMargin %g is not in [-100, 100]", i, ch.PaceMargin))
		}
		prov := ch.EffectiveProvider()
		if p, ok := c.Capacity.Providers[prov]; ok && p.SelfHosted() && !strings.EqualFold(strings.TrimSpace(ch.Harness), OmpHarness) {
			report(fmt.Errorf("capacity.choices[%d]: harness %q is a subscription; %s is a %s provider", i, ch.Harness, prov, KindSelfHosted))
		}
		if _, ok := c.Capacity.Providers[prov]; !ok {
			report(fmt.Errorf("capacity.choices[%d]: provider %q is not in capacity.providers", i, prov))
		}
	}
	if w := c.Watch.SwapWindow.Duration; w != 0 && w < MinSwapSpan {
		report(fmt.Errorf("watch.swapWindow: %s is under the %s a swap growth rate is measured over", w, MinSwapSpan))
	}
	if a := c.Agents; a.LastStepCeiling != 0 && a.LastStepCeiling <= a.RelayAt {
		report(fmt.Errorf("agents.lastStepCeiling: %d is not above relayAt %d", a.LastStepCeiling, a.RelayAt))
	}
	c.validateLabsEach(report)
	if o := c.Ollama; (o.URL != "" || c.Lemonade.URL != "") && (o.BudgetGiB < 1 || o.BudgetGiB > o.MaxBudgetGiB) {
		report(fmt.Errorf("ollama.budgetGiB: %d is not between 1 and maxBudgetGiB %d", o.BudgetGiB, o.MaxBudgetGiB))
	}
	checks := map[string]bool{}
	if len(c.Fatal.Checks) > 16 || len(c.Fatal.Repositories) > 16 {
		report(fmt.Errorf("fatal: at most 16 checks and 16 repositories"))
	}
	for i, check := range c.Fatal.Checks {
		if check.Name == "" || len(check.Probe) == 0 || check.Probe[0] == "" || checks[check.Name] {
			report(fmt.Errorf("fatal.checks[%d]: unique name and probe required", i))
		}
		if check.Harness != "" && !slices.Contains([]string{ClaudeHarness, OmpHarness, AgyHarness, CodexHarness}, check.Harness) {
			report(fmt.Errorf("fatal.checks[%d]: unknown harness", i))
		}
		checks[check.Name] = true
		if check.Repository != "" && !validFatalRepository(check.Repository) {
			report(fmt.Errorf("fatal.checks[%d]: invalid repository", i))
		}
	}
	for _, repo := range c.Fatal.Repositories {
		if !validFatalRepository(repo) {
			report(fmt.Errorf("fatal.repositories: expected owner/repository"))
		}
	}
	for repo, dir := range c.Fatal.Checkouts {
		if !validFatalRepository(repo) || dir == "" {
			report(fmt.Errorf("fatal.checkouts: valid repository and checkout required"))
		}
	}
	faults := map[string]bool{}
	for i, f := range c.Doctor.Faults {
		if f.Name == "" || f.Probe == "" || f.Remedy == "" || faults[f.Name] {
			report(fmt.Errorf("doctor.faults[%d]: a fault needs a name of its own, a probe and a remedy", i))
		}
		faults[f.Name] = true
	}
	names, repos := map[string]bool{}, map[string]string{}
	for i, l := range c.Lanes {
		if l.Name == "" || len(l.Repositories) == 0 || strings.Contains(l.Name, "/") {
			report(fmt.Errorf("lanes[%d]: a lane needs a name without a slash and repositories", i))
		}
		if names[l.Name] {
			report(fmt.Errorf("lanes: %q is named twice", l.Name))
		}
		names[l.Name] = true
		for _, r := range l.Repositories {
			if prev, ok := repos[r]; ok {
				report(fmt.Errorf("lanes: %s is in both %q and %q", r, prev, l.Name))
			}
			repos[r] = l.Name
		}
	}
}

func ownerReposEach(field string, repos []string, report func(error)) {
	for _, r := range repos {
		if o, name, ok := strings.Cut(r, "/"); !ok || o == "" || name == "" || strings.ContainsAny(name, "/ ") {
			report(fmt.Errorf("%s: %q is not owner/repo", field, r))
		}
	}
}
