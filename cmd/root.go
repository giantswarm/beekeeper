// Package cmd is beekeeper's command line.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Exit codes: 0 done, 1 error, 2 usage, 3 refused (a lease held or not
// granted, a hold set, the budget under its floor, a platform part this
// build does not have, a save by a release older than the one that wrote
// the state), 4 relieved (supervisor status in the session a
// relay relieved), 69 central unreachable (ExitCentral: a central verb
// whose instance did not answer), 78 vault (ExitVault: beekeeper secret
// could not read the shared vault: none configured, no token, op failing
// or silent), 125 outdated (a newer release exists: self-update --check, the status devctl's version check and
// muster's self-update --check use).
const (
	ExitError    = 1
	ExitUsage    = 2
	ExitRefused  = 3
	ExitRelieved = 4
	ExitVault    = 78
	ExitOutdated = 125
)

// exitError carries the process exit code of a failed command.
type exitError struct {
	code int
	msg  string
	err  error
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) Unwrap() error { return e.err }

// Code returns the exit code err asks for.
func Code(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	var stale *state.StaleWriterError
	if platform.Missing(err) || errors.As(err, &stale) {
		return ExitRefused
	}
	return ExitError
}

// newExitError is a failed command's error: its exit code, its message and
// what it wraps, if anything.
func newExitError(code int, msg string, err error) error {
	return &exitError{code: code, msg: msg, err: err}
}

func refused(format string, a ...any) error {
	return &exitError{code: ExitRefused, msg: fmt.Sprintf(format, a...)}
}

// configFlag names the configuration file.
const configFlag = "config"

func usageErr(format string, a ...any) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf(format, a...)}
}

// Main runs the command line and returns the process exit code. An error
// without a message (a wrapped command's exit code) prints nothing.
func Main() int {
	if len(os.Args) > len(secret.AskpassArgs) && slices.Equal(os.Args[1:1+len(secret.AskpassArgs)], secret.AskpassArgs) {
		// git's askpass helper: no configuration, nothing but the socket
		return secret.AskpassMain(os.Args[1+len(secret.AskpassArgs):], os.Stdout, os.Stderr)
	}
	defer cpuProfile(os.Getenv(cpuProfileEnv))()
	err := New().Execute()
	if err == nil {
		return 0
	}
	if msg := err.Error(); msg != "" {
		fmt.Fprintln(os.Stderr, project.Name+":", msg)
	}
	return Code(err)
}

