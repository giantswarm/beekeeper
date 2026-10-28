package cmd

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Harness is a coding agent harness beekeeper starts, lists, follows, wakes,
// stops and prices.
type Harness interface {
	// Name is the harness's short name ("" for Claude Code, "omp", "agy", "codex").
	Name() string
	// HostPrefix prefixes a started session's HostSession ("local_", "omp_", "agy_", "codex_").
	HostPrefix() string
	// EnvAgent is the environment variable a started session's shell carries, "" for Claude Code.
	EnvAgent() string
	// Start starts sp on this harness.
	Start(ctx context.Context, a *app, sp agentStart) error
	// DryRun prints how Start would start sp without starting anything.
	DryRun(ctx context.Context, a *app, sp agentStart, gaps []string) error
	// Wake delivers msg from by to the registered agent ag started under id.
	Wake(a *app, by state.Party, ag state.Agent, id, msg string) error
	// Send writes msg to the inbox of the agent name started under id.
	Send(a *app, name, id, msg string) error
	// Stop stops the unit and removes the inbox of the agent started under id.
	Stop(ctx context.Context, a *app, id string) string
	// Discover lists the running sessions of this harness.
	Discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session
	// Ended lists the sessions of this harness active since that no process runs.
	Ended(cfg *config.Config, running []*claude.Session, since time.Time) []*claude.Session
	// Tail reads the last n conversation turns of a transcript.
	Tail(path string, n int) ([]claude.Turn, error)
	// Follow reads the last n turns of a transcript, including tool calls.
	Follow(path string, n int) ([]claude.Turn, error)
	// ReadTranscript reads the work and activity of a session's transcript window.
	ReadTranscript(a *app, s *claude.Session, now time.Time) (claude.Work, claude.Activity)
	// SpendTranscripts reads whole-transcript API requests in [from, to) for spend.
	SpendTranscripts(cfg *config.Config, from, to time.Time) ([]claude.SpendTranscript, error)
}

// harnesses are the coding agent harnesses beekeeper supports, in discovery order.
var harnesses = []Harness{
	claudeHarness{},
	ompHarness{},
	agyHarness{},
	codexHarness{},
}

// harnessByName returns the harness for a --harness flag value.
func harnessByName(name string) (Harness, bool) {
	switch strings.TrimSpace(name) {
	case "", claudeComm:
		return claudeHarness{}, true
	case omp.Harness:
		return ompHarness{}, true
	case agy.Harness:
		return agyHarness{}, true
	case codex.Harness:
		return codexHarness{}, true
	}
	return nil, false
}

// harnessForSession returns the harness of s.
func harnessForSession(s *claude.Session) Harness {
	if s != nil {
		if h, ok := harnessByName(s.Harness); ok {
			return h
		}
	}
	return claudeHarness{}
}

// harnessForHost returns the non-Claude harness and start id for a HostSession
// ("omp_<id>", "agy_<id>").
func harnessForHost(hostSession string) (Harness, string, bool) {
	for _, h := range harnesses {
		if h.Name() == "" {
			continue
		}
		if id, ok := strings.CutPrefix(hostSession, h.HostPrefix()); ok {
			return h, id, true
		}
	}
	return nil, "", false
}

type claudeHarness struct{}

func (claudeHarness) Name() string       { return "" }
func (claudeHarness) HostPrefix() string { return "local_" }
func (claudeHarness) EnvAgent() string   { return "" }