type app struct {
	// queueReports routes session CLI reports through the supervisor inbox.
	// Embedded command adapters retain their synchronous transaction contract.
	queueReports       bool
	selectionKind      string
	selectionAuthor    string
	executionTransport func(execution.Host) execution.Transport
	executionProbe     func(context.Context, execution.Host) (execution.Capabilities, error)
	executionRuntime   func() (*execution.Server, error)
	executionUpdate    func(context.Context, execution.Host, string, []byte) (execution.HostUpdate, error)
	executionRequestID string
	cfgPath            string
	// loadedCfg is the configuration file loadConfig read; liveCapacity
	// reloads Capacity from cfgPath or loadedCfg without mutating cfg.
	loadedCfg string
	as        string
	json      bool
	// hook says a hook runs, for a session whose environment it keeps.
	hook bool

	cfg   *config.Config
	store state.Store
	ctx   context.Context
	now   time.Time
	// loopNow advances the long-running supervisor clock without mutating now
	// while actions from earlier passes still use the app.
	loopNow *atomic.Pointer[time.Time]
	out     io.Writer

	// kindClusters lists the running kind clusters; nil asks docker.
	kindClusters func() ([]string, error)
	// labDocker replaces Docker calls in lifecycle tests.
	labDocker func(context.Context, ...string) ([]byte, error)
	// labDirs maps the labs agentlab knows to their directories; nil asks
	// agentlab list.
	labDirs func() (map[string]string, error)
	// zone reads the machine's time zone; nil is machine.Zone.
	zone func() (*time.Location, error)
	// central is an app of beekeeper serve: its notes' filers run on other
	// machines and learn of an outcome from their person's mailbox.
	central bool
	// mailFiler queues a line for a note's filer in its person's mailbox,
	// addressed to the filing agent (a central app's tellFiler); nil tells
	// nobody.
	mailFiler func(ctx context.Context, n *state.Note, line string) error
	// centralClient calls the central instance (hub); nil until first used.
	centralClient *central.Client
	// guideDues are the guide's relay dues its guide watch said; nil in a
	// guide watch --once.
	guideDues relayDues
	// transcripts keeps a watch's transcript reads across its polls; nil
	// reads every time.
	transcripts *claude.TranscriptCache
	// origin reads a requester's working directory and the keys' variables
	// of its environment; nil is sandbox.Origin on /proc.
	origin func(pid int, keys []string) (string, []string, error)
	// ompModelList lists omp's model selectors (provider/id) in tests; nil:
	// omp models.
	ompModelList func(ctx context.Context) ([]string, error)
	// readUsage runs capacity.usage.command; nil is runUsageCommand.
	readUsage func(ctx context.Context, command []string, timeout time.Duration) (usageReading, error)
	// readServeMode runs a self-hosted provider's serveModeCommand; nil runs
	// the command itself.
	readServeMode func(ctx context.Context, command []string, stale time.Duration) (config.ServeModeReading, error)
	// serveModeEnv reaches the configured serving-mode reader and switcher.
	serveModeEnv []string
	// switchServeModeRun runs a self-hosted provider's
	// serveModeSwitchCommand; nil runs the command itself.
	switchServeModeRun func(ctx context.Context, command []string, mode string) (string, error)
	// serveModeSwitching names the node whose claim or release runs a
	// serving-mode switch right now: while that switch runs, the node
	// counts not held — its record stands for the switch's own party.
	serveModeSwitching string
	// serveModeSwitchHeld lets a test watch the machine's switch lock:
	// called while the lock stands, it reports what it sees.
	serveModeSwitchHeld func(name string)
	// serveModeSwitchingTo names, per provider, the mode a switch running
	// in this process runs into: status and the watch say it while the
	// command holds the machine's lock.
	serveModeSwitchingTo map[string]string
}

// requesterOrigin is a brokered request's working directory and the keys'
// variables of its environment (app.origin).
func (a *app) requesterOrigin(pid int, keys []string) (string, []string, error) {
	if a.origin != nil {
		return a.origin(pid, keys)
	}
	return sandbox.Origin("/proc", pid, keys)
}

func (a *app) context() context.Context {
	if a != nil && a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// New returns the root command.
func New() *cobra.Command {
	a := &app{out: os.Stdout}
	cobra.EnableCommandSorting = false
	root := &cobra.Command{
		Use:   project.Name,
		Short: "Keeps the Claude Code sessions sharing one machine working together",
		Long: `beekeeper keeps the Claude Code sessions sharing one machine working
together. It shows every running session and what it does, watches the
machine's memory, swap, disk and OOM kills, reads the GitHub budget all
sessions draw from, and holds the shared resources one session at a time:
kind labs, installations, the browser, merges into a repository.

Its state (the guide, grants, holds, registered agents, notes, timers,
session records) lives on disk and survives restarts: beekeeper handover
shows it, and a guide's successor starts from beekeeper guide handover
--prompt instead of a prose brief. The supervisor is the supervisor loop.

Exit codes: 0 done, 1 error, 2 usage, 3 refused, 4 relieved (guide
status after a relay), 69 the central instance unreachable (a central verb,
with central configured), 125 a newer release (self-update --check).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       project.VersionLine(),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.load(); err != nil {
				return err
			}
			cmd.SetContext(a.backgroundRESTContext(cmd.Context()))
			if err := a.checkExecutionCommand(cmd); err != nil {
				return err
			}
			// Admission must never wait for the first-run plugin sync (which
			// may fetch GitHub and take two minutes). The next ordinary command
			// still performs it.
			if cmd.Name() == fatalBudgetCommand {
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), firstRunTimeout)
			defer cancel()
			if syncsUnder(a.explicitConfig()) {
				syncOnce(ctx, cmd.ErrOrStderr(), a.cfg.StateDir)
			}
			return nil
		},
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&a.cfgPath, configFlag, "", "configuration file (default $XDG_CONFIG_HOME/beekeeper/config.yaml, or $BEEKEEPER_CONFIG)")
	pf.StringVar(&a.as, "as", "", "act as this person or script instead of the calling Claude Code session; agents park, idle and desktop act as the registered agent of this name")
	pf.BoolVar(&a.json, "json", false, "print JSON")
	pf.StringVar(&a.executionRequestID, "request-id", "", "idempotency key for a managed remote note, park or report retry")

	root.AddGroup(
		&cobra.Group{ID: "watching", Title: "Watching:"},
		&cobra.Group{ID: "sharing", Title: "Sharing:"},
		&cobra.Group{ID: supervisorGroup, Title: "Supervising:"},
		&cobra.Group{ID: "guarding", Title: "Guarding:"},
	)
	for _, c := range []*cobra.Command{a.statusCmd(), a.deliveryCmd(), a.simCmd(), a.capacityCmd(), a.sessionsCmd(), a.tailCmd(), a.transcriptsCmd(), a.snapshotCmd(), a.uiCmd(), a.onHost(a.watchCmd(), sandbox.OpWatch, 0), a.alertsCmd(), a.securityCmd(), a.budgetCmd(), a.spendCmd(), a.teleportCmd(), a.claudeLoginCmd(), a.psCmd()} {
		c.GroupID = "watching"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.leaseCmd(), a.holdCmd(), a.lanesCmd(), a.boardCmd(), a.browseCmd(), a.proofCmd(), a.onHost(a.personCmd(), sandbox.OpPerson, brokeredCallTimeout+time.Minute)} {
		c.GroupID = "sharing"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.supervisorCmd(), a.guideCmd(), a.emergencyCmd(), a.fatalCmd(), a.agentsCmd(), a.stopCmd(), a.resumeCmd(), a.doctorCmd(), a.worktreesCmd(), a.noteCmd(), a.answerCmd(), a.converseCmd(), a.timerCmd(), a.reporterCmd(), a.reportCmd(), a.handoverCmd(), a.logCmd(), a.lintCmd()} {
		c.GroupID = "supervising"
		root.AddCommand(c)
	}
	hook := a.hookCmd()
	hook.AddCommand(a.preInvocationCmd())
	for _, c := range []*cobra.Command{a.apiReadCmd(), a.runCmd(), hook, a.freeCmd(), a.cacheCmd(), a.diskCmd(), a.gateCmd(), a.scanCmd(), a.issuesCmd(), a.secretCmd(), a.sandboxCmd(), a.appCmd()} {
		c.GroupID = "guarding"
		root.AddCommand(c)
	}
	root.AddCommand(a.installCmd(), a.uninstallCmd(), a.selfUpdateCmd(), a.versionCmd(), a.configCmd(), a.mergeChildCmd(), a.followRunCmd(), a.squashMergeCmd(), a.centralCmd(), a.agyRunCmd(), a.codexRunCmd())
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: ExitUsage, msg: err.Error()}
	})
	// Runnable, so that an unknown command reaches the argument check below
	// instead of printing the help with exit 0.
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	usageArgs(root)
	return root
}

// usageArgs makes every command's argument check fail with the usage exit
// code: cobra returns those errors untyped.
func usageArgs(c *cobra.Command) {
	if check := c.Args; check != nil {
		c.Args = func(cmd *cobra.Command, args []string) error {
			if err := check(cmd, args); err != nil {
				return &exitError{code: ExitUsage, msg: err.Error()}
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		usageArgs(sub)
	}
}

func (a *app) load() error {
	err := a.loadConfig()
	if err != nil {
		return err
	}
	a.store, err = state.Open(a.cfg.StateDir)
	if err != nil {
		return err
	}
	a.now = clk.Now()
	a.queueReports = true
	a.observeREST()
	return nil
}

// caller is who runs this command: the Claude Code session it runs in, or
// the --as name, with the configured person, team and host.
func (a *app) caller() (state.Party, error) {
	if a.managedExecutionID() != "" {
		_, j, err := a.executionCaller()
		if err != nil {
			return state.Party{}, err
		}
		return state.Party{Session: j.Handle.ConversationID, Name: j.Request.Name, Host: string(j.Handle.Ref.Host), Person: a.cfg.Identity.Person, Team: a.cfg.Identity.Team}, nil
	}
	p, err := a.callerSession()
	p.Person, p.Team, p.Host = a.cfg.Identity.Person, a.cfg.Identity.Team, a.cfg.Identity.Host
	return p, err
}

// callerSession is the session or --as name that runs this command. The
// session's name is its desktop title, else the name in the environment,
// else the name its CLI's record holds (a `claude --bg` worker's tool
// commands inherit neither), else its id.
func (a *app) callerSession() (state.Party, error) {
	if a.as != "" {
		return state.Party{Name: a.as}, nil
	}
	p := envParty(os.Getenv)
	// Tool shells may retain only CODEX_THREAD_ID. Resolve it through the
	// runner's recorded link rather than treating the command as a person.
	if p.Session == "" && p.HostSession == "" && os.Getenv("CODEX_THREAD_ID") != "" {
		thread := os.Getenv("CODEX_THREAD_ID")
		inbox := codex.ResolveInbox(a.cfg.StateDir, thread)
		if inbox != "" && codex.ReadThread(a.cfg.StateDir, inbox) == thread {
			p.HostSession = codex.HostPrefix + strings.TrimSuffix(filepath.Base(inbox), ".in")
			p.Session = thread
		}
	}
	if id, ok := strings.CutPrefix(p.HostSession, codex.HostPrefix); ok {
		p.Session = codex.ReadThread(a.cfg.StateDir, codex.InboxPath(a.cfg.StateDir, id))
		if p.Name == "" {
			p.Name = p.HostSession
		}
		return p, nil
	}
	if os.Getenv(omp.EnvAgent) != "" || os.Getenv(agy.EnvAgent) != "" || os.Getenv(codex.EnvAgent) != "" {
		return p, nil
	}
	if p.Session == "" {
		return p, &exitError{code: ExitUsage, msg: "not inside a Claude Code session: pass --as <name>"}
	}
	a.discardInheritedHost(&p)
	if p.HostSession != "" {
		if r, ok := claude.ReadRecord(a.cfg, p.HostSession); ok && r.Title != "" {
			p.Name = r.Title
		}
	}
	var t *proc.Table
	if p.Name == "" {
		if tbl, err := plat.Machine.Processes(); err == nil {
			t = tbl
			p.Name = claude.RecordName(a.cfg, t, p.Session)
		}
	}
	a.rebindCallerSession(t, &p)
	if p.Name == "" {
		p.Name = p.Session
	}
	return p, nil
}

// A resumed background CLI may inherit the desktop caller's host id and
// title. Never use those to rename it or rebind the caller's state to it.
// A desktop restart still keeps its host when that record names this CLI.
func (a *app) discardInheritedHost(p *state.Party) {
	if p.HostSession == "" {
		return
	}
	if r, ok := claude.ReadRecord(a.cfg, p.HostSession); ok && r.CLISessionID != "" {
		if r.CLISessionID != p.Session && !slices.Contains(r.PriorCLISessionIDs, p.Session) {
			p.HostSession, p.Name = "", ""
		}
		return
	}
	if a.store == nil {
		return
	}
	st, err := a.store.Peek()
	if err != nil {
		return
	}
	// A known CLI's recorded identity is stronger than its inherited shell.
	for _, s := range st.Starts {
		if s.Session == p.Session && s.HostSession != p.HostSession {
			p.HostSession, p.Name = s.HostSession, s.Name
			return
		}
	}
}

// rebindCallerSession keeps a resumed session's recorded role and start bound
// when its CLI came back under a new session id, and keeps its numbered run
// title when the resumed CLI derived another.
func (a *app) rebindCallerSession(t *proc.Table, p *state.Party) {
	if a.store == nil || p.Session == "" {
		return
	}
	st, err := a.store.Peek()
	if err != nil {
		return
	}
	var prevID, jobID string
	if !hasStateSession(st, p.Session) && hasAnyStateSession(st) && hasCLISessionRecords(a.cfg) {
		if t == nil {
			t, _ = plat.Machine.Processes()
		}
		if t != nil {
			prevID, jobID = claude.ResumedSession(a.cfg, t, p.Session)
		}
	}
	var desktopInitID string
	var priorIDs []string
	if p.HostSession != "" {
		desktopInitID, _ = strings.CutPrefix(p.HostSession, "local_")
		if r, ok := claude.ReadRecord(a.cfg, p.HostSession); ok {
			priorIDs = r.PriorCLISessionIDs
		}
	}
	oldIDs := resumedStateSessions(st, p.Session, prevID, jobID, desktopInitID, priorIDs)
	needsHost := p.HostSession != "" && slices.ContainsFunc(roles, func(rl role) bool {
		h := rl.get(st).Holder
		return h != nil && h.Session == p.Session && h.HostSession == ""
	})
	if len(oldIDs) > 0 || needsHost {
		_ = a.store.UpdateWithin(100*time.Millisecond, func(ust *state.State) ([]state.Event, error) {
			for _, oldID := range oldIDs {
				rebindSessionState(ust, oldID, p.Session, p.HostSession)
			}
			if needsHost {
				rebindSessionState(ust, p.Session, p.Session, p.HostSession)
			}
			st = ust
			return nil, nil
		})
	}
	for _, rl := range roles {
		if h := rl.get(st).Holder; h != nil && h.Is(*p) && rl.runOf(h.Name) > 0 && rl.runOf(p.Name) == 0 {
			p.Name = h.Name
		} else if s, ok := st.BypassStartOf(*p); ok && s.Role == rl.name && rl.runOf(s.Name) > 0 && rl.runOf(p.Name) == 0 {
			p.Name = s.Name
		}
	}
}

func hasStateSession(st *state.State, id string) bool {
	if id == "" {
		return false
	}
	for _, s := range st.Starts {
		if s.Session == id {
			return true
		}
	}
	for _, ag := range st.Agents {
		if ag.Session == id {
			return true
		}
	}
	for _, rl := range roles {
		r := rl.get(st)
		if r.Holder != nil && r.Holder.Session == id {
			return true
		}
		if r.Relay != nil && (r.Relay.From.Session == id || r.Relay.To.Session == id) {
			return true
		}
		for _, rf := range r.Relieved {
			if rf.Party.Session == id {
				return true
			}
		}
	}
	return false
}

func hasAnyStateSession(st *state.State) bool {
	for _, s := range st.Starts {
		if s.Session != "" {
			return true
		}
	}
	for _, ag := range st.Agents {
		if ag.Session != "" {
			return true
		}
	}
	for _, rl := range roles {
		r := rl.get(st)
		if r.Holder != nil && r.Holder.Session != "" {
			return true
		}
		if r.Relay != nil && (r.Relay.From.Session != "" || r.Relay.To.Session != "") {
			return true
		}
		for _, rf := range r.Relieved {
			if rf.Party.Session != "" {
				return true
			}
		}
	}
	return false
}

func hasCLISessionRecords(cfg *config.Config) bool {
	if cfg == nil || cfg.Claude.SessionsDir == "" {
		return false
	}
	entries, err := os.ReadDir(cfg.Claude.SessionsDir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return !e.IsDir() && strings.HasSuffix(e.Name(), ".json")
	})
}

func resumedStateSessions(st *state.State, curID, prevID, jobID, desktopInitID string, priorIDs []string) []string {
	match := func(id string) bool {
		if id == "" || id == curID {
			return false
		}
		if prevID != "" && id == prevID {
			return true
		}
		if jobID != "" && len(id) >= 8 && strings.HasPrefix(id, jobID) {
			return true
		}
		return desktopInitID != "" && (id == desktopInitID || slices.Contains(priorIDs, id))
	}
	var out []string
	add := func(id string) {
		if match(id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, s := range st.Starts {
		add(s.Session)
	}
	for _, ag := range st.Agents {
		add(ag.Session)
	}
	for _, rl := range roles {
		r := rl.get(st)
		if r.Holder != nil {
			add(r.Holder.Session)
		}
		if r.Relay != nil {
			add(r.Relay.From.Session)
			add(r.Relay.To.Session)
		}
		for _, rf := range r.Relieved {
			add(rf.Party.Session)
		}
	}
	return out
}

// envParty is the session an environment names, read with get: an omp, agy or
// codex agent beekeeper started is its roster entry, whatever session
// variables its tool shell carries; else the Claude Code session's ids and name.
func envParty(get func(string) string) state.Party {
	if id := get(omp.EnvAgent); id != "" {
		return state.Party{HostSession: omp.HostPrefix + id, Name: get(omp.EnvName)}
	}
	if id := get(agy.EnvAgent); id != "" {
		return state.Party{HostSession: agy.HostPrefix + id, Name: get(agy.EnvName)}
	}
	if id := get(codex.EnvAgent); id != "" {
		return state.Party{HostSession: codex.HostPrefix + id, Name: get(codex.EnvName)}
	}
	return state.Party{
		Session:     get("CLAUDE_CODE_SESSION_ID"),
		HostSession: get("CLAUDE_CODE_HOST_SESSION_ID"),
		Name:        get("CLAUDE_CODE_SESSION_NAME"),
	}
}

// sessions reads the process table and the running sessions.
func (a *app) sessions() ([]*claude.Session, *proc.Table, error) {
	t, err := plat.Machine.Processes()
	if err != nil {
		return nil, nil, err
	}
	return discover(a.cfg, t, a.timeNow()), t, nil
}

// discover returns the running sessions of every harness.
func discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session {
	var out []*claude.Session
	for _, h := range harnesses {
		out = append(out, h.Discover(cfg, t, now)...)
	}
	return out
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
}

// ago renders how long ago t was: "4s", "12m", "3h05m", "2d".
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return dur(now.Sub(t))
}

func dur(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + dur(-d)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// clock renders t as local "15:04", with the date when it is not today.
func clock(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	t = t.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// clockZone is clock with the local zone's name: "08:30 CET". A timer's
// due time and the time it fired or started are read side by side, so
// both carry the zone they are in.
func clockZone(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return clock(now, t) + " " + t.Local().Format("MST")
}

// untilTime parses "15:30" (the next such local time) or a duration ("2h").
func untilTime(now time.Time, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d), nil
	}
	for _, layout := range []string{"15:04", "2006-01-02 15:04", time.RFC3339} {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err != nil {
			continue
		}
		if layout == "15:04" {
			t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
			if !t.After(now) {
				t = t.Add(24 * time.Hour)
			}
		}
		return t, nil
	}
	return time.Time{}, &exitError{code: ExitUsage, msg: fmt.Sprintf("%q is neither a time (15:30, 2006-01-02 15:04) nor a duration (2h)", s)}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// withOwner is s followed by whose agent p is, of which team, on which
// host, when the state knows it.
func withOwner(s string, p state.Party) string {
	if o := p.Owner(); o != "" {
		return s + " (" + o + ")"
	}
	return s
}

// quotedOwner is p's quoted name with whose agent it is, as withOwner.
func quotedOwner(p state.Party) string { return withOwner(fmt.Sprintf("%q", p.Name), p) }

func event(by state.Party, verb, format string, a ...any) state.Event {
	return state.Event{At: clk.Now().UTC(), By: by, Verb: verb, Detail: fmt.Sprintf(format, a...)}
}

// about is e with the issue or pull request ref names (owner/repo#n or its
// URL) as its Subject, which the event sink dispatches; a ref that names
// none leaves e as it is.
func about(e state.Event, ref string) state.Event {
	if r, ok := parseRef(ref); ok {
		e.Subject = refName(r)
	}
	return e
}

// listCmd is the `list` subcommand of a noun, which the noun alone runs too.
func listCmd(short string, run func() error) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: short,
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return run() },
	}
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintln(a.out, project.Name, project.VersionLine())
			return err
		},
	}
}

// timeNow returns the supervisor clock, or the command's fixed observation time.
func (a *app) timeNow() time.Time {
	if a.loopNow != nil {
		if now := a.loopNow.Load(); now != nil {
			return *now
		}
	}
	return a.now
}

// snapshotAtNow gives helpers that use a command's fixed time an observation
// that stays stable while subsequent supervisor passes advance their clock.
func (a *app) snapshotAtNow() *app {
	snapshot := *a
	snapshot.now = a.timeNow()
	snapshot.loopNow = nil
	return &snapshot
}