func (claudeHarness) Start(ctx context.Context, a *app, sp agentStart) error {
	sa, err := a.startAgent(ctx, sp)
	if err != nil {
		return err
	}
	if sa.background {
		_, err = fmt.Fprintf(a.out, "started %s: background session %s with Remote Control, in %s, busy with %q\n%s\nits turns run on %s\n",
			sp.name, sa.id, sa.dir, sa.task, sa.turn, cmp.Or(sa.want, "Claude Code's default model"))
		return err
	}
	_, err = fmt.Fprintf(a.out, "started %s: session %s, desktop local_%s, in %s, busy with %q\n%s\n",
		sp.name, sa.id, sa.id, sa.dir, sa.task, sa.turn)
	if err == nil && sa.kept != "" {
		_, err = fmt.Fprintf(a.out, "the desktop still shows %s\n", sa.kept)
	}
	if err == nil && sa.restored != "" {
		_, err = fmt.Fprintln(a.out, sa.restored)
	}
	if err == nil {
		_, err = fmt.Fprintln(a.out, titleLine(sp.name, sa.title))
	}
	if err == nil {
		_, err = fmt.Fprintln(a.out, modelLine(sa.model, sa.want, "local_"+sa.id))
	}
	if err == nil {
		_, err = fmt.Fprintln(a.out, browserLine(sa.chrome))
	}
	return err
}

func (claudeHarness) DryRun(ctx context.Context, a *app, sp agentStart, gaps []string) error {
	return a.dryRunStart(ctx, sp, gaps)
}

func (claudeHarness) Wake(*app, state.Party, state.Agent, string, string) error {
	return nil
}

func (claudeHarness) Send(*app, string, string, string) error {
	return nil
}

func (claudeHarness) Stop(context.Context, *app, string) string {
	return ""
}

func (claudeHarness) Discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session {
	return claude.Discover(cfg, t, now)
}

func (claudeHarness) Ended(*config.Config, []*claude.Session, time.Time) []*claude.Session {
	return nil
}

func (claudeHarness) Tail(path string, n int) ([]claude.Turn, error) {
	return claude.Tail(path, n)
}

func (claudeHarness) Follow(path string, n int) ([]claude.Turn, error) {
	return claude.Follow(path, n)
}

func (claudeHarness) ReadTranscript(a *app, s *claude.Session, _ time.Time) (claude.Work, claude.Activity) {
	work, act := a.readTranscript(s.Transcript)
	act.Price(a.cfg.Metrics)
	return work, act
}

func (claudeHarness) SpendTranscripts(cfg *config.Config, from, to time.Time) ([]claude.SpendTranscript, error) {
	return claude.ReadSpendTranscripts(cfg.Claude.ProjectsDir, from, to)
}

type ompHarness struct{}

func (ompHarness) Name() string       { return omp.Harness }
func (ompHarness) HostPrefix() string { return omp.HostPrefix }
func (ompHarness) EnvAgent() string   { return omp.EnvAgent }

func (ompHarness) Start(ctx context.Context, a *app, sp agentStart) error {
	return a.startOmpAgent(ctx, sp)
}

func (ompHarness) DryRun(_ context.Context, a *app, sp agentStart, gaps []string) error {
	return a.dryRunOmpStart(sp, gaps)
}

func (ompHarness) Wake(a *app, by state.Party, ag state.Agent, id, msg string) error {
	return a.wakeOmp(by, ag, id, msg)
}

func (ompHarness) Send(a *app, name, id, msg string) error {
	return a.sendOmp(name, id, msg)
}

func (ompHarness) Stop(ctx context.Context, a *app, id string) string {
	return a.endOmp(ctx, id)
}

func (ompHarness) Discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session {
	return omp.Discover(cfg.Omp.SessionsDir, t, now)
}

func (ompHarness) Ended(cfg *config.Config, running []*claude.Session, since time.Time) []*claude.Session {
	return omp.Ended(cfg.Omp.SessionsDir, running, since)
}

func (ompHarness) Tail(path string, n int) ([]claude.Turn, error) {
	return omp.Tail(path, n)
}

func (ompHarness) Follow(path string, n int) ([]claude.Turn, error) {
	return omp.Follow(path, n)
}

func (ompHarness) ReadTranscript(_ *app, s *claude.Session, now time.Time) (claude.Work, claude.Activity) {
	// omp prices its replies itself.
	return omp.ReadTranscript(s.Transcript, now)
}

func (ompHarness) SpendTranscripts(cfg *config.Config, from, to time.Time) ([]claude.SpendTranscript, error) {
	return omp.SpendTranscripts(cfg.Omp.SessionsDir, from, to), nil
}

type agyHarness struct{}

func (agyHarness) Name() string       { return agy.Harness }
func (agyHarness) HostPrefix() string { return agy.HostPrefix }
func (agyHarness) EnvAgent() string   { return agy.EnvAgent }

func (agyHarness) Start(ctx context.Context, a *app, sp agentStart) error {
	return a.startAgyAgent(ctx, sp)
}

func (agyHarness) DryRun(_ context.Context, a *app, sp agentStart, gaps []string) error {
	return a.dryRunAgyStart(sp, gaps)
}

func (agyHarness) Wake(a *app, by state.Party, ag state.Agent, id, msg string) error {
	return a.wakeAgy(by, ag, id, msg)
}

func (agyHarness) Send(a *app, name, id, msg string) error {
	return a.sendAgy(name, id, msg)
}

func (agyHarness) Stop(ctx context.Context, a *app, id string) string {
	return a.endAgy(ctx, id)
}

func (agyHarness) Discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session {
	return agy.Discover(cfg.Agy.BrainDir, cfg.StateDir, t, now)
}

func (agyHarness) Ended(cfg *config.Config, running []*claude.Session, since time.Time) []*claude.Session {
	return agy.Ended(cfg.Agy.BrainDir, running, since)
}

func (agyHarness) Tail(path string, n int) ([]claude.Turn, error) {
	return agy.Tail(path, n)
}

func (agyHarness) Follow(path string, n int) ([]claude.Turn, error) {
	return agy.Follow(path, n)
}

func (agyHarness) ReadTranscript(a *app, s *claude.Session, now time.Time) (claude.Work, claude.Activity) {
	work, act := agy.ReadTranscript(s.Transcript, now, s.Cwd)
	act.Price(a.cfg.Metrics)
	return work, act
}

func (agyHarness) SpendTranscripts(cfg *config.Config, from, to time.Time) ([]claude.SpendTranscript, error) {
	return agy.SpendTranscripts(cfg.Agy.BrainDir, from, to), nil
}

type codexHarness struct{}

func (codexHarness) Name() string       { return codex.Harness }
func (codexHarness) HostPrefix() string { return codex.HostPrefix }
func (codexHarness) EnvAgent() string   { return codex.EnvAgent }

func (codexHarness) Start(ctx context.Context, a *app, sp agentStart) error {
	return a.startCodexAgent(ctx, sp)
}

func (codexHarness) DryRun(_ context.Context, a *app, sp agentStart, gaps []string) error {
	return a.dryRunCodexStart(sp, gaps)
}

func (codexHarness) Wake(a *app, by state.Party, ag state.Agent, id, msg string) error {
	return a.wakeCodex(by, ag, id, msg)
}

func (codexHarness) Send(a *app, name, id, msg string) error {
	return a.sendCodex(name, id, msg)
}

func (codexHarness) Stop(ctx context.Context, a *app, id string) string {
	return a.endCodex(ctx, id)
}

func (codexHarness) Discover(cfg *config.Config, t *proc.Table, now time.Time) []*claude.Session {
	return codex.Discover(cfg.StateDir, cfg.Codex.SessionsDir, t, now)
}

func (codexHarness) Ended(cfg *config.Config, running []*claude.Session, since time.Time) []*claude.Session {
	return codex.Ended(cfg.StateDir, cfg.Codex.SessionsDir, running, since)
}

func (codexHarness) Tail(path string, n int) ([]claude.Turn, error) {
	return codex.Tail(path, n)
}

func (codexHarness) Follow(path string, n int) ([]claude.Turn, error) {
	return codex.Follow(path, n)
}

func (codexHarness) ReadTranscript(a *app, s *claude.Session, now time.Time) (claude.Work, claude.Activity) {
	work, act := codex.ReadTranscript(s.Transcript, now, s.Model)
	act.Price(a.cfg.Metrics)
	return work, act
}

func (codexHarness) SpendTranscripts(cfg *config.Config, from, to time.Time) ([]claude.SpendTranscript, error) {
	return codex.SpendTranscripts(cfg.StateDir, cfg.Codex.SessionsDir, from, to), nil
}
